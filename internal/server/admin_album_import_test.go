package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func albumImportAccounts(t *testing.T, s *Server) {
	t.Helper()
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdminCreatesAlbumForUserWithoutGrantingAdminAccess(t *testing.T) {
	s := importServer(t)
	albumImportAccounts(t, s)
	request := carrierRequest(t, s)
	w := request("POST", "/api/admin/albums", strings.NewReader(`{"name":"代建相册","owner_id":"user:alpha"}`), "application/json")
	var created struct{ ID int64 }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	a, err := s.DB.OwnedAlbum("user:alpha", created.ID)
	if err != nil || a.Name != "代建相册" {
		t.Fatal(a, err)
	}
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		w := uploaderCall(s, "GET", "/api/account/albums", owner, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "代建相册") != (owner == "user:alpha") {
			t.Fatal("album scope", owner, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{
		`{"name":"invalid","owner_id":"missing"}`,
		`{"name":"invalid","owner_id":"all"}`,
		`{"name":"invalid","owner_id":"user:alpha","topic_id":20}`,
	} {
		if w := request("POST", "/api/admin/albums", strings.NewReader(body), "application/json"); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := uploaderCall(s, "POST", "/api/account/albums", "user:alpha", map[string]any{"name": "spoof", "owner_id": "user:bravo"}); w.Code != 400 {
		t.Fatal("user assigned a foreign owner", w.Code, w.Body.String())
	}
	if w := uploaderCall(s, "PATCH", "/api/account/albums/"+jsonNumber(created.ID), "user:alpha", map[string]any{"name": "spoof", "owner_id": "user:bravo"}); w.Code != 400 {
		t.Fatal("user changed album ownership", w.Code)
	}
	if w := uploaderCall(s, "POST", "/api/account/albums?owner_id=user:bravo", "user:alpha", map[string]any{"name": "自己的相册"}); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/admin/albums"}, {"GET", "/api/admin/accounts"},
		{"GET", "/api/admin/local-imports"}, {"GET", "/api/admin/local-imports/browse"},
		{"POST", "/api/admin/local-imports"},
	} {
		if w := uploaderCall(s, route.method, route.path, "user:alpha", nil); w.Code != 401 {
			t.Fatal("ordinary account gained administration", route, w.Code)
		}
	}
	if w := uploaderCall(s, "GET", "/api/account/local-imports", "user:alpha", nil); w.Code != 404 {
		t.Fatal("unwanted account import API remains", w.Code)
	}
	disabled := true
	if err = s.DB.UpdateManagedAccount("user:bravo", nil, &disabled, ""); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "/api/admin/albums", strings.NewReader(`{"name":"disabled","owner_id":"user:bravo"}`), "application/json"); w.Code != 400 {
		t.Fatal("disabled target accepted", w.Code)
	}
}

func TestAdminImportUsesTargetAlbumOwnerAcrossRestartAndDedup(t *testing.T) {
	s := importServer(t)
	albumImportAccounts(t, s)
	request := carrierRequest(t, s)
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	source := filepath.Join(root, "photo.png")
	original := interactionPNG(t)
	if err := os.WriteFile(source, original, 0400); err != nil {
		t.Fatal(err)
	}
	albums := map[string]int64{}
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		id, err := s.DB.InsertOwnerAlbum(owner, owner+"相册")
		if err != nil {
			t.Fatal(err)
		}
		albums[owner] = id
	}
	start := func(album int64) int64 {
		t.Helper()
		body := dbx.JSONValue(map[string]any{"paths": []string{source}, "album_id": album, "schedule": "now"})
		w := request("POST", "/api/admin/local-imports", strings.NewReader(body), "application/json")
		var result struct{ ID int64 }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := s.Svc.RunLocalImportScan(t.Context(), result.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Svc.ChangeLocalImport(result.ID, "start"); err != nil {
			t.Fatal(err)
		}
		return result.ID
	}
	id := start(albums["user:alpha"])
	if err := s.Svc.ChangeLocalImport(id, "pause"); err != nil {
		t.Fatal(err)
	}
	// Reopen the persisted database, retaining the old session and batch state.
	var seq int
	var name, path string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	reopened, err := dbx.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s.DB, s.Svc.DB = reopened, reopened
	b, err := s.Svc.LocalImportBatch(id)
	if err != nil || b.OwnerID != "user:alpha" || b.State != "paused" {
		t.Fatal("batch identity lost", b, err)
	}
	if err = s.Svc.ChangeLocalImport(id, "resume"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	items, err := s.Svc.LocalImportItems(id, 0)
	if err != nil || len(items) != 1 || len(items[0].FileIDs) != 1 {
		t.Fatal(items, err)
	}
	f, err := s.DB.GetFile(items[0].FileIDs[0])
	if err != nil || f.OwnerID != "user:alpha" || f.AlbumID.Int64 != albums["user:alpha"] || f.Published {
		t.Fatal("import assigned to administrator or published", f, err)
	}
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		w := uploaderCall(s, "GET", "/api/account/files/"+f.ID+"/download", owner, nil)
		if owner == "user:alpha" && (w.Code != 200 || !bytes.Equal(w.Body.Bytes(), original)) || owner == "user:bravo" && w.Code != 404 {
			t.Fatal("imported original isolation", owner, w.Code)
		}
	}
	otherAlbum, _ := s.DB.InsertOwnerAlbum("user:alpha", "另一个相册")
	duplicate := start(otherAlbum)
	finishImport(t, s, duplicate)
	dup, err := s.Svc.LocalImportItems(duplicate, 0)
	if err != nil || len(dup) != 1 || dup[0].State != "duplicate" || dup[0].ExistingAlbum != "user:alpha相册" {
		t.Fatal("owner-scoped duplicate receipt", dup, err)
	}
	bravo := start(albums["user:bravo"])
	finishImport(t, s, bravo)
	var total int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE owner_id IN ('user:alpha','user:bravo')`).Scan(&total); err != nil || total != 2 {
		t.Fatal("cross-owner dedup reused private media", total, err)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("server source changed", err)
	}
	// A legacy batch without an owner field remains administrator-owned.
	legacy, err := s.Svc.CreateLocalImport([]string{source}, 0, false, false, "now")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE local_import_batches SET context=json_remove(context,'$.owner_id') WHERE id=?`, legacy); err != nil {
		t.Fatal(err)
	}
	if owner, err := s.DB.LocalImportOwner(legacy); err != nil || owner != dbx.AdminOwner {
		t.Fatal("legacy batch ownership changed", owner, err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(legacy, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, legacy)
	blocked := start(otherAlbum)
	disabled := true
	if err = s.DB.UpdateManagedAccount("user:alpha", nil, &disabled, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImport(t.Context(), blocked); err == nil {
		t.Fatal("import continued into a disabled account")
	}
	if b, err := s.Svc.LocalImportBatch(blocked); err != nil || b.State != "failed" {
		t.Fatal("disabled target left import permanently running", b, err)
	}
	var settings int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM settings WHERE k LIKE 'auth.account_role.%'`).Scan(&settings); err != nil || settings != 0 {
		t.Fatal("unwanted roles retained", settings, err)
	}
}
