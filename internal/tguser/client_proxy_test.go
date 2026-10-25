package tguser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pimg/internal/proxytraffic"

	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/http/httpproxy"
)

type proxyRequest struct{ method, target string }

func TestTelegramProxyTrafficCountsAndPauses(t *testing.T) {
	clearProxyEnvironment(t)
	u, _ := localConnectProxy(t, "tracked", http.StatusOK)
	m, _ := proxytraffic.New(proxytraffic.DefaultState(), nil)
	dial, err := clientProxyDial(u, m)
	if err != nil {
		t.Fatal(err)
	}
	assertDialMarker(t, dial, "149.154.167.50:443", "tracked")
	v := m.View()
	if v.DailyUsed <= int64(len("tracked")) || v.DailyUsed != v.MonthlyUsed {
		t.Fatal("CONNECT bytes not counted", v)
	}
	used := v.DailyLimit
	if err := m.Update(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "149.154.167.50:443"); !errors.Is(err, proxytraffic.ErrLimit) {
		t.Fatal("TG dial bypassed quota", err)
	}
	c := New(Options{ProxyURL: u, ProxyTraffic: m})
	if _, _, err := c.ready(); !errors.Is(err, proxytraffic.ErrLimit) {
		t.Fatal("ready masked quota as unauthorized", err)
	}
	if err := c.safeError(errors.New("private network failure")); !errors.Is(err, proxytraffic.ErrLimit) {
		t.Fatal("sanitization lost quota signal", err)
	}
}

// The proxy returns bytes itself; it never resolves or connects to the target.
func localConnectProxy(t *testing.T, marker string, status int) (string, <-chan proxyRequest) {
	t.Helper()
	requests := make(chan proxyRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- proxyRequest{r.Method, r.Host}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 Connection Established\r\n\r\n%s", marker)
		_ = rw.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

func assertDialMarker(t *testing.T, dial dcs.DialFunc, target, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != want {
		t.Fatalf("tunnel bytes %q, %v; want %q", got, err, want)
	}
}

func refuseDirect(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unexpected direct dial")
}

func TestUserEnvironmentProxySelection(t *testing.T) {
	httpsProxy, httpsRequests := localConnectProxy(t, "https-env", http.StatusOK)
	httpProxy, httpRequests := localConnectProxy(t, "http-env", http.StatusOK)
	target := "203.0.113.20:443"
	dial, err := environmentProxyDial(&httpproxy.Config{HTTPSProxy: httpsProxy, HTTPProxy: httpProxy}, refuseDirect)
	if err != nil {
		t.Fatal(err)
	}
	assertDialMarker(t, dial, target, "https-env")
	if r := <-httpsRequests; r.method != http.MethodConnect || r.target != target {
		t.Fatal("incorrect HTTPS proxy CONNECT", r)
	}
	select {
	case r := <-httpRequests:
		t.Fatal("lower-priority HTTP proxy used", r)
	default:
	}

	// Standard environment shorthand is accepted without a URL scheme.
	dial, err = environmentProxyDial(&httpproxy.Config{HTTPProxy: strings.TrimPrefix(httpProxy, "http://")}, refuseDirect)
	if err != nil {
		t.Fatal(err)
	}
	assertDialMarker(t, dial, target, "http-env")
	if r := <-httpRequests; r.method != http.MethodConnect || r.target != target {
		t.Fatal("incorrect HTTP proxy CONNECT", r)
	}
}

func TestUserEnvironmentNoProxyMatchesTarget(t *testing.T) {
	proxyURL, requests := localConnectProxy(t, "proxy", http.StatusOK)
	for _, tt := range []struct {
		name, noProxy, target string
		bypass                bool
	}{
		{"CIDR", "203.0.113.0/24", "203.0.113.20:443", true},
		{"IPv6 CIDR", "2001:db8::/32", "[2001:db8::20]:443", true},
		{"domain", ".example.invalid", "dc.example.invalid:443", true},
		{"port", "dc.example.invalid:443", "dc.example.invalid:443", true},
		{"different port", "dc.example.invalid:80", "dc.example.invalid:443", false},
		{"wildcard", "*", "203.0.113.20:443", true},
		{"localhost", "", "localhost:443", true},
		{"loopback", "", "127.0.0.1:443", true},
		{"unmatched", "other.invalid", "dc.example.invalid:443", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directCalls := 0
			direct := func(_ context.Context, network, target string) (net.Conn, error) {
				directCalls++
				if network != "tcp" || target != tt.target {
					t.Fatal("direct target changed", network, target)
				}
				reader, writer := net.Pipe()
				go func() { _, _ = io.WriteString(writer, "direct"); _ = writer.Close() }()
				return reader, nil
			}
			dial, err := environmentProxyDial(&httpproxy.Config{HTTPSProxy: proxyURL, NoProxy: tt.noProxy}, direct)
			if err != nil {
				t.Fatal(err)
			}
			want := "proxy"
			if tt.bypass {
				want = "direct"
			}
			assertDialMarker(t, dial, tt.target, want)
			if tt.bypass {
				if directCalls != 1 {
					t.Fatal("NO_PROXY did not use direct dial", directCalls)
				}
				select {
				case r := <-requests:
					t.Fatal("NO_PROXY target sent to proxy", r)
				default:
				}
			} else {
				if directCalls != 0 {
					t.Fatal("unmatched target bypassed proxy")
				}
				if r := <-requests; r.target != tt.target {
					t.Fatal("proxy target changed", r)
				}
			}
		})
	}
}

func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(name, "")
	}
}

