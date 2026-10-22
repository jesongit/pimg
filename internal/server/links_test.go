package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestFileLinksAuthenticationAndExplicitTemporarySharing(t *testing.T) {
	s := interactionServer(t)
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "share.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("links-fixture"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "links-fixture", Name: "links fixture", TokenHash: fmt.Sprintf("%x", sum)}); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(method, suffix, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/admin/files/"+f.ID+suffix, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer links-fixture")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	counts := func() [2]int {
		t.Helper()
		var result [2]int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects").Scan(&result[0]); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&result[1]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := counts()
	if w := request("GET", "/links", "", false); w.Code != 401 {
		t.Fatal("links endpoint bypassed authentication", w.Code)
	}
	w := request("GET", "/links", "", true)
	var links store.AvailableLinks
	if err := json.Unmarshal(w.Body.Bytes(), &links); err != nil || w.Code != 200 || len(links.Items) != 1 || links.Items[0].Access != "api_token" || links.Items[0].Role != "archive" {
		t.Fatal("private archive unexpectedly has public links", w.Code, w.Body.String(), err)
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("signed link enumeration may be cached")
	}
	if after := counts(); after != before {
		t.Fatal("GET created temporary copies or scheduled background work", before, after)
	}
	w = request("POST", "/temp", `{"kind":"original"}`, true)
	var view store.TempViewResult
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || !strings.Contains(view.URL, "/content?") {
		t.Fatal("normal private preview changed", w.Code, w.Body.String(), err)
	}
	for _, body := range []string{`{"kind":"original","share":"true"}`, `{"kind":"original","share":1}`} {
		if w := request("POST", "/temp", body, true); w.Code != 400 {
			t.Fatal("a non-boolean sharing option was accepted", w.Code, w.Body.String())
		}
	}
	if after := counts(); after != before {
		t.Fatal("default preview or invalid sharing parameters created background work", before, after)
	}
	w = request("POST", "/temp", `{"kind":"original","share":true}`, true)
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || view.Status != "pending" {
		t.Fatal("explicit sharing did not queue a temporary copy", w.Code, w.Body.String(), err)
	}
	if err := s.Svc.RunTempMaterialize(context.Background(), f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	w = request("POST", "/temp", `{"kind":"original","share":true}`, true)
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || view.Status != "ready" || !strings.HasPrefix(view.URL, "/temporary/") || view.ExpiresAt <= 0 {
		t.Fatal("shareable temporary link missing", w.Code, w.Body.String(), err)
	}
	w = request("GET", "/links", "", true)
	if err := json.Unmarshal(w.Body.Bytes(), &links); err != nil || w.Code != 200 || len(links.Items) != 2 || links.Items[1].Role != "temporary" || strings.Contains(links.Items[0].URL, "/api/admin/") {
		t.Fatal("links endpoint did not enumerate the existing shareable copy", w.Code, w.Body.String(), err)
	}
	fresh, err := s.DB.GetFile(f.ID)
	if err != nil || fresh.Published || fresh.HotBackend != "" {
		t.Fatal("temporary sharing published the private archive", err)
	}
	if err := s.DB.SetPurgeState(f.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/links", "", true); w.Code != 409 {
		t.Fatal("deleting file still handed out links", w.Code, w.Body.String())
	}
}
