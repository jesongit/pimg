package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

// Manual service tests do not run the engine. Cancel their outstanding queue
// references explicitly before deletion, just as the user is now required to.
func cancelFixtureReferences(t *testing.T, s *Service, fileID string) {
	t.Helper()
	refs, err := s.DB.FileTaskReferences(fileID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range refs {
		task, err := s.DB.GetTask(id)
		if err != nil || task.State == "running" {
			t.Fatal("fixture must stop running work before cancellation", err)
		}
		if err = s.DB.RequestTaskCancel(id); err != nil {
			t.Fatal(err)
		}
	}
}
func completeFixtureArchives(t *testing.T, s *Service, fileID string) {
	t.Helper()
	for _, typ := range []string{dbx.TaskArchive, dbx.TaskDirectArchive} {
		if task, err := s.DB.FindActiveTask(typ, fileID); err == nil {
			if err = s.DB.FinishTask(task.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func runFixtureSync(t *testing.T, s *Service, fileID string, repo int64) {
	t.Helper()
	if err := s.DB.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	j, err := s.DB.FindActiveTask(dbx.TaskColdSync, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunColdSync(WithTaskID(context.Background(), j.ID), fileID, repo); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.FinishTask(j.ID, ""); err != nil {
		t.Fatal(err)
	}
}
func localReplicaFixture(t *testing.T) (*Service, *dbx.File, int64, int64) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.InsertAlbum("旅行相册")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(workflowPNG(t)), "photo.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	completeFixtureArchives(t, s, f.ID)
	repo, err := s.AddColdRepository("本地副仓", "local", json.RawMessage(`{"directory":"replicas/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	return s, f, repo, album
}

func TestColdSyncExactCiphertextAlbumRenameMoveAndQueuedCleanup(t *testing.T) {
	s, f, repo, album := localReplicaFixture(t)
	master := s.Master
	s.Master = nil
	runFixtureSync(t, s, f.ID, repo)
	s.Master = master
	j, _ := s.DB.GetColdSyncJob(repo, f.ID)
	var copies []replicaCopy
	_ = json.Unmarshal([]byte(j.Objects), &copies)
	if len(copies) != 1 {
		t.Fatal("replica must contain exactly one original", len(copies))
	}
	before, _ := s.DB.GetFile(f.ID)
	if before.ColdBackend != "local" {
		t.Fatal("replication changed foreground routing")
	}
	adapter := storage.LocalReplica{DataDir: s.Cfg.Paths.DataDir, Directory: "replicas/a"}
	var oldOriginal replicaCopy
	for _, copy := range copies {
		if copy.Name == f.Name+".png" {
			oldOriginal = copy
		}
		if err := verifyReplica(context.Background(), adapter, copy); err != nil {
			t.Fatal(err)
		}
	}
	if oldOriginal.Reference == "" {
		t.Fatal("encrypted original missing")
	}
	if err := s.DB.RenameAlbum(album, "改名相册"); err != nil {
		t.Fatal(err)
	}
	runFixtureSync(t, s, f.ID, repo)
	renamed, err := adapter.Open(context.Background(), oldOriginal.Reference)
	if err != nil {
		t.Fatal("rename invalidated existing location", err)
	}
	renamed.Close()
	dest, _ := s.DB.InsertAlbum("目标相册")
	if err := s.DB.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: dest, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	runFixtureSync(t, s, f.ID, repo)
	cleanup, err := s.DB.FindActiveTask(dbx.TaskColdDelete, f.ID)
	if err != nil {
		t.Fatal("obsolete copy was deleted without an explicit task", err)
	}
	r, err := adapter.Open(context.Background(), oldOriginal.Reference)
	if err != nil {
		t.Fatal("old copy removed before cleanup task", err)
	}
	r.Close()
	var payload struct {
		RepositoryID int64    `json:"repository_id"`
		References   []string `json:"references"`
	}
	if err = json.Unmarshal([]byte(cleanup.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if err = s.RunColdDelete(context.Background(), f.ID, payload.RepositoryID, payload.References); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.FinishTask(cleanup.ID, "")
	if _, err = adapter.Open(context.Background(), oldOriginal.Reference); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete location remains", err)
	}
	if _, err = s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("sync/move changed the primary", err)
	}
	cancelFixtureReferences(t, s, f.ID)
	if err = s.RunPurgeFile(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	j, _ = s.DB.GetColdSyncJob(repo, f.ID)
	if j.Objects != "[]" {
		t.Fatal("purge left secondary references", j.Objects)
	}
}

func TestColdSyncTelegramCarrierBytesWithoutDecryption(t *testing.T) {
	s, tg := workflowEnv(t)
	album, _ := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	completeFixtureArchives(t, s, f.ID)
	repo, err := s.AddColdRepository("PNG archive", "local", json.RawMessage(`{"directory":"replicas/png"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.DB.SetColdRepositoryEnabled(repo, true)
	s.Master = nil
	runFixtureSync(t, s, f.ID, repo)
	j, _ := s.DB.GetColdSyncJob(repo, f.ID)
	var copies []replicaCopy
	_ = json.Unmarshal([]byte(j.Objects), &copies)
	adapter := storage.LocalReplica{DataDir: s.Cfg.Paths.DataDir, Directory: "replicas/png"}
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	ref, err := s.TGReference(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	original, err := tg.Download(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, copy := range copies {
		if copy.Name == f.Name+".png" {
			r, err := adapter.Open(context.Background(), copy.Reference)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(r)
			r.Close()
			if !bytes.Equal(data, original) || !bytes.HasPrefix(data, []byte{137, 80, 78, 71}) {
				t.Fatal("encrypted PNG was decrypted or rewrapped")
			}
			return
		}
	}
	t.Fatal("PNG carrier not replicated")
}

func TestColdSyncRelativeDirectoriesAndForegroundPreemption(t *testing.T) {
	s, _, _ := newTestEnv(t)
	archiveDir, err := filepath.Rel(s.Cfg.Paths.DataDir, s.Cfg.Storage.LocalColdDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{".", "../outside", `D:\absolute`, filepath.ToSlash(archiveDir), "uploads", "tmp", "diagnostics", "diagnostics/backup"} {
		raw, _ := json.Marshal(localReplicaConfig{Directory: directory})
		if _, err := s.AddColdRepository("bad", "local", raw); err == nil {
			t.Fatal("accepted unsafe directory", directory)
		}
	}
	if _, err := s.AddColdRepository("ok", "local", json.RawMessage(`{"directory":"replicas/safe"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddColdRepository("overlap", "local", json.RawMessage(`{"directory":"replicas/safe/nested"}`)); err == nil {
		t.Fatal("overlapping repository roots accepted")
	}
	ctx, done, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finish := s.BeginForeground()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("foreground operation did not preempt sync")
	}
	done()
	if _, _, err = s.BeginIdle(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("idle work started during foreground operation", err)
	}
	finish()
	if _, err = os.Stat(filepath.Join(s.Cfg.Paths.DataDir, "replicas", "safe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("adding unchecked repository performed storage writes", err)
	}
}

type faultReplica struct {
	storage.Replica
	corruptNextOpen, failNextDelete bool
}

func (r *faultReplica) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
	if r.corruptNextOpen {
		r.corruptNextOpen = false
		return io.NopCloser(bytes.NewReader([]byte("corrupt secondary"))), nil
	}
	return r.Replica.Open(ctx, ref)
}
func (r *faultReplica) Delete(ctx context.Context, ref string) error {
	if r.failNextDelete {
		r.failNextDelete = false
		return errors.New("synthetic secondary offline")
	}
	return r.Replica.Delete(ctx, ref)
}
func faultReplicaFixture(t *testing.T, s *Service) (int64, *faultReplica) {
	t.Helper()
	driver := &faultReplica{Replica: storage.LocalReplica{DataDir: s.Cfg.Paths.DataDir, Directory: "replicas/fault"}}
	if err := s.RegisterReplicaDriver(ReplicaDriver{ID: "fault", Name: "synthetic", Validate: func(json.RawMessage) (string, error) { return "{}", nil }, Open: func(context.Context, string) (storage.Replica, error) { return driver, nil }}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.AddColdRepository("fault", "fault", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	return repo, driver
}
func TestColdSyncFailedReadbackRetainsPrimaryAndResumesCheckpoint(t *testing.T) {
	s, f, _, _ := localReplicaFixture(t)
	repo, driver := faultReplicaFixture(t, s)
	driver.corruptNextOpen = true
	if err := s.DB.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.RunColdSync(context.Background(), f.ID, repo); err == nil {
		t.Fatal("corrupt secondary accepted")
	}
	var completed int64
	if err := s.DB.QueryRow(`SELECT done_revision FROM cold_sync_files WHERE repository_id=? AND file_id=?`, repo, f.ID).Scan(&completed); err != nil || completed != 0 {
		t.Fatal("failed readback marked synchronized", err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("secondary error harmed the primary", err)
	}
	if err := s.RunColdSync(context.Background(), f.ID, repo); err != nil {
		t.Fatal("saved write could not resume", err)
	}
	j, _ := s.DB.GetColdSyncJob(repo, f.ID)
	var copies []replicaCopy
	_ = json.Unmarshal([]byte(j.Objects), &copies)
	refs := map[string]bool{}
	for _, c := range copies {
		if refs[c.Reference] {
			t.Fatal("retry duplicated one committed object", c.Reference)
		}
		refs[c.Reference] = true
	}
}
func TestColdSyncPurgeRevokesPublicLinksBeforeOfflineSecondaryAndRetainsPrimary(t *testing.T) {
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	localHTTP(t, s)
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(workflowPNG(t)), "published.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunDirectArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	completeFixtureArchives(t, s, f.ID)
	if err = s.RunPublish(context.Background(), f.ID, false); err != nil {
		t.Fatal(err)
	}
	cancelFixtureReferences(t, s, f.ID)
	repo, driver := faultReplicaFixture(t, s)
	runFixtureSync(t, s, f.ID, repo)
	f, _ = s.DB.GetFile(f.ID)
	address := s.localHot.PublicURL(origKey(f))
	checkHTTP(t, address, 200, nil)
	driver.failNextDelete = true
	if err = s.RunPurgeFile(context.Background(), f.ID); err == nil {
		t.Fatal("secondary failure was ignored")
	}
	checkHTTP(t, address, 404, nil)
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	if len(chunks) == 0 {
		t.Fatal("primary erased before secondary cleanup finished")
	}
	if err = s.RunPurgeFile(context.Background(), f.ID); err != nil {
		t.Fatal("secondary cleanup could not resume", err)
	}
}
