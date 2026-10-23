package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserIndexSettingsAtomicAndAdminOnly(t *testing.T) {
	s := interactionServer(t)
	t.Cleanup(s.Svc.StopBrowserIndex)
	request := fileBrowserRequest(t, s)
	get := request("GET", "/api/admin/settings", nil, "")
	var settings struct {
		Paths []string `json:"browser_watch_paths"`
	}
	if get.Code != 200 || json.Unmarshal(get.Body.Bytes(), &settings) != nil || settings.Paths == nil || len(settings.Paths) != 0 {
		t.Fatal(get.Code, get.Body.String())
	}
	for _, endpoint := range []struct{ method, path string }{{"GET", "/api/admin/file-browser/index"}, {"POST", "/api/admin/file-browser/index/refresh"}} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		if w.Code != 401 {
			t.Fatal("index endpoint allowed anonymous access", w.Code)
		}
	}
	source := t.TempDir()
	child := filepath.Join(source, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"browser_watch_paths": []string{child, source}})
	w := request("PATCH", "/api/admin/settings", bytes.NewReader(body), "application/json")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil || len(settings.Paths) != 1 || settings.Paths[0] != source {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, invalid := range []any{nil, "not-an-array", []any{1}, []string{"relative"}, []string{filepath.Join(source, "missing")}} {
		body, _ = json.Marshal(map[string]any{"browser_watch_paths": invalid, "site_name": "must-not-save"})
		w = request("PATCH", "/api/admin/settings", bytes.NewReader(body), "application/json")
		if w.Code != 400 {
			t.Fatal("invalid roots accepted", w.Code, w.Body.String())
		}
		var name string
		_ = s.DB.QueryRow(`SELECT v FROM settings WHERE k='site.name'`).Scan(&name)
		if name == "must-not-save" || len(s.Svc.BrowserWatchPaths()) != 1 {
			t.Fatal("invalid patch applied partially")
		}
	}
	w = request("PATCH", "/api/admin/settings", bytes.NewBufferString(`{"browser_watch_paths":[]}`), "application/json")
	if w.Code != 200 || len(s.Svc.BrowserWatchPaths()) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/api/admin/file-browser/index/refresh", bytes.NewBufferString(`{}`), "application/json")
	if w.Code != 400 {
		t.Fatal("refresh started without configured folders", w.Code)
	}
}
