package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestAccountGalleryAlbumMutationsAndScopedMedia(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		s.DB.CreateAccount(owner, owner, "hash")
		sum := sha256.Sum256([]byte(owner))
		s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])})
	}
	alpha, _ := s.DB.InsertOwnerAlbum("user:alpha", "A")
	bravo, _ := s.DB.InsertOwnerAlbum("user:bravo", "B")
	def, _ := s.DB.EnsureOwnerDefaultAlbum("user:alpha")
	for _, method := range []string{"PATCH", "DELETE"} {
		for _, id := range []int64{bravo, 999999} {
			w := uploaderCall(s, method, "/api/account/albums/"+jsonNumber(id), "user:alpha", map[string]any{"name": "stolen"})
			if w.Code != 404 {
				t.Fatal(method, id, w.Code, w.Body.String())
			}
		}
		w := uploaderCall(s, method, "/api/account/albums/"+jsonNumber(def), "user:alpha", map[string]any{"name": "default"})
		if w.Code != 400 {
			t.Fatal("fixed album changed", w.Code)
		}
	}
	if w := uploaderCall(s, "PATCH", "/api/account/albums/"+jsonNumber(alpha), "user:alpha", map[string]any{"name": " renamed "}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a, _ := s.DB.GetAlbum(alpha)
	if a.Name != "renamed" {
		t.Fatal(a)
	}
	if err := s.DB.InsertFile(&dbx.File{ID: "alpha-media", OwnerID: "user:alpha", Name: "A", OrigName: "a.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 4, AlbumID: sql.NullInt64{Int64: alpha, Valid: true}, SHA256: "hash", HasThumb: true, ArchiveState: "done", KeyWrapped: []byte{}, KeyNonce: []byte{}}); err != nil {
		t.Fatal(err)
	}
	if w := uploaderCall(s, "DELETE", "/api/account/albums/"+jsonNumber(alpha), "user:alpha", nil); w.Code != 409 {
		t.Fatal("nonempty album deleted", w.Code)
	}
	s.DB.Exec(`UPDATE files SET has_thumb=1 WHERE id='alpha-media'`)
	for _, path := range []string{"/api/account/files/alpha-media", "/api/account/files"} {
		w := uploaderCall(s, "GET", path, "user:alpha", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "/api/account/files/alpha-media/content") || strings.Contains(w.Body.String(), "/api/admin/files/") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := uploaderCall(s, "POST", "/api/account/files/alpha-media/temp", "user:alpha", map[string]any{"kind": "thumb"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/api/account/files/alpha-media/content") {
		t.Fatal(w.Code, w.Body.String())
	}
	s.DB.Exec(`DELETE FROM files WHERE id='alpha-media'`)
	if w := uploaderCall(s, "DELETE", "/api/account/albums/"+jsonNumber(alpha), "user:alpha", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, e := s.DB.GetAlbum(alpha); e != dbx.ErrNotFound {
		t.Fatal("album retained", e)
	}
	if a, e := s.DB.GetAlbum(bravo); e != nil || a.Name != "B" {
		t.Fatal("foreign album changed", a, e)
	}
	// A request from an old tab cannot silently use a replacement login cookie.
	req := httptest.NewRequest("GET", "/api/account/files", nil)
	req.Header.Set("Authorization", "Bearer user:alpha")
	req.Header.Set("X-Pimg-Owner", "user:bravo")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatal("stale account page remained authorized", res.Code)
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
