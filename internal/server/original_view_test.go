package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestOriginalViewProgressAccessAndPersistentCachePolicy(t *testing.T) {
	s := interactionServer(t)
	t.Cleanup(s.Svc.StopMediaLoads)
	data := interactionPNG(t)
	f, err := legacyUploadFixture(s.Svc,context.Background(), bytes.NewReader(data), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("view-only-fixture"))
	if err = s.DB.InsertToken(&dbx.APIToken{ID: "view-fixture", Name: "test", TokenHash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer view-only-fixture")
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	path := "/api/admin/files/" + f.ID + "/original"
	if w := call("POST", path, `{}`, false); w.Code != 401 {
		t.Fatal("unauthenticated load accepted", w.Code)
	}
	var settings map[string]any
	w := call("GET", "/api/admin/settings", "", true)
	json.Unmarshal(w.Body.Bytes(), &settings)
	if settings["original_cache_hours"] != float64(24) {
		t.Fatal("missing one-day default", settings["original_cache_hours"])
	}
	for _, body := range []string{`{"original_cache_hours":0}`, `{"original_cache_hours":-1}`, `{"original_cache_hours":1.5}`, `{"original_cache_hours":8761}`, `{"original_cache_hours":null}`} {
		if w := call("PATCH", "/api/admin/settings", body, true); w.Code != 400 {
			t.Fatal("bad policy accepted", body, w.Code)
		}
	}
	if w := call("PATCH", "/api/admin/settings", `{"original_cache_hours":48}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.Svc.OriginalCacheHours() != 48 {
		t.Fatal("runtime cache policy not updated")
	}
	if saved, _ := s.DB.GetSetting("site.original_cache_hours"); saved != "48" {
		t.Fatal("cache policy not persisted")
	}
	s.Svc.SetOriginalCacheHours(24)
	s.applyDynamicFromSettings()
	if s.Svc.OriginalCacheHours() != 48 {
		t.Fatal("saved policy not restored")
	}
	restarted, err := store.New(s.Cfg, s.DB, s.Svc.Master, s.Svc.TG, s.Svc.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.StopMediaLoads)
	if _, err := New(s.Cfg, s.DB, restarted, nil, s.Log); err != nil {
		t.Fatal(err)
	}
	if restarted.OriginalCacheHours() != 48 {
		t.Fatal("restart lost saved cache duration")
	}
	if cached, err := restarted.CachedOriginal(f); err != nil {
		t.Fatal("restart lost verified original cache", err)
	} else {
		cached.Close()
	}
	if w := call("GET", "/admin", "", true); !strings.Contains(w.Header().Get("Content-Security-Policy"), "blob:") {
		t.Fatal("image progress display blocked by CSP")
	}
	s.Svc.ForgetMediaViews(f)
	deadline := time.Now().Add(5 * time.Second)
	var view struct {
		store.OriginalView
		URL string `json:"url"`
	}
	for {
		w = call("POST", path, `{}`, true)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Status == "ready" {
			break
		}
		if view.Status != "loading" || view.Percent >= 100 || time.Now().After(deadline) {
			t.Fatal("bad loading progress", view)
		}
		time.Sleep(time.Millisecond * 5)
	}
	if view.Percent != 100 || view.Loaded != int64(len(data)) || view.ExpiresAt <= time.Now().Add(47*time.Hour).Unix() {
		t.Fatal("incorrect completed receipt", view)
	}
	if w = call("GET", view.URL, "", false); w.Code != 401 {
		t.Fatal("cached original bypassed authentication")
	}
	if w = call("GET", view.URL, "", true); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("cached original differs", w.Code)
	}
	r := httptest.NewRequest("GET", view.URL, nil)
	r.Header.Set("Authorization", "Bearer view-only-fixture")
	r.Header.Set("Range", "bytes=0-15")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), data[:16]) {
		t.Fatal("cached media range failed", w.Code)
	}
	if _, err = s.Svc.QueueConversion(context.Background(), f.ID, false); err != nil {
		t.Fatal(err)
	}
	if w = call("GET", view.URL, "", true); w.Code != 409 {
		t.Fatal("conversion left old cache accessible", w.Code)
	}
	if w = call("GET", privateContentURL(f.ID, "thumb"), "", true); w.Code != 409 {
		t.Fatal("conversion left old preview accessible", w.Code)
	}
}
