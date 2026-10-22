package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestFilePublicThumbnailGrantAndOverrideRevocation(t *testing.T) {
	s := policyServer(t)
	album, _ := s.DB.InsertAlbum("private")
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "private.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		return policyRequest(t, s, method, path, body)
	}
	public := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	change := func(value bool) {
		t.Helper()
		w := request("PATCH", "/api/admin/files/"+f.ID, fmt.Sprintf(`{"show_home":%t}`, value))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	change(true)
	w := public("/api/home")
	var home struct {
		Total int
		Items []map[string]any
	}
	if json.Unmarshal(w.Body.Bytes(), &home) != nil || home.Total != 1 {
		t.Fatal(w.Body.String())
	}
	thumb := home.Items[0]["thumb_url"].(string)
	for _, key := range []string{"url", "original_url", "video_url", "key_wrapped", "owner_id", "sha256"} {
		if _, ok := home.Items[0][key]; ok {
			t.Fatal("public metadata leak", key)
		}
	}
	if w = public(thumb); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(w.Code)
	}
	if w = public(thumb + "?kind=original"); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("not restricted to thumbnail")
	}
	if w = public(privateContentURL(f.ID, "original")); w.Code < 400 {
		t.Fatal("original exposed")
	}
	current, _ := s.DB.GetFile(f.ID)
	if current.Published || current.Storage == "direct" {
		t.Fatal("grant changed original")
	}
	if err = s.DB.SetAlbumDisplayed(album, true); err != nil {
		t.Fatal(err)
	}
	change(false)
	if w = public(thumb); w.Code != 404 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("file hide did not override album", w.Code)
	}
	if w = public("/api/home"); strings.Contains(w.Body.String(), f.ID) {
		t.Fatal("hidden file listed")
	}
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "show_home", "public-batch-test", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	w = request("POST", "/api/admin/batch", fmt.Sprintf(`{"action":"show_home","ids":[%q],"operation_id":%d}`, f.ID, op.ID))
	if w.Code != 200 || !s.DB.FileExhibited(f.ID) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = s.DB.SealTaskOperation(op.ID, dbx.AdminOwner); err != nil {
		t.Fatal(err)
	}
	current, _ = s.DB.GetFile(f.ID)
	if current.Published || current.SHA256 != f.SHA256 || current.OwnerID != f.OwnerID {
		t.Fatal("batch changed originals")
	}
}
