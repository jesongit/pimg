package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"pimg/internal/diagnostics"
	"strings"
	"testing"
	"time"
)

func TestDebugSettingsDefaultOffAndCleanupPreservesBasicLogsAndBusiness(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	path := s.performanceDatabasePath()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("default off created debug database", err)
	}
	w := call("GET", "/api/admin/settings", nil, "")
	var settings map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil || settings["debug_enabled"] != false {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, value := range []string{`"true"`, `1`, `null`} {
		if w = call("PATCH", "/api/admin/settings", strings.NewReader(`{"debug_enabled":`+value+`,"site_name":"must-not-save"}`), "application/json"); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = call("PATCH", "/api/admin/settings", strings.NewReader(`{"debug_enabled":true}`), "application/json")
	if w.Code != 200 || !s.DB.Performance().Enabled() {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("GET", "/api/admin/files?token=never-store-this-query", nil, "")
	if w.Header().Get("X-Pimg-Request-ID") == "" {
		t.Fatal("debug request has no ID")
	}
	if err := s.DB.Performance().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := s.DB.Performance().Query(t.Context(), time.Hour)
	if err != nil || len(data["metrics"].([]diagnostics.MetricSummary)) == 0 {
		t.Fatal(data, err)
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "never-store-this-query") {
		t.Fatal("query retained")
	}
	diagnosticToken(t, s, "debug-reader", "diagnostics", "admin:1")
	for _, url := range []string{"/api/diagnostics/metrics", "/api/diagnostics/logs", "/api/diagnostics/status"} {
		if w = diagnosticCall(s, url, "198.51.100.1:1234", "debug-reader", ""); w.Code != 200 {
			t.Fatal(url, w.Code, w.Body.String())
		}
	}
	if w = diagnosticCall(s, "/api/diagnostics/metrics?window=invalid", "198.51.100.1:1234", "debug-reader", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	seedInteractionFile(t, s, "kept-after-debug", 1, sql.NullInt64{})
	w = call("PATCH", "/api/admin/settings", strings.NewReader(`{"debug_enabled":false}`), "application/json")
	if w.Code != 200 || s.DB.Performance().Enabled() {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("debug file survived off", err)
	}
	if _, err = s.DB.GetFile("kept-after-debug"); err != nil {
		t.Fatal("business file lost", err)
	}
	if w = diagnosticCall(s, "/api/diagnostics/logs", "198.51.100.1:1234", "debug-reader", ""); w.Code != 200 {
		t.Fatal("logs disabled", w.Code)
	}
	if w = diagnosticCall(s, "/api/admin/settings", "198.51.100.1:1234", "debug-reader", ""); w.Code != 401 {
		t.Fatal("read-only token could change mode")
	}
}

func TestFailedDebugSettingCommitLeavesCollectorAndStorageOff(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_debug_setting BEFORE INSERT ON settings WHEN NEW.k='site.debug_enabled' BEGIN SELECT RAISE(ABORT,'fixture write failure');END`); err != nil {
		t.Fatal(err)
	}
	w := call("PATCH", "/api/admin/settings", strings.NewReader(`{"debug_enabled":true}`), "application/json")
	if w.Code != 500 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.DB.Performance().Enabled() {
		t.Fatal("failed save enabled collector")
	}
	if _, err := os.Stat(s.performanceDatabasePath()); !os.IsNotExist(err) {
		t.Fatal("failed save kept performance data", err)
	}
}

func TestDebugDatabasesAreIsolatedWhenRuntimeDirectoryIsShared(t *testing.T) {
	s := interactionServer(t)
	s.Cfg.Paths.RuntimeDir = t.TempDir()
	path := s.performanceDatabasePath()
	p := diagnostics.NewPerformance(path, nil)
	defer p.Close()
	if err := p.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	p.Observe("http", "fixture", time.Second, 0)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg := *s.Cfg
	cfg.Paths.DataDir = filepath.Join(t.TempDir(), "other-data")
	other := &Server{Cfg: &cfg}
	otherPath := other.performanceDatabasePath()
	if path == otherPath {
		t.Fatal("shared diagnostic file")
	}
	otherPerf := diagnostics.NewPerformance(otherPath, nil)
	defer otherPerf.Close()
	if err := otherPerf.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("other instance deleted active diagnostic data", err)
	}
}

func TestDebugHTTPPanicFinishesAttemptWithoutRetainingRequestPath(t *testing.T) {
	s := interactionServer(t)
	if err := s.DB.Performance().SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	h := s.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic panic") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/private-filename?token=private-value", nil))
	}()
	if err := s.DB.Performance().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := s.DB.Performance().Query(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range data["metrics"].([]diagnostics.MetricSummary) {
		if m.Kind == "http" {
			found = true
			if m.Count != 1 || m.Errors != 1 {
				t.Fatal(m)
			}
		}
	}
	if !found {
		t.Fatal(data)
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "private-filename") || strings.Contains(string(raw), "private-value") {
		t.Fatal("request input retained")
	}
}
