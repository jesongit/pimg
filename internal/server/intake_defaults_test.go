package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"pimg/internal/dbx"
	"strings"
	"testing"
)

func TestUploadDefaultsPlainPrivateAndUnclassified(t *testing.T) {
	s := importServer(t)
	f := uploadPolicyFile(t, s, "")
	archiveImportFile(t, s, f)
	f, err := s.DB.GetFile(f.ID)
	defaultAlbum, _ := s.DB.EnsureOwnerDefaultAlbum(dbx.AdminOwner)
	if err != nil || f.Storage != "direct" || f.Published || !f.AlbumID.Valid || f.AlbumID.Int64 != defaultAlbum {
		t.Fatal("unexpected upload defaults", f, err)
	}
}

func TestLocalImportDefaultsPlainPrivateAndUnclassified(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	if err := os.WriteFile(filepath.Join(root, "plain.png"), interactionPNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	w := carrierRequest(t, s)("POST", "/api/admin/local-imports", strings.NewReader(`{"paths":["plain.png"],"recursive":true,"schedule":"now"}`), "application/json")
	var body struct {
		ID int64 `json:"id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.ID == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	b, err := s.Svc.LocalImportBatch(body.ID)
	if err != nil || b.AlbumID != 0 || b.Encrypted {
		t.Fatal("unexpected import defaults", b, err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), body.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(body.ID, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, body.ID)
	items, err := s.Svc.LocalImportItems(body.ID, 0)
	if err != nil || len(items) != 1 || items[0].State != "done" || len(items[0].FileIDs) != 1 {
		t.Fatal(items, err)
	}
	f, err := s.DB.GetFile(items[0].FileIDs[0])
	defaultAlbum, _ := s.DB.EnsureOwnerDefaultAlbum(dbx.AdminOwner)
	if err != nil || f.Storage != "direct" || !f.AlbumID.Valid || f.AlbumID.Int64 != defaultAlbum || f.Published {
		t.Fatal("wrong imported file defaults", f, err)
	}
}
