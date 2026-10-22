package server

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestUploadFormatsSettingsExposeDefaultsAndEffectivePolicy(t *testing.T) {
	s := importServer(t)
	defaultImportFormats(t, s)
	call := carrierRequest(t, s)
	for _, custom := range []string{"", "png"} {
		if err := s.DB.SetSetting("site.allowed_exts", custom); err != nil {
			t.Fatal(err)
		}
		if err := s.loadSiteSettings(); err != nil {
			t.Fatal(err)
		}
		s.applyDynamicFromSettings()
		w := call("GET", "/api/admin/settings", nil, "")
		var settings struct {
			Allowed   string   `json:"allowed_exts"`
			Defaults  []string `json:"default_allowed_exts"`
			Effective []string `json:"effective_allowed_exts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, ext := range []string{"mov", "ts", "avi", "flv", "m2ts", "mts", "heic", "heif", "jpeg"} {
			if !slices.Contains(settings.Defaults, ext) {
				t.Error("settings omitted default format", ext)
			}
		}
		want := settings.Defaults
		if custom != "" {
			want = []string{"png"}
		}
		if settings.Allowed != custom || !slices.Equal(settings.Effective, want) || !slices.Equal(s.Svc.AllowedExts(), want) {
			t.Fatal("displayed and enforced formats differ", settings)
		}
	}
}

func TestLoginCookieSupportsHTTPIPAndHTTPSProxy(t *testing.T) {
	for _, tc := range []struct {
		name, address, publicURL, forwarded string
		wantSecure                          bool
	}{
		{"http-ip", "http://192.0.2.10:8787/api/auth/login", "", "", false},
		{"http-ip-with-public-domain", "http://192.0.2.10:8787/api/auth/login", "https://photos.example", "", false},
		{"https-ip", "https://192.0.2.10:8787/api/auth/login", "", "", true},
		{"https-proxy", "http://photos.example/api/auth/login", "", "https", true},
		{"https-configured-domain", "http://photos.example/api/auth/login", "https://photos.example", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := interactionServer(t)
			if err := s.DB.EnsureAdminWithSetup("qa", adminPasswordHash(t, "fixture-password"), false); err != nil {
				t.Fatal(err)
			}
			s.Svc.SetPublicBaseURL(tc.publicURL)
			r := httptest.NewRequest("POST", tc.address, strings.NewReader(`{"username":"qa","password":"fixture-password"}`))
			r.Header.Set("Content-Type", "application/json")
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			cookies := w.Result().Cookies()
			if w.Code != 200 || len(cookies) != 1 {
				t.Fatal(w.Code, w.Body.String(), cookies)
			}
			if cookies[0].Secure != tc.wantSecure || !cookies[0].HttpOnly || cookies[0].Path != "/" {
				t.Fatal("cookie incompatible with access scheme", cookies[0])
			}
		})
	}
}
