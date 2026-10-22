package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/config"
	"pimg/internal/dbx"
	"pimg/internal/diagnostics"
)

func diagnosticToken(t *testing.T, s *Server, token, scope, owner string) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: token, Name: token, Scope: scope, OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
}

func diagnosticCall(s *Server, path, remote, token, xff string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestDiagnosticTokenIsReadOnlyRevocableAndSanitized(t *testing.T) {
	s := interactionServer(t)
	s.Log = slog.New(diagnostics.NewHandler(slog.NewTextHandler(io.Discard, nil), s.Diagnostics))
	diagnosticToken(t, s, "readonly", "diagnostics", dbx.AdminOwner)
	diagnosticToken(t, s, "admin-token", "admin", dbx.AdminOwner)
	diagnosticToken(t, s, "other-token", "account", "user:test")
	diagnosticToken(t, s, "spoof-owner", "diagnostics", "user:test")
	for _, path := range []string{"/api/diagnostics/status", "/api/diagnostics/logs", "/api/diagnostics/activity"} {
		for _, token := range []string{"", "bad-token", "other-token", "spoof-owner"} {
			if w := diagnosticCall(s, path, "198.51.100.10:9999", token, ""); w.Code != 401 {
				t.Fatal(path, token, w.Code, w.Body.String())
			}
		}
		for _, token := range []string{"readonly", "admin-token"} {
			if w := diagnosticCall(s, path, "198.51.100.10:9999", token, ""); w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal(path, token, w.Code, w.Body.String())
			}
		}
	}
	for _, path := range []string{"/api/admin/settings", "/api/admin/tokens", "/api/admin/files", "/api/admin/tasks", "/api/account/files"} {
		if w := diagnosticCall(s, path, "198.51.100.10:9999", "readonly", ""); w.Code != 401 {
			t.Fatal("scope escaped", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/admin/logs/clear", strings.NewReader(`{"confirm":"CLEAR"}`))
	r.Header.Set("Authorization", "Bearer readonly")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("scope mutated history", w.Code)
	}
	s.Log.Error("upload failed", "err", "https://username:password@example.test/object?signature=private-query", "password", "private-password")
	w = diagnosticCall(s, "/api/diagnostics/logs?level=error", "198.51.100.10:9999", "readonly", "")
	if !strings.Contains(w.Body.String(), "upload failed") || strings.Contains(w.Body.String(), "private-query") || strings.Contains(w.Body.String(), "private-password") {
		t.Fatal(w.Body.String())
	}
	id, err := s.DB.EnqueueTask("diagnostic-fixture", "", `{"password":"payload-secret"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='failed',last_error='Bearer error-secret',result='{"secret":"result-secret"}' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.RecordIngestEvent(dbx.IngestEvent{SourceKey: "fixture", Type: "failed", Reason: "token=event-secret", Body: json.RawMessage(`{"password":"body-secret"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"tasks", "events"} {
		w = diagnosticCall(s, "/api/diagnostics/activity?source="+source, "198.51.100.10:9999", "readonly", "")
		for _, secret := range []string{"payload-secret", "result-secret", "body-secret", "event-secret", "error-secret"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("activity leaked", secret, w.Body.String())
			}
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, suffix := range []string{"logs?limit=201", "logs?after=-1", "logs?level=debug", "logs?q=" + strings.Repeat("x", 257), "activity?limit=0", "activity?source=arbitrary", "activity?before=bad", "activity?since=-1"} {
		if w := diagnosticCall(s, "/api/diagnostics/"+suffix, "198.51.100.10:9999", "readonly", ""); w.Code != 400 {
			t.Fatal(suffix, w.Code)
		}
	}
	if err = s.DB.RevokeToken("readonly"); err != nil {
		t.Fatal(err)
	}
	if w := diagnosticCall(s, "/api/diagnostics/status", "198.51.100.10:9999", "readonly", ""); w.Code != 401 {
		t.Fatal("revoked token worked")
	}
}

func TestDiagnosticIPTrustDoesNotEscapeOrTrustSpoofedHeaders(t *testing.T) {
	s := interactionServer(t)
	s.diagnosticIPs, _ = config.ParseIPRanges([]string{"192.168.2.0/24", "::1"})
	s.diagnosticProxies, _ = config.ParseIPRanges([]string{"10.0.0.2"})
	for _, test := range []struct {
		remote, xff, token string
		status             int
	}{
		{"192.168.2.20:9999", "", "", 200}, {"[::ffff:192.168.2.20]:9999", "", "", 200}, {"[::1]:9999", "", "", 200},
		{"198.51.100.10:9999", "192.168.2.20", "", 401},
		{"10.0.0.2:9999", "192.168.2.20", "", 200},
		{"10.0.0.2:9999", "192.168.2.20, 198.51.100.10", "", 401},
		{"10.0.0.2:9999", "", "", 401}, {"10.0.0.2:9999", "bad IP", "", 401},
		{"192.168.2.20:9999", "", "invalid", 401}, {"invalid remote", "192.168.2.20", "", 401},
	} {
		if w := diagnosticCall(s, "/api/diagnostics/status", test.remote, test.token, test.xff); w.Code != test.status {
			t.Fatal(test, w.Code, w.Body.String())
		}
	}
	if w := diagnosticCall(s, "/api/admin/settings", "192.168.2.20:9999", "", ""); w.Code != 401 {
		t.Fatal("IP allowlist granted admin")
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		r := httptest.NewRequest("GET", "/api/diagnostics/logs", nil)
		r.RemoteAddr = "192.168.2.20:9999"
		v := "cross-site"
		if header == "Origin" {
			v = "https://evil.example"
		}
		r.Header.Set(header, v)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross site LAN access", w.Code)
		}
	}
}

func TestCreateDiagnosticTokenAndActivityReadHasNoTaskSideEffects(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	w := call("POST", "/api/admin/tokens", strings.NewReader(`{"name":"remote diagnosis","scope":"diagnostics"}`), "application/json")
	var token struct {
		Token string `json:"token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "upload", "expired-intake", "album:0;encrypted:false", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE task_operations SET expires_at=1 WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if w = diagnosticCall(s, "/api/diagnostics/activity?limit=1", "198.51.100.1:1234", token.Token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var state string
	s.DB.QueryRow(`SELECT input_state FROM task_operations WHERE id=?`, op.ID).Scan(&state)
	if state != "open" {
		t.Fatal("read modified intake", state)
	}
	r := httptest.NewRequest("GET", "/api/diagnostics/status", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "carrier-test-session"})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie authorized diagnosis")
	}
}
