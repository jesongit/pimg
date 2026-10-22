package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type reviewArchiveFailure struct {
	storage.LocalCold
	manifestOnly bool
	written      string
}

func (b *reviewArchiveFailure) PutReader(ctx context.Context, key string, r io.Reader, n int64, caption string) (storage.Reference, error) {
	if !b.manifestOnly {
		return storage.Reference{}, errors.New("injected unavailable archive")
	}
	ref, err := b.LocalCold.PutReader(ctx, key, r, n, caption)
	if err == nil && strings.Contains(key, "manifest") && b.written == "" {
		b.written = key
		return storage.Reference{}, errors.New("injected lost manifest response")
	}
	return ref, err
}

func TestReviewUnavailableTargetDoesNotStarveHealthyArchive(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	i, _ := s.instance(peer)
	i.cold = &reviewArchiveFailure{LocalCold: i.cold.(storage.LocalCold)}
	if err := s.RunArchiveSync(context.Background(), f.ID); err == nil {
		t.Fatal("unavailable target should remain retryable")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM archive_manifests WHERE file_id=? AND instance_id='r2'`, f.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("healthy archive was starved by another target", count, err)
	}
}

func TestReviewGarbageNeverDeletesCurrentArchive(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	objects, _ := s.DB.ArchiveObjects(f.ID, "local")
	for _, o := range objects {
		if _, err := s.DB.Exec(`INSERT INTO archive_garbage(file_id,instance_id,reference) VALUES(?,?,?)`, f.ID, "local", o.Reference); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.cleanupArchiveGarbage(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("cleanup deleted a currently referenced object", err)
	}
}

func TestReviewInterruptedCanonicalMoveRepairsLocator(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	album, _ := s.DB.InsertAlbum("after interrupted move")
	if err := s.DB.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: album, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER review_chunk_fault BEFORE UPDATE ON file_chunks BEGIN SELECT RAISE(ABORT,'injected chunk commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchiveSync(context.Background(), f.ID); err == nil {
		t.Fatal("fault did not interrupt canonical commit")
	}
	if _, err := s.DB.Exec(`DROP TRIGGER review_chunk_fault`); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	r, err := restarted.SourceStream(context.WithValue(context.Background(), archiveSourceContext{}, "local"), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("restart did not repair canonical locator before retiring old objects", err)
	}
}

func TestReviewManifestRejectsStaleAlbumObjects(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	album, _ := s.DB.InsertAlbum("concurrent destination")
	if err := s.DB.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: album, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	cs, _ := s.allChunks(f.ID)
	if err := s.saveArchiveManifest(context.Background(), f, "local", "review-new-revision", len(cs)); err == nil {
		t.Fatal("manifest claimed stale locations as current album")
	}
}

func TestReviewLostManifestAcknowledgmentRemainsPurgeable(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(peer)
	fault := &reviewArchiveFailure{LocalCold: i.cold.(storage.LocalCold), manifestOnly: true}
	i.cold = fault
	if err := s.DB.UpdateFileMeta(f.ID, "new metadata", sql.NullInt64{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil || fault.written != "" {
		t.Fatal("metadata edit must not upload a manifest", err)
	}
	if _, err := s.DB.Exec(`UPDATE tasks SET state='canceled',cancel_requested=1 WHERE file_id=? AND state IN ('pending','failed')`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunPurgeFile(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if fault.written != "" {
		t.Fatal("unacknowledged manifest became an untracked remote orphan")
	}
}

func reviewSourceFixture(t *testing.T) (*Service, int64, *dbx.File, string, []byte) {
	t.Helper()
	s, _, plain := peerFixture(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "inbox"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "inbox", "source.png")
	if err := os.WriteFile(path, plain, 0600); err != nil {
		t.Fatal(err)
	}
	instance, err := s.SaveStorageInstance("", "review source", "local", InstanceConfig{Directory: root}, true)
	if err != nil {
		t.Fatal(err)
	}
	album, _ := s.DB.EnsureDefaultAlbum()
	rule, err := s.SaveImportRule(dbx.ImportRule{Name: "review source", InstanceID: instance, Source: "inbox", AlbumID: album, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunSourceScan(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	var file string
	if err = s.DB.QueryRow(`SELECT file_id FROM import_receipts WHERE rule_id=?`, rule).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	f, _ := s.DB.GetFile(file)
	return s, rule, f, path, plain
}

func TestReviewSourceContentChangeWithPreservedMetadataIsRetained(t *testing.T) {
	s, _, f, path, plain := reviewSourceFixture(t)
	info, _ := os.Stat(path)
	changed := append([]byte(nil), plain...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunSourceCleanup(context.Background(), f.ID); err == nil {
		t.Error("changed bytes were treated as the archived source version")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, changed) {
		t.Fatal("source content was lost despite preserved timestamp", err)
	}
}

func TestReviewPausedSourceRuleDoesNotDeleteOriginal(t *testing.T) {
	s, rule, f, path, _ := reviewSourceFixture(t)
	if _, err := s.DB.Exec(`UPDATE import_rules SET enabled=0 WHERE id=?`, rule); err != nil {
		t.Fatal(err)
	}
	if err := s.RunSourceCleanup(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("paused rule still removed source", err)
	}
}

func TestReviewTemporaryCopyRejectsWholeFileHashMismatch(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if _, err := s.DB.Exec(`UPDATE files SET sha256=? WHERE id=?`, strings.Repeat("f", 64), f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunTempMaterialize(context.Background(), f.ID, "original"); err == nil {
		t.Fatal("temporary publication skipped whole-original verification")
	}
	var count int
	_ = s.DB.QueryRow(`SELECT count(*) FROM temp_objects WHERE file_id=?`, f.ID).Scan(&count)
	if count != 0 {
		t.Fatal("invalid plaintext received a temporary publication intent")
	}
}

type reviewSlowTemp struct {
	storage.Hot
	db      *dbx.DB
	started chan struct{}
	release chan struct{}
}

func (h *reviewSlowTemp) PutObject(ctx context.Context, scope, key string, r io.Reader, n int64, mime, cache string) error {
	if _, err := h.db.Exec(`UPDATE temp_objects SET expires_at=0 WHERE object_key=?`, key); err != nil {
		return err
	}
	close(h.started)
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return h.Hot.PutObject(ctx, scope, key, r, n, mime, cache)
}

func TestReviewExpiryCannotRaceInFlightTemporaryUpload(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	h := &reviewSlowTemp{Hot: s.hotBackends["local"], db: s.DB, started: make(chan struct{}), release: make(chan struct{})}
	s.hotBackends["local"] = h
	var release sync.Once
	defer release.Do(func() { close(h.release) })
	done := make(chan error, 1)
	go func() { done <- s.RunTempMaterialize(context.Background(), f.ID, "original") }()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("temporary upload did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	n, _ := s.CleanupExpiredTemp(ctx)
	cancel()
	if n != 0 {
		t.Error("expiry acknowledged deletion before in-flight plaintext committed")
	}
	release.Do(func() { close(h.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n, err := s.CleanupExpiredTemp(context.Background()); err != nil || n != 1 {
		t.Fatal("committed expired upload lost its cleanup record", n, err)
	}
}

type reviewDeleteFailure struct{ storage.Hot }

func (h reviewDeleteFailure) Delete(context.Context, string, string) error {
	return errors.New("injected provider outage")
}
func TestReviewExpiryOutageDoesNotStarveAnotherInstance(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	id, err := s.SaveStorageInstance("", "offline display", "local", InstanceConfig{Directory: t.TempDir()}, true)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(id)
	i.hot = reviewDeleteFailure{i.hot}
	for n := 0; n < 200; n++ {
		if err = s.DB.InsertTempObject(&dbx.TempObject{ID: fmt.Sprintf("000-offline-%03d", n), FileID: f.ID, Kind: "original", Backend: id, ObjectKey: fmt.Sprintf("old-%03d", n), ExpiresAt: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.localHot.PutObject(context.Background(), storage.Temporary, "healthy", bytes.NewReader(plain), int64(len(plain)), "image/png", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.InsertTempObject(&dbx.TempObject{ID: "zzz-healthy-last", FileID: f.ID, Kind: "original", Backend: "local", ObjectKey: "healthy", ExpiresAt: 0}); err != nil {
		t.Fatal(err)
	}
	first, _ := s.CleanupExpiredTemp(context.Background())
	if first != 0 {
		t.Fatal("fixture must fill the first batch with the unavailable instance", first)
	}
	if n, _ := s.CleanupExpiredTemp(context.Background()); n != 1 {
		t.Fatal("same failed batch starved the healthy instance", first, n)
	}
	if _, err = s.localHot.HeadSize(context.Background(), storage.Temporary, "healthy"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("healthy expired object was not physically deleted", err)
	}
}

func TestReviewStalePendingSourceDoesNotBlockNewVersion(t *testing.T) {
	s, _, _ := peerFixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "source.png")
	if err := os.WriteFile(path, tinyMedia(t), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := s.SaveStorageInstance("", "pending source", "local", InstanceConfig{Directory: root}, true)
	if err != nil {
		t.Fatal(err)
	}
	album, _ := s.DB.EnsureDefaultAlbum()
	rule, err := s.SaveImportRule(dbx.ImportRule{Name: "pending source", InstanceID: id, AlbumID: album, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(id)
	page, err := i.source.List(context.Background(), "", "", false)
	if err != nil || len(page.Entries) != 1 {
		t.Fatal(err)
	}
	old := page.Entries[0]
	if _, err = s.DB.Exec(`INSERT INTO import_receipts(rule_id,locator,version,entry,state) VALUES(?,?,?,?,'pending')`, rule, old.ID, old.Version, dbx.JSONValue(old)); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	img.SetRGBA(0, 0, color.RGBA{R: 9, A: 255})
	var changed bytes.Buffer
	if err = png.Encode(&changed, img); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, changed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RunSourceScan(context.Background(), rule); err != nil {
		t.Fatal("stale receipt blocked the current source version", err)
	}
	var state string
	if err = s.DB.QueryRow(`SELECT state FROM import_receipts WHERE rule_id=? AND version=?`, rule, old.Version).Scan(&state); err != nil || state != "ignored" {
		t.Fatal("stale receipt was not retired", state, err)
	}
	var count int
	_ = s.DB.QueryRow(`SELECT count(*) FROM import_receipts WHERE rule_id=? AND state='archiving'`, rule).Scan(&count)
	if count != 1 {
		t.Fatal("new source version not ingested", count)
	}
}

func TestReviewArchiveVerificationFallsBackToCompletePeer(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.DB.ListChunks(f.ID, "data")
	disk := s.coldBackends["local"].(storage.LocalCold).Disk
	if err := os.WriteFile(filepath.Join(disk.Dir, filepath.FromSlash(cs[0].ObjectKey)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("healthy complete peer was ignored during verification", err)
	}
}

func TestReviewVerificationCannotAssembleIncompletePeers(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if _, err := s.DB.Exec(`DELETE FROM file_chunks WHERE file_id=? AND kind='data'`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DELETE FROM archive_objects WHERE file_id=? AND kind='data'`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE files SET storage='direct',sha256=?,size=6 WHERE id=?`, sha256Hex([]byte("abcdef")), f.ID); err != nil {
		t.Fatal(err)
	}
	local := s.coldBackends["local"].(storage.LocalCold)
	a, err := local.Put(context.Background(), "review-half-a", []byte("abc"), "")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.archiveBackend(peer)
	b, err := other.Put(context.Background(), "review-half-b", []byte("def"), "")
	if err != nil {
		t.Fatal(err)
	}
	for idx, data := range []string{"abc", "def"} {
		key := "missing"
		if idx == 0 {
			key = a.Key
		}
		if err = s.DB.UpsertChunk(&dbx.Chunk{FileID: f.ID, Kind: "data", Idx: idx, ObjectKey: key, Size: 3, SHA256: sha256Hex([]byte(data))}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.DB.SaveArchiveObject(dbx.ArchiveObject{FileID: f.ID, InstanceID: "local", Kind: "data", Idx: 0, Reference: referenceJSON(a), Size: 3, SHA256: sha256Hex([]byte("abc")), Verified: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SaveArchiveObject(dbx.ArchiveObject{FileID: f.ID, InstanceID: peer, Kind: "data", Idx: 1, Reference: referenceJSON(b), Size: 3, SHA256: sha256Hex([]byte("def")), Verified: true}); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	if err = s.verifyIdleArchive(context.Background(), f); err == nil {
		t.Fatal("source cleanup must require one complete archive, not complementary partial replicas")
	}
}

type reviewUnplannedArchive struct {
	storage.Cold
	writer *reviewArchiveFailure
}

func (b reviewUnplannedArchive) PutReader(ctx context.Context, key string, r io.Reader, n int64, caption string) (storage.Reference, error) {
	return b.writer.PutReader(ctx, key, r, n, caption)
}
func TestReviewUnplannedLostManifestIsRecoveredBeforePurge(t *testing.T) {
	s, peer, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(peer)
	fault := &reviewArchiveFailure{LocalCold: i.cold.(storage.LocalCold), manifestOnly: true}
	i.cold = reviewUnplannedArchive{Cold: fault.LocalCold, writer: fault}
	if err := s.DB.UpdateFileMeta(f.ID, "changed metadata", sql.NullInt64{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil || fault.written != "" {
		t.Fatal("metadata edit must not upload a manifest", err)
	}
	if _, err := s.DB.Exec(`UPDATE tasks SET state='canceled' WHERE file_id=? AND state IN ('pending','failed')`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunPurgeFile(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if fault.written != "" {
		t.Fatal("unplanned manifest orphaned")
	}
}

func TestReviewPurgeRemovesExtraCanonicalLocator(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	cs, _ := s.DB.ListChunks(f.ID, "data")
	b := s.coldBackends["local"].(storage.LocalCold)
	r, err := b.Open(context.Background(), chunkRef(cs[0]))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := b.Put(context.Background(), "review-extra-canonical", data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SaveArchiveObject(dbx.ArchiveObject{FileID: f.ID, InstanceID: "local", Kind: "data", Idx: cs[0].Idx, Reference: referenceJSON(ref), Size: cs[0].Size, SHA256: cs[0].SHA256, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='canceled' WHERE file_id=? AND state IN ('pending','failed')`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunPurgeFile(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(b.Disk.Dir, ref.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("extra verified canonical locator leaked after purge", err)
	}
}

type reviewRemoteRead struct {
	storage.Hot
	opens int
}

func (h *reviewRemoteRead) OpenObject(context.Context, string, string) (io.ReadCloser, error) {
	h.opens++
	return nil, errors.New("remote unavailable")
}
func TestReviewOriginalHonorsExplicitArchivePriority(t *testing.T) {
	s, _, plain := peerFixture(t)
	i0, _ := s.instance("local")
	c0 := i0.config
	c0.Priority = 20
	if _, err := s.SaveStorageInstance("local", i0.row.Name, "local", c0, true); err != nil {
		t.Fatal(err)
	}
	f := archiveMedia(t, s, plain)
	id, err := s.SaveStorageInstance("", "remote temporary", "r2", InstanceConfig{Endpoint: "https://example.invalid", Bucket: "review", AccessKey: "fixture", SecretKey: "fixture", Prefix: "review"}, true)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(id)
	h := &reviewRemoteRead{Hot: i.hot}
	i.hot = h
	obj := &dbx.TempObject{ID: "review-remote", FileID: f.ID, Kind: "original", ObjectKey: "remote", Backend: id, Size: sql.NullInt64{Int64: f.Size, Valid: true}, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err = s.DB.InsertTempObject(obj); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.MarkTempReady(obj.ID); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(s.originalCachePath(f))
	r, err := s.ViewOriginal(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, plain) || h.opens != 0 {
		t.Fatal("local original was delayed by a remote display read", h.opens, err)
	}
}

func TestReviewInheritedStorageNamespacesCannotBeAliased(t *testing.T) {
	s, _, _ := peerFixture(t)
	root := s.coldBackends["local"].(storage.LocalCold).Disk.Dir
	if _, err := s.SaveStorageInstance("", "duplicate local archive", "local", InstanceConfig{Directory: root}, true); err == nil {
		t.Fatal("bootstrap archive root accepted as an independent instance")
	}
	c := InstanceConfig{Endpoint: s.Cfg.R2.Endpoint, Bucket: s.Cfg.R2.PermBucket, AccessKey: "fixture", SecretKey: "fixture"}
	for _, prefix := range []string{"", "archives", "pimg/subfolder"} {
		c.Prefix = prefix
		if _, err := s.SaveStorageInstance("", "duplicate R2", "r2", c, true); err == nil {
			t.Fatal("bootstrap R2 namespace accepted as independent storage", prefix)
		}
	}
	c.Prefix = "review-independent"
	if _, err := s.SaveStorageInstance("", "independent R2", "r2", c, true); err != nil {
		t.Fatal("isolated R2 prefix should remain usable", err)
	}
}

func TestReviewPolicyInputCannotMutateRuntimeSelection(t *testing.T) {
	s, _, _ := peerFixture(t)
	p := s.StoragePolicy()
	want := p.Archives[0]
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	p.Archives[0] = "modified after save"
	if got := s.StoragePolicy().Archives[0]; got != want {
		t.Fatal("caller retained mutable storage policy state", got)
	}
}

func TestReviewPermanentPublishFallsBackToHealthyArchive(t *testing.T) {
	s, _, plain := peerFixture(t)
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(plain), "publish.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunDirectArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunUnpublish(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	cs, _ := s.DB.ListChunks(f.ID, "data")
	disk := s.coldBackends["local"].(storage.LocalCold).Disk
	if err = os.WriteFile(filepath.Join(disk.Dir, filepath.FromSlash(cs[0].ObjectKey)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	s.ForgetMediaViews(f)
	if err = s.RunPublish(context.Background(), f.ID, false); err != nil {
		t.Fatal("publication ignored a healthy original replica", err)
	}
}
