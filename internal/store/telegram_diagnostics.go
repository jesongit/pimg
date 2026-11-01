package store

import (
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
	"pimg/internal/tguser"
)

type TelegramDiagnostics struct {
	ObservedAt        int64                         `json:"observed_at"`
	State             string                        `json:"state"`
	Configured        bool                          `json:"configured"`
	RuntimeStarted    bool                          `json:"runtime_started"`
	RuntimeRunning    bool                          `json:"runtime_running"`
	RuntimeError      bool                          `json:"runtime_error"`
	RuntimeErrorKind  string                        `json:"runtime_error_kind,omitempty"`
	SessionStorage    string                        `json:"session_storage"`
	ProxyConfigured   bool                          `json:"proxy_configured"`
	EnvironmentProxy  bool                          `json:"environment_proxy_configured"`
	ProxyQuotaBlocked bool                          `json:"proxy_quota_blocked"`
	Connection        *tguser.ConnectionDiagnostics `json:"connection,omitempty"`
}

// Observe only: no reconnect, authorization probe, account pin, session load,
// provider call or quota persistence. Account/group IDs and credentials are
// deliberately absent, including error text returned by arbitrary transports.
func (s *Service) TelegramDiagnostics() TelegramDiagnostics {
	connection := s.storageConnection()
	environment := httpproxy.FromEnvironment()
	d := TelegramDiagnostics{ObservedAt: time.Now().UnixMilli(), State: "unconfigured", SessionStorage: "unknown", ProxyConfigured: connection.ProxyURL != "", EnvironmentProxy: environment.HTTPSProxy != "" || environment.HTTPProxy != ""}
	if s.proxyTraffic != nil {
		d.ProxyQuotaBlocked = s.proxyTraffic.Snapshot().Blocked
	}
	r := s.telegram
	if r == nil {
		return d
	}
	r.mu.RLock()
	c, root, done, runError := r.client, r.root, r.done, r.lastError
	r.mu.RUnlock()
	d.RuntimeStarted = root != nil && root.Err() == nil
	d.RuntimeRunning = d.RuntimeStarted && !channelDone(done)
	d.RuntimeError = runError != ""
	if c != nil {
		status := c.Status() // Never call r.Status(), which may pin an account.
		d.Configured = status.Configured
		switch status.State {
		case "unconfigured", "connecting", "phone", "code", "password", "authorized", "reauth_required", "error":
			d.State = status.State
		default:
			d.State = "unknown"
		}
		if observer, ok := c.(interface {
			Diagnostics() tguser.ConnectionDiagnostics
		}); ok {
			value := observer.Diagnostics()
			d.Connection = &value
		}
	}
	if runError != "" {
		d.State = "error"
		d.RuntimeErrorKind = "runtime_stopped"
		switch {
		case strings.Contains(runError, "正由另一个"):
			d.RuntimeErrorKind = "session_in_use"
		case strings.Contains(runError, "锁定"):
			d.RuntimeErrorKind = "session_lock"
		case strings.Contains(runError, "会话目录"):
			d.RuntimeErrorKind = "session_directory"
		}
	}
	info, err := os.Lstat(r.sessionFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.SessionStorage = "missing"
	case err != nil:
		d.SessionStorage = "inaccessible"
	case !info.Mode().IsRegular():
		d.SessionStorage = "not_regular"
	default:
		d.SessionStorage = "present"
	}
	// Normalize a potentially unset connection label without returning any
	// values from the proxy URL or process environment.
	if d.Connection != nil && strings.TrimSpace(d.Connection.TransportState) == "" {
		d.Connection.TransportState = "not_started"
	}
	return d
}
