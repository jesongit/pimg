package storagehttp

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http/httpproxy"
	"pimg/internal/proxytraffic"
)

func TestMeteredHTTPProxyCountsBothDirectionsAndRecovers(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("reply:"), b...))
	}))
	defer proxy.Close()
	m, err := proxytraffic.New(proxytraffic.DefaultState(), nil)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewMeteredTransport(proxy.URL, m)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for range 2 {
		resp, err := client.Post("http://synthetic.invalid/item", "text/plain", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(b) != "reply:payload" {
			t.Fatal(string(b), err)
		}
	}
	v := m.View()
	if v.DailyUsed <= int64(2*len("reply:payloadpayload")) || v.DailyUsed != v.MonthlyUsed {
		t.Fatal("socket headers or bytes missing", v)
	}
	used := v.DailyLimit
	if err := m.Update(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://synthetic.invalid/item"); err == nil {
		t.Fatal("idle connection bypassed quota")
	}
	if calls.Load() != 2 {
		t.Fatal("request sent at quota", calls.Load())
	}
	v = m.View()
	zero := int64(0)
	if err := m.Update(proxytraffic.Patch{DailyUsed: &zero, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://synthetic.invalid/item")
	if err != nil {
		t.Fatal("correction did not resume", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatal("not resumed")
	}
}

func TestMeteredNoProxyContinuesAtQuota(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("direct")) }))
	defer origin.Close()
	m, _ := proxytraffic.New(proxytraffic.DefaultState(), nil)
	v := m.View()
	used := v.DailyLimit
	if err := m.Update(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	transport, _ := NewMeteredTransport("", m)
	defer transport.CloseIdleConnections()
	selector := (&httpproxy.Config{HTTPProxy: "http://127.0.0.1:1", NoProxy: "*"}).ProxyFunc()
	transport.proxy.Proxy = func(r *http.Request) (*url.URL, error) { return selector(r.URL) }
	client := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal("direct request paused", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := m.View(); got.DailyUsed != used || got.MonthlyUsed != 0 {
		t.Fatal("direct traffic charged", got)
	}
}

func TestProxyValidationDoesNotExposeCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://fixture-user:fixture-secret@proxy:bad", "http://proxy:65536",
		"http://proxy:0", "http://proxy:", "http://:7890", "127.0.0.1:7890",
		"vless://fixture-secret@proxy:443", "http://proxy/path",
		"http://proxy?token=fixture-secret", "http://proxy#fixture-secret",
	} {
		if _, err := ParseProxyURL(raw); err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("invalid proxy accepted or credentials exposed")
		}
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		if _, err := ParseProxyURL(scheme + "://user:password@[::1]:7890"); err != nil {
			t.Fatal(scheme, err)
		}
	}
}

func TestExplicitProxyNeverFallsBackDirect(t *testing.T) {
	var directRequests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directRequests.Add(1)
	}))
	defer origin.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv("NO_PROXY", "*")
	transport, err := NewTransport("http://fixture-user:fixture-secret@" + address)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	_, err = client.Get(origin.URL)
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("failed proxy must fail without exposing credentials")
	}
	if directRequests.Load() != 0 {
		t.Fatal("failed proxy silently fell back to a direct connection")
	}
}
