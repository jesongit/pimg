package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestFooterSettingsPersistenceOrderAndEmpty(t *testing.T) {
	s := interactionServer(t)
	if got := s.siteSettings().FooterLinks; len(got) != 1 || got[0].Icon != "github" {
		t.Fatalf("default links: %+v", got)
	}
	links := []FooterLink{{ID: "mail", Label: "联系我", Icon: "mail", URL: "mailto:hello@example.com"}, {ID: "blog", Label: "博客", Icon: "custom", URL: "https://example.com/blog", IconURL: "/images/blog.svg"}}
	body, _ := json.Marshal(map[string]any{"footer_text": "记录生活中的光", "footer_links": links})
	w := httptest.NewRecorder()
	s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if err := s.loadSiteSettings(); err != nil {
		t.Fatal(err)
	}
	if got := s.siteSettings(); got.FooterText != "记录生活中的光" || !reflect.DeepEqual(got.FooterLinks, links) {
		t.Fatalf("not restored in order: %+v", got)
	}
	w = httptest.NewRecorder()
	s.handleSite(w, httptest.NewRequest("GET", "/api/site", nil))
	var public struct {
		FooterLinks []FooterLink `json:"footer_links"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || !reflect.DeepEqual(public.FooterLinks, links) {
		t.Fatalf("public response: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", strings.NewReader(`{"footer_links":[]}`)))
	if w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if err := s.loadSiteSettings(); err != nil {
		t.Fatal(err)
	}
	if got := s.siteSettings().FooterLinks; got == nil || len(got) != 0 {
		t.Fatalf("empty config restored defaults: %+v", got)
	}
}

func TestFooterSettingsRejectInvalidWithoutPartialWrite(t *testing.T) {
	s := interactionServer(t)
	bad := []string{
		`null`, `"[]"`, `{}`, `[{}]`,
		`[{"id":"a","label":"bad","icon":"link","url":"javascript:alert(1)"}]`,
		`[{"id":"a","label":"bad","icon":"link","url":"//evil.example"}]`,
		`[{"id":"a","label":"bad","icon":"link","url":"/\\evil.example"}]`,
		`[{"id":"a","label":"bad","icon":"link","url":"https://user:pass@example.com"}]`,
		`[{"id":"a","label":"bad","icon":"admin","url":"/admin"}]`,
		`[{"id":"a","label":"bad","icon":"custom","url":"/blog","icon_url":"data:image/svg+xml,unsafe"}]`,
		`[{"id":"a","label":"bad","icon":"custom","url":"/blog"}]`,
		`[{"id":"a","label":"one","icon":"link","url":"/one"},{"id":"a","label":"two","icon":"link","url":"/two"}]`,
	}
	for _, value := range bad {
		t.Run(value, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", strings.NewReader(`{"site_name":"must-not-save","footer_links":`+value+`}`)))
			if w.Code != 400 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if name, _ := s.DB.GetSetting("site.name"); name != "" {
				t.Fatal("invalid footer partially changed site name")
			}
		})
	}
	items := make([]any, 13)
	if _, err := parseFooterLinks(items); err == nil {
		t.Fatal("accepted more than 12 icons")
	}
}
