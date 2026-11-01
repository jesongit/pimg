package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTelegramDiagnosticsDoesNotPinAuthorizeOrReadSessionContents(t *testing.T) {
	s, r := runtimeFixture(t)
	if err := os.WriteFile(r.sessionFile(), []byte("private-session-contents"), 0600); err != nil {
		t.Fatal(err)
	}
	r.lastError = "private-runtime-error"
	c := r.current().(*runtimeFixtureClient)
	c.st.Name, c.st.PasswordHint, c.st.Error = "private-name", "private-password-hint", "private-error"
	d := s.TelegramDiagnostics()
	data, _ := json.Marshal(d)
	for _, secret := range []string{"private-", "account_id", "chat_id", "api_hash", r.sessionFile()} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private data leaked", string(data))
		}
	}
	if d.SessionStorage != "present" || !d.Configured || !d.RuntimeError || d.RuntimeErrorKind != "runtime_stopped" {
		t.Fatal(d)
	}
	if pinned, err := s.DB.GetSetting("telegram.account_id"); err != nil || pinned != "" {
		t.Fatal("diagnostic read pinned account", pinned, err)
	}
	if c.authCalls.Load() != 0 {
		t.Fatal("diagnostic read initiated authorization")
	}
}

func TestTGReadyDoesNotMislabelConnectingAsMissingAuthorization(t *testing.T) {
	s, r := runtimeFixture(t)
	c := r.current().(*runtimeFixtureClient)
	c.st.State, c.st.Error = "connecting", ""
	err := s.TGReady()
	if err == nil || !strings.Contains(err.Error(), "连接") || strings.Contains(err.Error(), "授权") {
		t.Fatal(err)
	}
}
