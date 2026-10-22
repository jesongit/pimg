package tasks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"pimg/internal/store"
	"pimg/internal/tgx"
)

type heldReplica struct {
	storage.Replica
	started, release chan struct{}
}

func (h *heldReplica) Put(ctx context.Context, album, file string, obj storage.ReplicaObject, input io.Reader) (string, error) {
	ref, err := h.Replica.Put(ctx, album, file, obj, input)
	if err != nil {
		return ref, err
	}
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-h.release
	return ref, nil
}
func TestColdSyncRunningCancellationKeepsReferenceUntilStopped(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	svc, err := store.New(cfg, db, master, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(db, svc, 1, log)
	defer engine.Stop()
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 24, 24)))
	f, err := legacyUploadFixture(svc, context.Background(), &encoded, "photo.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := db.ClaimTask(time.Now().Unix(), 100)
	if err != nil {
		t.Fatal(err)
	}
	engine.run(archive)
	held := &heldReplica{Replica: storage.LocalReplica{DataDir: dir, Directory: "replicas/held"}, started: make(chan struct{}, 1), release: make(chan struct{})}
	err = svc.RegisterReplicaDriver(store.ReplicaDriver{ID: "held", Name: "synthetic adapter", Validate: func(json.RawMessage) (string, error) { return "{}", nil }, Open: func(context.Context, string) (storage.Replica, error) { return held, nil }})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := svc.AddColdRepository("held", "held", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetColdRepositoryEnabled(repo, true)
	if err = db.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimIdleTask(time.Now().Unix(), 100)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); engine.run(job) }()
	select {
	case <-held.started:
	case <-time.After(3 * time.Second):
		t.Fatal("sync did not start")
	}
	if err = engine.CancelTask(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal("deletion should queue behind running work", err)
	}
	if err = db.DeleteFile(f.ID); !errors.Is(err, dbx.ErrTaskReferenced) {
		t.Fatal("running cancelled task lost its reference", err)
	}
	state, _ := db.GetTask(job.ID)
	if state.State != "running" {
		t.Fatal("running operation was declared stopped early")
	}
	close(held.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not stop")
	}
	state, _ = db.GetTask(job.ID)
	if state.State != "canceled" {
		t.Fatal("cancellation not acknowledged", state.State)
	}
	if _, err = db.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal(err)
	}
	deletion, err := db.ClaimIdleTask(time.Now().Unix(), 100)
	if err != nil {
		t.Fatal(err)
	}
	engine.run(deletion)
	if _, err = db.GetFile(f.ID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("deletion did not finish", err)
	}
	receipt, _ := db.GetColdSyncJob(repo, f.ID)
	if receipt.Objects != "[]" {
		t.Fatal("cancelled partial replica was not cleaned", receipt.Objects)
	}
}

func TestColdSyncAutomaticWorkerWaitsForForegroundThenReplicates(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	// An injected unused fixture transport disables the user-account runtime.
	// Every operation in this test uses local storage; no provider is contacted.
	svc, err := store.New(cfg, db, master, tgx.New("", "", ""), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 24, 24)))
	f, err := legacyUploadFixture(svc, context.Background(), &encoded, "auto.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := svc.AddColdRepository("auto", "local", json.RawMessage(`{"directory":"replicas/automatic"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	finishForeground := svc.BeginForeground()
	released := false
	defer func() {
		if !released {
			finishForeground()
		}
	}()
	engine := New(db, svc, 1, log)
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	wait := func(label string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if ready() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal(label)
	}
	wait("normal archive did not finish", func() bool { current, err := db.GetFile(f.ID); return err == nil && current.ArchiveState == "done" })
	time.Sleep(2200 * time.Millisecond)
	repos, err := db.ColdRepositories()
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Synced != 0 {
		t.Fatal("idle worker wrote while foreground activity was held")
	}
	finishForeground()
	released = true
	wait("idle worker did not automatically replicate the archive", func() bool { repos, err := db.ColdRepositories(); return err == nil && repos[0].Synced == 1 })
	if _, err = svc.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("replication changed the primary", err)
	}
}
