package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExhibitionGrantsOnlyThumbnailsForEncryptedAndUnpublishedFiles(t *testing.T) {
	s := interactionServer(t)
	album, err := s.DB.InsertAlbum("exhibition")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "private.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if err = s.DB.SetAlbumDisplayed(album, true); err != nil {
		t.Fatal(err)
	}
	w := call("/api/home")
	var home struct {
		Items []map[string]any
		Total int
	}
	if err = json.Unmarshal(w.Body.Bytes(), &home); err != nil || home.Total != 1 {
		t.Fatal(w.Body.String(), err)
	}
	item := home.Items[0]
	if _, ok := item["url"]; ok {
		t.Fatal("original URL exposed")
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("home cacheable")
	}
	thumb, ok := item["thumb_url"].(string)
	if !ok || !strings.HasPrefix(thumb, "/api/exhibit/files/") {
		t.Fatal(item)
	}
	w = call(thumb)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || !bytes.HasPrefix(w.Body.Bytes(), []byte{0xff, 0xd8}) {
		t.Fatal(w.Code, w.Header())
	}
	if w = call(thumb + "?kind=original"); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("query selected original", w.Code)
	}
	for _, path := range []string{privateContentURL(f.ID, "original"), privateContentURL(f.ID, "thumb"), "/previews/" + f.ID} {
		if w = call(path); w.Code < 400 {
			t.Fatal("private source exposed", path, w.Code)
		}
	}
	current, _ := s.DB.GetFile(f.ID)
	if current.Published || current.Storage == "direct" {
		t.Fatal("display modified encryption/publication")
	}
	if err = s.DB.SetAlbumDisplayed(album, false); err != nil {
		t.Fatal(err)
	}
	if w = call(thumb); w.Code != 404 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("revocation ignored", w.Code)
	}
	if w = call("/api/home"); strings.Contains(w.Body.String(), f.ID) {
		t.Fatal("disabled album still listed")
	}
}

func TestExhibitionManagementRequiresAdministrator(t *testing.T) {
	s := interactionServer(t)
	a, _ := s.DB.InsertAlbum("private")
	for _, path := range []string{"/api/admin/albums", "/api/account/albums"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PATCH", "/api/admin/albums/1", strings.NewReader(`{"displayed":true}`))
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unauthorized display change", w.Code)
	}
	if album, _ := s.DB.GetAlbum(a); album.Displayed {
		t.Fatal("unauthorized mutation")
	}
}
