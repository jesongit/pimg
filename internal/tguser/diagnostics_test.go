package tguser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"pimg/internal/proxytraffic"
)

func TestConnectionFailuresAreTypedAndNeverExposeRawErrors(t *testing.T) {
	for _, item := range []struct {
		err         error
		stage, kind string
	}{
		{&net.DNSError{Err: "private-user:private-password", Name: "private.example"}, "tcp_connect", "dns"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "tcp_connect", "connection_refused"},
		{&transportFailure{stage: "proxy_connect", cause: context.DeadlineExceeded, message: "private-password"}, "proxy_connect", "timeout"},
		{&transportFailure{stage: "proxy_connect", status: 407}, "proxy_connect", "proxy_auth_required"},
		{&transportFailure{stage: "proxy_connect", status: 403}, "proxy_connect", "proxy_rejected"},
		{errSessionInvalid, "session", "session_invalid"},
		{ErrNotAuthorized, "authorization", "session_unauthorized"},
		{proxytraffic.ErrLimit, "proxy_connect", "proxy_quota"},
		{errors.New("socks5://private-user:private-password@private.example:1080"), "mtproto", "transport_error"},
	} {
		f := failureDetails(item.stage, item.err)
		if f.Kind != item.kind || f.Stage != item.stage {
			t.Fatal(f, item.kind)
		}
		data, _ := json.Marshal(f)
		for _, secret := range []string{"private-user", "private-password", "private.example"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("raw error leaked", string(data))
			}
		}
	}
}

func TestDiagnosticDialFailureHistoryAndReadyRecovery(t *testing.T) {
	clearProxyEnvironment(t)
	c := New(Options{APIID: 1, APIHash: "fixture"})
	dial := c.diagnosticDial(func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED })
	c.connectionState(telegram.ConnectionStateConnecting)
	for i := 0; i < 25; i++ {
		_, _ = dial(context.Background(), "tcp", "private-target:443")
	}
	d := c.Diagnostics()
	if d.DialAttempts != 25 || d.InFlightDials != 0 || len(d.RecentFailures) != 16 || d.LastFailure.Kind != "connection_refused" || d.ProxyMode != "direct" || d.ConnectingSince == 0 {
		t.Fatal(d)
	}
	if c.Status().Error == "" {
		t.Fatal("connecting UI did not receive safe failure reason")
	}
	d.RecentFailures[0].Message = "external mutation"
	d.LastFailure.Message = "external mutation"
	if c.Diagnostics().LastFailure.Message == "external mutation" {
		t.Fatal("diagnostic snapshot changed live state")
	}
	c.connectionFailure("tcp_connect", context.Canceled)
	if c.Diagnostics().LastFailure.Kind != "connection_refused" {
		t.Fatal("cancellation overwrote real error")
	}
	c.connectionState(telegram.ConnectionStateReady)
	if d = c.Diagnostics(); !d.Connected || d.ConnectingSince != 0 || d.LastReadyAt == 0 || c.Status().Error != "" {
		t.Fatal(d, c.Status())
	}
}

func TestDiagnosticDialRecordsActualSuccessfulProxyRoute(t *testing.T) {
	clearProxyEnvironment(t)
	proxy, _ := localConnectProxy(t, "ok", http.StatusOK)
	c := New(Options{ProxyURL: proxy})
	dial, err := clientProxyDial(proxy)
	if err != nil {
		t.Fatal(err)
	}
	assertDialMarker(t, c.diagnosticDial(dial), "149.154.167.50:443", "ok")
	d := c.Diagnostics()
	if d.DialAttempts != 1 || d.DialSuccesses != 1 || d.InFlightDials != 0 || d.ProxyMode != "configured" || d.ProxyScheme != "http" {
		t.Fatal(d)
	}
}

func TestTelegramRunObservesProxyAuthFailureWithoutSendingCredentialsToDiagnostics(t *testing.T) {
	clearProxyEnvironment(t)
	proxy, _ := localConnectProxy(t, "", http.StatusProxyAuthRequired)
	var logs bytes.Buffer
	c := New(Options{APIID: 1, APIHash: "private-api-hash", SessionFile: filepath.Join(t.TempDir(), "session.enc"), SessionKey: bytes.Repeat([]byte{7}, 32), ProxyURL: strings.Replace(proxy, "://", "://private-user:private-password@", 1), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for c.Diagnostics().LastFailure == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	d := c.Diagnostics()
	cancel()
	<-done
	if d.LastFailure == nil || d.LastFailure.Kind != "proxy_auth_required" || d.LastFailure.HTTPStatus != 407 || d.ProxyMode != "configured" || d.ProxyScheme != "http" || d.DialAttempts == 0 {
		t.Fatal(d)
	}
	data, _ := json.Marshal(d)
	for _, secret := range []string{"private-api-hash", "private-user", "private-password", proxy} {
		if strings.Contains(string(data)+logs.String(), secret) {
			t.Fatal("sensitive transport data leaked")
		}
	}
}
