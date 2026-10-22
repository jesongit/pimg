package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"testing"
	"time"
)

func tinyMedia(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 16))); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func peerFixture(t *testing.T) (*Service, string, []byte) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	if e := s.SetStorage("local", "local"); e != nil {
		t.Fatal(e)
	}
	id, e := s.SaveStorageInstance("", "第二归档", "local", InstanceConfig{Directory: t.TempDir()}, true)
	if e != nil {
		t.Fatal(e)
	}
	p := s.StoragePolicy()
	p.Archives = []string{"local", id, "r2"}
	p.NormalMB = 1024
	p.AcceleratedMB = 1024
	p.SyncCacheMB = 64
	if e = s.SaveStoragePolicy(p); e != nil {
		t.Fatal(e)
	}
	return s, id, tinyMedia(t)
}
func archiveMedia(t *testing.T, s *Service, b []byte) *dbx.File {
	t.Helper()
	f, e := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(b), "image.png", "png", sql.NullInt64{}, nil, 0, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RunArchive(context.Background(), f.ID); e != nil {
		t.Fatal(e)
	}
	f, e = s.DB.GetFile(f.ID)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func TestEqualArchiveCopiesAndCorruptionFailover(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	ctx := context.Background()
	if e := s.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal(e)
	}
	for _, target := range []string{"local", peer, "r2"} {
		rc, e := s.SourceStream(context.WithValue(ctx, archiveSourceContext{}, target), f)
		if e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(rc)
		rc.Close()
		if e != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%s restored wrong bytes: %v", target, e)
		}
	}
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	disk := s.coldBackends["local"].(storage.LocalCold).Disk
	if e := os.WriteFile(filepath.Join(disk.Dir, filepath.FromSlash(chunks[0].ObjectKey)), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	os.Remove(s.originalCachePath(f))
	original, e := s.ViewOriginal(ctx, f)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(original)
	original.Close()
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("original failover did not verify full bytes", e)
	}
	if _, e = s.SaveStorageInstance(peer, "moved", "local", InstanceConfig{Directory: t.TempDir()}, true); e == nil {
		t.Fatal("mutable locator accepted while referenced")
	}
}

type ambiguousArchive struct {
	storage.LocalCold
	fail   bool
	writes int
}

func TestArchiveSyncRetriesCorruptPreferredSource(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	ctx := context.Background()
	if e := s.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal(e)
	}
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	disk := s.coldBackends["local"].(storage.LocalCold).Disk
	if e := os.WriteFile(filepath.Join(disk.Dir, filepath.FromSlash(chunks[0].ObjectKey)), []byte("damaged"), 0600); e != nil {
		t.Fatal(e)
	}
	id, e := s.SaveStorageInstance("", "新增归档", "local", InstanceConfig{Directory: t.TempDir()}, true)
	if e != nil {
		t.Fatal(e)
	}
	p := s.StoragePolicy()
	p.Archives = append(p.Archives, id)
	if e = s.SaveStoragePolicy(p); e != nil {
		t.Fatal(e)
	}
	s.storages.mu.Lock()
	s.storages.scores = map[string]sourceScore{}
	s.storages.mu.Unlock()
	if e = s.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal("healthy peer must replace corrupt source", e)
	}
	rc, e := s.SourceStream(context.WithValue(ctx, archiveSourceContext{}, id), f)
	if e != nil {
		t.Fatal(e)
	}
	defer rc.Close()
	got, e := io.ReadAll(rc)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("replica bytes differ after source failover", e)
	}
}

