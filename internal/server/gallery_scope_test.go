package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestGalleryOwnerFiltersAndAccountPreviewBatchIsolation(t *testing.T) {
	s := interactionServer(t)
	owners := []string{dbx.AdminOwner, "user:alpha", "user:bravo"}
	albums := map[string]int64{}
	for _, owner := range owners {
		if owner != dbx.AdminOwner {
			s.DB.CreateAccount(owner, owner, "hash")
		}
		sum := sha256.Sum256([]byte(owner))
		scope := "account"
		if owner == dbx.AdminOwner {
			scope = "admin"
		}
		s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: scope, OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])})
		album, err := s.DB.EnsureOwnerDefaultAlbum(owner)
		if err != nil {
			t.Fatal(err)
		}
		albums[owner] = album
		err = s.DB.InsertFile(&dbx.File{ID: owner + "-file", OwnerID: owner, Name: owner, OrigName: "x.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 4, SHA256: "hash", AlbumID: sql.NullInt64{Int64: album, Valid: true}, ArchiveState: "done", KeyWrapped: []byte{}, KeyNonce: []byte{}})
		if err != nil {
			t.Fatal(err)
		}
	}
	s.DB.Exec(`UPDATE files SET has_thumb=1`)
	for _, v := range []struct {
		path, token string
		count       int
		owner       string
	}{
		{"/api/admin/files", dbx.AdminOwner, 1, dbx.AdminOwner},
		{"/api/admin/files?owner_id=user:alpha", dbx.AdminOwner, 1, "user:alpha"},
		{"/api/admin/files?owner_id=all", dbx.AdminOwner, 3, ""},
		{"/api/account/files?owner_id=all", "user:alpha", 1, "user:alpha"},
		{"/api/account/files?owner_id=user:bravo", "user:alpha", 1, "user:alpha"},
	} {
		w := uploaderCall(s, "GET", v.path, v.token, nil)
		var result struct {
			Total int
			Items []struct {
				Owner string `json:"owner_id"`
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != v.count {
			t.Fatal(v.path, w.Code, w.Body.String())
		}
		for _, f := range result.Items {
			if v.owner != "" && f.Owner != v.owner {
				t.Fatal("foreign file visible", v.path, f)
			}
		}
	}
	for _, v := range []struct{ path, token, owner string }{
		{"/api/admin/albums", dbx.AdminOwner, dbx.AdminOwner},
		{"/api/admin/albums?owner_id=user:bravo", dbx.AdminOwner, "user:bravo"},
		{"/api/account/albums?owner_id=user:bravo", "user:alpha", "user:alpha"},
	} {
		w := uploaderCall(s, "GET", v.path, v.token, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+jsonNumber(albums[v.owner])) {
			t.Fatal(v.path, w.Body.String())
		}
		for owner, id := range albums {
			if owner != v.owner && strings.Contains(w.Body.String(), `"id":`+jsonNumber(id)+",") {
				t.Fatal("foreign album visible", v.path, w.Body.String())
			}
		}
	}
	for _, v := range []struct {
		path, token string
		total       int
	}{
		{"/api/admin/files/timeline?group=day", dbx.AdminOwner, 1},
		{"/api/admin/files/timeline?group=day&owner_id=all", dbx.AdminOwner, 3},
		{"/api/account/files/timeline?group=day&owner_id=all", "user:alpha", 1},
	} {
		w := uploaderCall(s, "GET", v.path, v.token, nil)
		var p struct{ Groups []struct{ Count int } }
		json.Unmarshal(w.Body.Bytes(), &p)
		total := 0
		for _, g := range p.Groups {
			total += g.Count
		}
		if w.Code != 200 || total != v.total {
			t.Fatal("timeline scope", v.path, w.Body.String())
		}
	}
	ids := "admin:1-file,user:alpha-file,user:bravo-file"
	w := uploaderCall(s, "GET", "/api/account/files/previews?owner_id=all&ids="+ids, "user:alpha", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/api/account/files/user:alpha-file/content") || strings.Contains(w.Body.String(), "bravo") || strings.Contains(w.Body.String(), "admin:1-file") {
		t.Fatal("preview leak", w.Body.String())
	}
	w = uploaderCall(s, "GET", "/api/admin/files/previews?ids="+ids, dbx.AdminOwner, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "admin:1-file") || strings.Contains(w.Body.String(), "alpha") {
		t.Fatal("default preview owner", w.Body.String())
	}
	for _, body := range []map[string]any{
		{"action": "move_album", "ids": []string{"user:alpha-file", "user:bravo-file"}, "album_id": albums["user:alpha"]},
		{"action": "move_album", "ids": []string{"user:alpha-file"}, "album_id": albums["user:bravo"]},
	} {
		if w := uploaderCall(s, "POST", "/api/account/batch", "user:alpha", body); w.Code != 404 {
			t.Fatal("foreign batch accepted", w.Code, w.Body.String())
		}
	}
	if w := uploaderCall(s, "POST", "/api/account/batch", "user:alpha", map[string]any{"action": "show_home", "ids": []string{"user:alpha-file"}}); w.Code != 403 {
		t.Fatal("home mutation authorized", w.Code)
	}
	destination, _ := s.DB.InsertOwnerAlbum("user:alpha", "own destination")
	w = uploaderCall(s, "POST", "/api/account/batch", "user:alpha", map[string]any{"action": "move_album", "ids": []string{"user:alpha-file"}, "album_id": destination})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f, _ := s.DB.GetFile("user:alpha-file")
	if f.AlbumID.Int64 != destination {
		t.Fatal("own batch move failed", w.Body.String())
	}
	if w := uploaderCall(s, "GET", "/api/admin/files?owner_id=missing", dbx.AdminOwner, nil); w.Code != 400 {
		t.Fatal("unknown owner broadened access", w.Code)
	}
}
