package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/server"
	"pimg/internal/store"
)

// Real TLS connections to fixture TG/S3/CDN endpoints must all use an
// authenticated CONNECT proxy. No target DNS or external service is contacted.
func TestStorageClientsUseProxyForTelegramR2AndCDN(t *testing.T) {
	payload := bytes.Repeat([]byte("fixture-ciphertext"), 2048)
	var mu sync.Mutex
	requests := map[string]int{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.Method+" "+r.Host+r.URL.Path]++
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"), strings.HasSuffix(r.URL.Path, "/getChat"):
			io.WriteString(w, `{"ok":true,"result":{"id":1,"type":"channel"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendDocument"):
			file, _, err := r.FormFile("document")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			got, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(got, payload) {
				t.Error("TG upload changed payload", err)
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"document":{"file_id":"fixture","file_size":%d}}}`, len(payload))
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			io.WriteString(w, `{"ok":true,"result":{"file_path":"fixture.bin"}}`)
		case strings.HasPrefix(r.URL.Path, "/file/bot"):
			w.Write(payload)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			io.WriteString(w, `{"ok":true,"result":true}`)
		case r.Host == "api.cloudflare.com":
			if r.Header.Get("Authorization") != "Bearer fixture-cdn-token" {
				t.Error("CDN token missing")
			}
			io.WriteString(w, `{"success":true}`)
		case r.URL.Path == "/temp/fixture.bin":
			switch r.Method {
			case "PUT":
				got, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(got, payload) {
					t.Error("R2 upload changed payload", err)
				}
				w.Header().Set("ETag", `"fixture"`)
			case "GET", "HEAD":
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				if r.Method == "GET" {
					w.Write(payload)
				}
			case "DELETE":
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(400)
			}
		default:
			t.Error("unexpected fixture request", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(origin.Close)

	// Trust only the fixture's certificate. ServerName matches its loopback SAN;
	// CONNECT targets remain distinct so proxy routing is checked independently.
	previous := http.DefaultTransport
	base := previous.(*http.Transport).Clone()
	base.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	base.TLSClientConfig.ServerName = "127.0.0.1"
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = previous; base.CloseIdleConnections() })
	t.Setenv("NO_PROXY", "*")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	var connections []net.Conn
	connects := map[string]int{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-password")) {
			t.Error("storage proxy CONNECT/authentication missing")
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		mu.Lock()
		connects[r.Host]++
		connections = append(connections, client)
		mu.Unlock()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		go func() { io.Copy(upstream, rw) }()
		io.Copy(client, upstream)
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range connections {
			c.Close()
		}
	})
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("fixture-user", "fixture-password")
	cfg := &config.Config{
		Storage:  config.Storage{ProxyURL: proxyURL.String()},
		DevMock:  true, // Explicit mock transport; production uses the MTProto client.
		Telegram: config.Telegram{APIBase: "https://telegram.example.invalid", BotToken: "fixture-token", ChannelID: "fixture-channel"},
		R2: config.R2{Endpoint: "https://r2.example.invalid", AccessKeyID: "fixture", SecretAccessKey: "fixture",
			TempBucket: "temp", PermBucket: "perm", PublicBaseURL: "https://public.example.invalid",
			CDNZoneID: "fixture-zone", CDNAPIToken: "fixture-cdn-token"},
	}
	tg, r2, err := storageClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tg.TestConnectivity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tg.SendDocument(ctx, "fixture.bin", payload, ""); err != nil {
		t.Fatal(err)
	}
	rc, err := tg.Download(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	verifyProxyPayload(t, rc, payload)
	if err := tg.DeleteMessage(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := r2.PutObject(ctx, "temp", "fixture.bin", bytes.NewReader(payload), int64(len(payload)), "application/octet-stream", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := r2.HeadSize(ctx, "temp", "fixture.bin"); err != nil || n != int64(len(payload)) {
		t.Fatal("R2 HEAD", n, err)
	}
	rc, err = r2.Get(ctx, "temp", "fixture.bin")
	if err != nil {
		t.Fatal(err)
	}
	verifyProxyPayload(t, rc, payload)
	if err := r2.Delete(ctx, "temp", "fixture.bin"); err != nil {
		t.Fatal(err)
	}
	if err := r2.PurgeCDN(ctx, []string{r2.PublicURL("fixture.bin")}); err != nil {
		t.Fatal(err)
	}
	if r2.PublicURL("fixture.bin") != "https://public.example.invalid/fixture.bin" {
		t.Fatal("proxy changed the visitor's public URL")
	}
	signed, err := r2.PresignGet(ctx, "temp", "fixture.bin", time.Minute, "")
	if err != nil || !strings.HasPrefix(signed, "https://r2.example.invalid/temp/fixture.bin?") {
		t.Fatal("proxy changed the visitor's presigned URL", err)
	}
	if http.DefaultTransport != base {
		t.Fatal("storage proxy modified the process-wide transport")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, target := range []string{"telegram.example.invalid:443", "r2.example.invalid:443", "api.cloudflare.com:443"} {
		if connects[target] == 0 {
			t.Error("missing storage proxy tunnel", target)
		}
	}
	if len(requests) != 11 {
		encoded, _ := json.Marshal(requests)
		t.Errorf("expected 11 TG/S3/CDN operations, got %s", encoded)
	}
}

func verifyProxyPayload(t *testing.T, rc io.ReadCloser, expected []byte) {
	t.Helper()
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || sha256.Sum256(got) != sha256.Sum256(expected) {
		t.Fatal("payload hash changed through storage proxy", err)
	}
}

func TestVisitorsConnectDirectWithUnavailableStorageProxy(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Storage:  config.Storage{Cold: "telegram", Hot: "r2", ProxyURL: "http://127.0.0.1:1"},
		Paths:    config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")},
		Telegram: config.Telegram{APIBase: "https://api.telegram.org", BotToken: "fixture-token", ChannelID: "fixture-channel"},
		R2: config.R2{Endpoint: "https://r2.example.invalid", AccessKeyID: "fixture", SecretAccessKey: "fixture",
			TempBucket: "temp", PermBucket: "perm", PublicBaseURL: "https://public.example.invalid"},
	}
	tg, r2, err := storageClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	master, err := cryptox.NewMaster(cryptox.GenerateMasterKey())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := store.New(cfg, db, master, tg, r2, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, db, svc, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	t.Cleanup(web.Close)
	directTransport := &http.Transport{Proxy: nil}
	t.Cleanup(directTransport.CloseIdleConnections)
	visitor := &http.Client{Transport: directTransport, Timeout: time.Second}
	for _, path := range []string{"/healthz", "/api/site", "/api/home"} {
		resp, err := visitor.Get(web.URL + path)
		if err != nil {
			t.Fatal("direct visitor could not reach pimg", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatal("visitor request unexpectedly depends on storage proxy", path, resp.StatusCode)
		}
	}
}
