package store

import (
	"context"
	"os"
	"path/filepath"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"strings"
	"testing"
)

func legacyArchiveFixture(t *testing.T) (*Service, *dbx.File, storage.LocalCold, string, string) {
	t.Helper()
	s, _, data := peerFixture(t)
	f := archiveMedia(t, s, data)
	instance, err := s.instance("local")
	if err != nil {
		t.Fatal(err)
	}
	local := instance.cold.(storage.LocalCold)
	chunks, err := s.DB.ListChunks(f.ID, "data")
	if err != nil || len(chunks) != 1 {
		t.Fatal(chunks, err)
	}
	current := chunks[0].ObjectKey
	legacy := strings.TrimPrefix(current, "accounts/"+dbx.OwnerDirectory(f.OwnerID)+"/")
	if err = datalayout.CopyTree(filepath.Join(local.Disk.Dir, filepath.FromSlash(current)), filepath.Join(local.Disk.Dir, filepath.FromSlash(legacy))); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(local.Disk.Dir, filepath.FromSlash(current))); err != nil {
		t.Fatal(err)
	}
	objects, err := s.DB.ArchiveObjects(f.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		ref := objectReference(object)
		ref.Key = legacy
		object.Reference = referenceJSON(ref)
		if err = s.DB.SaveArchiveObject(object); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec(`UPDATE file_chunks SET object_key=? WHERE file_id=? AND kind='data'`, legacy, f.ID); err != nil {
		t.Fatal(err)
	}
	return s, f, local, legacy, current
}

func TestLocalAccountDirectoryMigrationVerifiesAndRetainsOldReference(t *testing.T) {
	s, f, local, legacy, current := legacyArchiveFixture(t)
	if err := s.MigrateLocalArchiveFile(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	if chunks[0].ObjectKey != current {
		t.Fatal(chunks[0].ObjectKey)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("migrated original failed recovery", err)
	}
	if _, err := os.Stat(filepath.Join(local.Disk.Dir, filepath.FromSlash(legacy))); err != nil {
		t.Fatal("old copy removed before cleanup", err)
	}
	var garbage int
	s.DB.QueryRow(`SELECT COUNT(*) FROM archive_garbage WHERE file_id=?`, f.ID).Scan(&garbage)
	if garbage != 1 {
		t.Fatal("old locator untracked", garbage)
	}
	if err := s.MigrateLocalArchiveFile(context.Background(), f.ID); err != nil {
		t.Fatal("retry was not idempotent", err)
	}
}

func TestLocalAccountMigrationConflictKeepsOriginalLocator(t *testing.T) {
	s, f, local, legacy, current := legacyArchiveFixture(t)
	target := filepath.Join(local.Disk.Dir, filepath.FromSlash(current))
	os.MkdirAll(filepath.Dir(target), 0700)
	os.WriteFile(target, []byte("conflicting-data"), 0600)
	if err := s.MigrateLocalArchiveFile(context.Background(), f.ID); err == nil {
		t.Fatal("conflicting destination overwritten")
	}
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	if chunks[0].ObjectKey != legacy {
		t.Fatal("locator switched on failure")
	}
	bytes, _ := os.ReadFile(target)
	if string(bytes) != "conflicting-data" {
		t.Fatal("conflicting object changed")
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("old original unavailable", err)
	}
}

func TestAccountSourceImportUsesTargetAlbumOwner(t *testing.T) {
	s, instanceID, data := peerFixture(t)
	s.DB.CreateAccount("user:alpha", "alpha", "hash")
	album, err := s.DB.InsertOwnerAlbum("user:alpha", "相册")
	if err != nil {
		t.Fatal(err)
	}
	var root string
	for _, instance := range s.StorageInstances() {
		if instance.ID == instanceID {
			root = instance.Config.Directory
		}
	}
	if root == "" {
		t.Fatal("source root missing")
	}
	os.MkdirAll(filepath.Join(root, "incoming"), 0700)
	if err = os.WriteFile(filepath.Join(root, "incoming", "photo.png"), data, 0600); err != nil {
		t.Fatal(err)
	}
	rule, err := s.SaveImportRule(dbx.ImportRule{Name: "账号来源", InstanceID: instanceID, Source: "incoming", AlbumID: album, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunSourceScan(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	files, total, err := s.DB.ListFiles(dbx.FileFilter{OwnerID: "user:alpha", Page: 1, PerPage: 10})
	if err != nil || total != 1 || files[0].AlbumID.Int64 != album {
		t.Fatal("source imported into wrong account", files, total, err)
	}
}