func (b *ambiguousArchive) PutReader(ctx context.Context, k string, r io.Reader, n int64, c string) (storage.Reference, error) {
	b.writes++
	ref, e := b.LocalCold.PutReader(ctx, k, r, n, c)
	if e == nil && b.fail {
		b.fail = false
		return ref, errors.New("lost acknowledgment")
	}
	return ref, e
}
func TestArchiveSyncLostAcknowledgmentAndRestart(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	runtime, _ := s.instance(peer)
	driver := &ambiguousArchive{LocalCold: runtime.cold.(storage.LocalCold), fail: true}
	runtime.cold = driver
	if e := s.RunArchiveSync(context.Background(), f.ID); e == nil {
		t.Fatal("fault did not fire")
	}
	objects, _ := s.DB.ArchiveObjects(f.ID, peer)
	if len(objects) != 1 || objects[0].Verified || objectReference(objects[0]).Key == "" {
		t.Fatal("ambiguous write lost its cleanup locator", objects)
	}
	// Reconstruct runtime state, retaining only SQLite and the same key/config.
	restored, e := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if e != nil {
		t.Fatal(e)
	}
	if e = restored.RunArchiveSync(context.Background(), f.ID); e != nil {
		t.Fatal(e)
	}
	if e = restored.RunArchiveSync(context.Background(), f.ID); e != nil {
		t.Fatal(e)
	}
	var manifests int
	if e = s.DB.QueryRow(`SELECT COUNT(*) FROM archive_manifests WHERE file_id=?`, f.ID).Scan(&manifests); e != nil || manifests != 3 {
		t.Fatal("incomplete durable replicas", manifests, e)
	}
	entries, _ := os.ReadDir(s.ctDir)
	for _, v := range entries {
		if filepath.Ext(v.Name()) == ".part" {
			t.Fatal("sync spool leaked")
		}
	}
}
func TestImportOrganizesOnlyAfterVerifiedArchiveAndPreservesSourceChanges(t *testing.T) {
	s, _, plain := peerFixture(t)
	root := t.TempDir()
	source, e := s.SaveStorageInstance("", "待整理", "local", InstanceConfig{Directory: root}, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(root, "inbox"), 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "inbox", "one.png")
	if e = os.WriteFile(path, plain, 0600); e != nil {
		t.Fatal(e)
	}
	album, e := s.DB.EnsureDefaultAlbum()
	if e != nil {
		t.Fatal(e)
	}
	rule, e := s.SaveImportRule(dbx.ImportRule{Encrypted: true, Name: "来源整理", InstanceID: source, Source: "inbox", AlbumID: album, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RunSourceScan(context.Background(), rule); e != nil {
		t.Fatal(e)
	}
	var file string
	if e = s.DB.QueryRow(`SELECT file_id FROM import_receipts WHERE rule_id=? AND state='archiving'`, rule).Scan(&file); e != nil {
		t.Fatal(e)
	}
	if e = s.RunSourceCleanup(context.Background(), file); e == nil {
		t.Fatal("source deleted before archive was ready")
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("source lost before verification")
	}
	if e = s.RunArchive(context.Background(), file); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, append(append([]byte{}, plain...), 1), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.RunSourceCleanup(context.Background(), file); e == nil {
		t.Fatal("changed source removed")
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("changed source was lost")
	}
	// Restore the exact scanned file version for successful cleanup.
	var entryRaw string
	_ = s.DB.QueryRow(`SELECT entry FROM import_receipts WHERE rule_id=?`, rule).Scan(&entryRaw)
	var entry SourceEntry
	_ = json.Unmarshal([]byte(entryRaw), &entry)
	var n, stamp int64
	_, _ = fmt.Sscanf(entry.Version, "%d:%d", &n, &stamp)
	_ = os.WriteFile(path, plain, 0600)
	_ = os.Chtimes(path, time.Unix(0, stamp), time.Unix(0, stamp))
	if e = s.RunSourceCleanup(context.Background(), file); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("source was not organized", e)
	}
	if e = s.RunSourceScan(context.Background(), rule); e != nil {
		t.Fatal(e)
	}
	var count int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM import_receipts WHERE rule_id=?`, rule).Scan(&count)
	if count != 1 {
		t.Fatal("receipt replay duplicated source", count)
	}
	f, _ := s.DB.GetFile(file)
	if f.Published || f.Storage == "direct" || f.AlbumID.Int64 != album {
		t.Fatal("source import changed privacy or target album")
	}
}
func TestStorageConfigSecretsAndIndependentTemporaryRole(t *testing.T) {
	s, _, plain := peerFixture(t)
	id, e := s.SaveStorageInstance("", "私有 R2", "r2", InstanceConfig{Endpoint: "https://account.example.com", Bucket: "test", AccessKey: "ACCESS-PRIVATE", SecretKey: "SECRET-PRIVATE", Prefix: "one"}, true)
	if e != nil {
		t.Fatal(e)
	}
	var encrypted []byte
	_ = s.DB.QueryRow(`SELECT config FROM storage_instances WHERE id=?`, id).Scan(&encrypted)
	if bytes.Contains(encrypted, []byte("SECRET-PRIVATE")) {
		t.Fatal("storage credentials stored in plaintext")
	}
	views, _ := json.Marshal(s.StorageInstances())
	if bytes.Contains(views, []byte("SECRET-PRIVATE")) || bytes.Contains(views, []byte("ACCESS-PRIVATE")) {
		t.Fatal("credentials leaked through settings")
	}
	p := s.StoragePolicy()
	p.Permanent = "r2"
	p.Temporary = "local"
	if e = s.SaveStoragePolicy(p); e != nil {
		t.Fatal(e)
	}
	f := archiveMedia(t, s, plain)
	if e = s.RunTempMaterialize(context.Background(), f.ID, "original"); e != nil {
		t.Fatal(e)
	}
	obj, e := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	if e != nil || obj.Backend != "local" {
		t.Fatal("temp role was coupled to publication", e)
	}
	_, _ = s.DB.Exec(`UPDATE temp_objects SET expires_at=0 WHERE id=?`, obj.ID)
	restored, e := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if e != nil {
		t.Fatal(e)
	}
	if n, e := restored.CleanupExpiredTemp(context.Background()); e != nil || n != 1 {
		t.Fatal("restart did not catch up expiry", n, e)
	}
	if _, e = s.Verify(context.Background(), f.ID); e != nil {
		t.Fatal("expiry damaged archive", e)
	}
}

func TestArchiveCopiesFollowAlbumMoveWithoutLeavingOldObjects(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	ctx := context.Background()
	if e := s.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal(e)
	}
	old, _ := s.DB.ArchiveObjects(f.ID, peer)
	album, e := s.DB.InsertAlbum("整理后的目录")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DB.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: album, Valid: true}, nil); e != nil {
		t.Fatal(e)
	}
	if e = s.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal(e)
	}
	fresh, _ := s.DB.GetFile(f.ID)
	for _, id := range s.StoragePolicy().Archives {
		objects, e := s.DB.ArchiveObjects(f.ID, id)
		if e != nil {
			t.Fatal(e)
		}
		for _, o := range objects {
			if o.AlbumID != album {
				t.Fatal("copy did not follow destination album")
			}
		}
		rc, e := s.SourceStream(context.WithValue(ctx, archiveSourceContext{}, id), fresh)
		if e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(rc)
		rc.Close()
		if e != nil || !bytes.Equal(got, plain) {
			t.Fatal("move damaged original", e)
		}
	}
	b, _ := s.archiveBackend(peer)
	for _, o := range old {
		rc, e := b.Open(ctx, objectReference(o))
		if e == nil {
			rc.Close()
			t.Fatal("old directory object was not retired")
		}
	}
}

func TestFutureDriverLoadsBeforePersistedInstances(t *testing.T) {
	s, _, _ := newTestEnv(t)
	def := DriverDefinition{ID: "fixture", Name: "扩展驱动", Fields: []DriverField{{Key: "options.root", Label: "目录", Type: "text", Required: true}}, Open: func(_ *Service, _ string, c InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) {
		disk, e := storage.NewDisk(c.Options["root"])
		return storage.LocalCold{Disk: disk}, nil, nil, nil, e
	}}
	first, e := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log, WithStorageDriver(def))
	if e != nil {
		t.Fatal(e)
	}
	id, e := first.SaveStorageInstance("", "扩展实例", "fixture", InstanceConfig{Options: map[string]string{"root": t.TempDir()}}, true)
	if e != nil {
		t.Fatal(e)
	}
	restored, e := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log, WithStorageDriver(def))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = restored.archiveBackend(id); e != nil {
		t.Fatal(e)
	}
}