func TestUserExplicitProxyOverridesEnvironmentAndNoProxy(t *testing.T) {
	clearProxyEnvironment(t)
	explicit, requests := localConnectProxy(t, "explicit", http.StatusOK)
	t.Setenv("HTTPS_PROXY", "http://user:secret@invalid.invalid/path")
	t.Setenv("HTTP_PROXY", "http://invalid.invalid/path")
	t.Setenv("NO_PROXY", "*")
	dial, err := clientProxyDial(explicit)
	if err != nil {
		t.Fatal(err)
	}
	target := "127.0.0.1:1"
	assertDialMarker(t, dial, target, "explicit")
	if r := <-requests; r.target != target {
		t.Fatal("explicit proxy did not override NO_PROXY", r)
	}
}

func TestUserProxyEnvironmentIsFreshAndNoProxyPreservesDefault(t *testing.T) {
	clearProxyEnvironment(t)
	for _, noProxy := range []string{"", "*"} {
		t.Setenv("NO_PROXY", noProxy)
		if dial, err := clientProxyDial(""); err != nil || dial != nil {
			t.Fatal("unconfigured proxy changed default resolver", err)
		}
	}
	t.Setenv("HTTPS_PROXY", "127.0.0.1:1")
	if dial, err := clientProxyDial(""); err != nil || dial == nil {
		t.Fatal("new environment proxy not read", err)
	}
	t.Setenv("HTTPS_PROXY", "http://user:secret@invalid.invalid/path")
	if _, err := clientProxyDial(""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("changed malformed proxy ignored or leaked", err)
	}
	t.Setenv("HTTPS_PROXY", "")
	if dial, err := clientProxyDial(""); err != nil || dial != nil {
		t.Fatal("removed environment proxy cached", err)
	}
}

func TestUserProxyFailureNeverFallsBackToDirect(t *testing.T) {
	proxyURL, requests := localConnectProxy(t, "", http.StatusBadGateway)
	directCalls := 0
	direct := func(context.Context, string, string) (net.Conn, error) {
		directCalls++
		return nil, errors.New("unexpected direct dial")
	}
	dial, err := environmentProxyDial(&httpproxy.Config{HTTPSProxy: proxyURL}, direct)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dial(context.Background(), "tcp", "203.0.113.20:443"); err == nil {
		_ = conn.Close()
		t.Fatal("failed environment proxy returned success")
	}
	if directCalls != 0 {
		t.Fatal("failed environment proxy fell back to direct")
	}
	<-requests

	// An explicit proxy must also fail even though its target is reachable.
	dial, err = proxyDial(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dial(context.Background(), "tcp", strings.TrimPrefix(proxyURL, "http://")); err == nil {
		_ = conn.Close()
		t.Fatal("failed explicit proxy fell back to direct")
	}
	<-requests
}

func TestUserEnvironmentProxyRejectsInvalidAndCGI(t *testing.T) {
	for _, raw := range []string{"http://user:secret@invalid.invalid/path", "http://%secret", "ftp://user:secret@invalid.invalid"} {
		if _, err := environmentProxyDial(&httpproxy.Config{HTTPSProxy: raw}, refuseDirect); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid environment proxy accepted or leaked", err)
		}
	}
	dial, err := environmentProxyDial(&httpproxy.Config{HTTPProxy: "127.0.0.1:1", CGI: true}, refuseDirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "203.0.113.20:443"); err == nil {
		t.Fatal("unsafe CGI HTTP_PROXY accepted")
	}
}
