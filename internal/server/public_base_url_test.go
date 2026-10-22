package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/store"
)

func TestPublicBaseURLWebOverrideAndRestart(t *testing.T) {
	s := interactionServer(t)
	s.Cfg.Server.PublicBaseURL = "http://legacy.example"
	restart := func() {
		t.Helper()
		svc, err := store.New(s.Cfg, s.DB, s.Svc.Master, s.Svc.TG, s.Svc.R2, s.Log)
		if err != nil {
			t.Fatal(err)
		}
		s.Svc = svc
		s.applyDynamicFromSettings()
	}
	restart()
	if s.Svc.PublicBaseURL() != "" {
		t.Fatal("retired config replaced the web value")
	}
	f, err := legacyUploadFixture(s.Svc, context.Background(), bytes.NewReader(interactionPNG(t)), "existing.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunDirectArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	private, err := legacyUploadFixture(s.Svc, context.Background(), bytes.NewReader(interactionPNG(t)), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), private.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunTempMaterialize(context.Background(), private.ID, "original"); err != nil {
		t.Fatal(err)
	}
	patch := func(value string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"public_base_url": value})
		w := httptest.NewRecorder()
		s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	check := func(base string) {
		t.Helper()
		if s.Svc.PublicBaseURL() != base || !strings.HasPrefix(s.Svc.PublicURL(f), base+"/media/permanent/") || s.Svc.ThumbURL(f) != base+"/previews/"+f.ID {
			t.Fatal("existing local URLs did not update", s.Svc.PublicURL(f), s.Svc.ThumbURL(f))
		}
		links, err := s.Svc.FileLinks(context.Background(), f.ID)
		if err != nil || len(links.Items) == 0 {
			t.Fatal("missing existing publication", links, err)
		}
		for _, link := range links.Items {
			if !strings.HasPrefix(link.URL, base+"/") {
				t.Fatal("stale publication link", link.URL)
			}
		}
		view, err := s.Svc.GetTempView(context.Background(), private, "original")
		if err != nil || !strings.HasPrefix(view.URL, base+"/media/temp/") {
			t.Fatal("stale temporary link", view, err)
		}
		w := httptest.NewRecorder()
		s.handleGetSettings(w, httptest.NewRequest("GET", "/api/admin/settings", nil))
		var settings map[string]any
		json.Unmarshal(w.Body.Bytes(), &settings)
		if settings["public_base_url"] != base {
			t.Fatal("settings did not return effective value", settings["public_base_url"])
		}
	}
	check("")
	patch(" https://images.example.com/// ")
	check("https://images.example.com")
	// Clearing is a durable override, including when a legacy config remains.
	restart()
	check("https://images.example.com")
	patch("")
	check("")
	restart()
	check("")
	if s.Cfg.Server.PublicBaseURL != "http://legacy.example" {
		t.Fatal("runtime change mutated original config")
	}
}

func TestPublicBaseURLValidationAtomicityAndCookie(t *testing.T) {
	s := interactionServer(t)
	s.DB.EnsureAdmin("admin", adminPasswordHash(t, "existing-password"))
	for _, value := range []any{nil, 1, "example.com", "javascript:alert(1)", "https://user:pass@host", "https://host/path", "https://host?x=y", "https://host?", "https://host#fragment", "https://host\\evil", "https://", "https://host:bad"} {
		body, _ := json.Marshal(map[string]any{"public_base_url": value, "site_name": "must not save"})
		w := httptest.NewRecorder()
		s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", bytes.NewReader(body)))
		if w.Code != 400 {
			t.Fatal("accepted invalid URL", value, w.Code, w.Body.String())
		}
		if name, _ := s.DB.GetSetting("site.name"); name != "" {
			t.Fatal("invalid URL saved unrelated field")
		}
	}
	for _, secure := range []bool{true, false} {
		base := "http://127.0.0.1:8787"
		if secure {
			base = "https://images.example.com"
		}
		body, _ := json.Marshal(map[string]string{"public_base_url": base})
		w := httptest.NewRecorder()
		s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		// Simulate the configured public host, including an HTTPS proxy whose
		// backend connection is HTTP. An unrelated HTTP IP must stay usable too
		// (covered by TestLoginCookieSupportsHTTPIPAndHTTPSProxy).
		request := httptest.NewRequest("POST", strings.Replace(base, "https:", "http:", 1)+"/api/auth/login", strings.NewReader(`{"username":"admin","password":"existing-password"}`))
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, request)
		if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Secure != secure {
			t.Fatal("cookie did not follow effective address", secure, w.Code)
		}
	}
}

func TestPublicBaseURLBeforeStorageConfigured(t *testing.T) {
	s := interactionServer(t)
	s.Cfg.Storage.Cold, s.Cfg.Storage.Hot = "telegram", "r2"
	svc, err := store.New(s.Cfg, s.DB, s.Svc.Master, nil, nil, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	s.Svc = svc
	policy := svc.StoragePolicy()
	w := httptest.NewRecorder()
	s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", strings.NewReader(`{"public_base_url":"https://first.example"}`)))
	if w.Code != 200 || svc.PublicBaseURL() != "https://first.example" {
		t.Fatal(w.Code, w.Body.String())
	}
	if after := svc.StoragePolicy(); after.Permanent != policy.Permanent || after.Temporary != policy.Temporary || strings.Join(after.Archives, ",") != strings.Join(policy.Archives, ",") {
		t.Fatal("site change modified storage policy")
	}
}
