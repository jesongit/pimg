package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"pimg/internal/dbx"
)

// Admin galleries use admin authentication while preserving the selected user's
// file ownership. Ordinary accounts cannot gain this authority via owner_id.
func TestAdminManagesOtherOwnerGalleryAndKeepsAccountIsolation(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{dbx.AdminOwner, "user:managed", "user:other"} {
		if owner != dbx.AdminOwner {
			if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
				t.Fatal(err)
			}
		}
		scope := "account"
		if owner == dbx.AdminOwner {
			scope = "admin"
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, OwnerID: owner, Scope: scope, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
	album, err := s.DB.InsertOwnerAlbum("user:managed", "Managed album")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := s.DB.InsertOwnerAlbum("user:managed", "Destination")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.DB.InsertOwnerAlbum("user:other", "Other album")
	if err != nil {
		t.Fatal(err)
	}
	f := &dbx.File{ID: "managed-photo", OwnerID: "user:managed", Name: "Original name", OrigName: "kept.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 123, SHA256: "kept-hash", AlbumID: sql.NullInt64{Int64: album, Valid: true}, ArchiveState: "done", Storage: "direct", ColdBackend: "local", KeyWrapped: []byte{1}, KeyNonce: []byte{2}}
	if err = s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any, want int) {
		t.Helper()
		r := uploaderCall(s, method, path, dbx.AdminOwner, body)
		if r.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, r.Code, r.Body.String())
		}
	}
	call("PATCH", "/api/admin/files/managed-photo?owner_id=user:managed", map[string]any{"name": "Renamed by admin"}, 200)
	call("PATCH", "/api/admin/albums/"+jsonNumber(album)+"?owner_id=user:managed", map[string]any{"name": "Renamed album"}, 200)
	// Exercise the same operation -> batch -> seal flow used by the gallery UI.
	for i, change := range []map[string]any{
		{"action": "set_media_time", "media_at": int64(1709251199)},
		{"action": "move_album", "album_id": destination},
		{"action": "publish"},
	} {
		request := map[string]any{"key": fmt.Sprint("managed-", i), "kind": change["action"], "total_files": 1}
		for _, key := range []string{"media_at", "album_id"} {
			if value, ok := change[key]; ok {
				request[key] = value
			}
		}
		r := uploaderCall(s, "POST", "/api/admin/operations?owner_id=user:managed", dbx.AdminOwner, request)
		var op struct {
			ID int64 `json:"operation_id"`
		}
		if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &op) != nil || op.ID == 0 {
			t.Fatal(r.Code, r.Body.String())
		}
		change["ids"] = []string{f.ID}
		change["operation_id"] = op.ID
		r = uploaderCall(s, "POST", "/api/admin/batch?owner_id=user:managed", dbx.AdminOwner, change)
		var result struct{ Done, Skipped int }
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Done != 1 || result.Skipped != 0 {
			t.Fatal(r.Code, r.Body.String())
		}
		call("POST", fmt.Sprintf("/api/admin/operations/%d/seal?owner_id=user:managed", op.ID), nil, 200)
	}
	got, err := s.DB.GetFile(f.ID)
	if err != nil || got.OwnerID != f.OwnerID || got.AlbumID.Int64 != destination || got.Name != "Renamed by admin" || got.MediaAt != 1709251199 || got.OrigName != f.OrigName || got.Size != f.Size || got.SHA256 != f.SHA256 {
		t.Fatal("admin management changed owner/original or did not apply", got, err)
	}
	if a, err := s.DB.GetAlbum(album); err != nil || a.Name != "Renamed album" || a.OwnerID != f.OwnerID {
		t.Fatal(a, err)
	}
	// Even an administrator must not move a file into another user's album.
	r := uploaderCall(s, "POST", "/api/admin/batch?owner_id=user:managed", dbx.AdminOwner, map[string]any{"action": "move_album", "album_id": foreign, "ids": []string{f.ID}})
	var denied struct{ Done, Skipped int }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &denied) != nil || denied.Done != 0 || denied.Skipped != 1 {
		t.Fatal("cross-owner move accepted", r.Code, r.Body.String())
	}
	// An ordinary user's forged owner query cannot reach admin routes or mutate
	// somebody else's files/albums through account routes.
	for _, attempt := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/admin/files/managed-photo?owner_id=user:managed", map[string]any{"name": "stolen"}},
		{"PATCH", "/api/account/files/managed-photo?owner_id=user:managed", map[string]any{"name": "stolen"}},
		{"PATCH", "/api/account/albums/" + jsonNumber(album) + "?owner_id=user:managed", map[string]any{"name": "stolen"}},
		{"POST", "/api/account/batch?owner_id=user:managed", map[string]any{"action": "set_media_time", "ids": []string{f.ID}, "media_at": 0}},
		{"POST", "/api/account/batch?owner_id=user:managed", map[string]any{"action": "purge", "confirm": "PURGE", "ids": []string{f.ID}}},
	} {
		r = uploaderCall(s, attempt.method, attempt.path, "user:other", attempt.body)
		if r.Code != 401 && r.Code != 404 {
			t.Fatal("ordinary account override accepted", attempt.path, r.Code, r.Body.String())
		}
	}
	got, err = s.DB.GetFile(f.ID)
	if err != nil || got.Name != "Renamed by admin" || got.AlbumID.Int64 != destination || got.OwnerID != f.OwnerID || got.PurgeState.Valid {
		t.Fatal("rejected requests changed the file", got, err)
	}
	// A queued purge must still require the existing explicit confirmation.
	call("DELETE", "/api/admin/files/managed-photo?owner_id=user:managed", nil, 400)
	call("DELETE", "/api/admin/files/managed-photo?owner_id=user:managed&confirm=PURGE", nil, 200)
	got, err = s.DB.GetFile(f.ID)
	if err != nil || got.PurgeState.String != "pending" || got.OwnerID != f.OwnerID || got.SHA256 != f.SHA256 {
		t.Fatal("admin purge not queued safely", got, err)
	}
}
