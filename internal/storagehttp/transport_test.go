package storagehttp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
