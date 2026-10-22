package tasks

import (
	"bytes"
	"context"
	"database/sql"
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
	"pimg/internal/store"
)

func TestOwnerTransferWorkerClaimsCutoverAndIndependentCleanup(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	svc, err := store.New(cfg, db, master, nil, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(db, svc, 1, logger)
	defer engine.Stop()
	if err = db.CreateAccount("user:recipient", "Recipient", "hash"); err != nil {
		t.Fatal(err)
	}
	album, err := db.InsertOwnerAlbum("user:recipient", "Target")
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 24, 24))); err != nil {
		t.Fatal(err)
	}
	f, err := svc.ReceiveUpload(context.Background(), &encoded, "image.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := db.ClaimTask(time.Now().Unix(), 100)
	if err != nil {
		t.Fatal(err)
	}
	engine.run(archive)
	c, err := svc.QueueOwnerTransfer(context.Background(), f.ID, album, 0)
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimTask(time.Now().Unix(), 100)
	if err != nil || job.ID != c.TaskID || job.Type != dbx.TaskTransferOwner {
		t.Fatal("worker could not claim transfer", job, err)
	}
	engine.run(job)
	task, err := db.GetTask(c.TaskID)
	if err != nil || task.State != "done" {
		t.Fatal("transfer worker failed", task, err)
	}
	target, err := db.GetVisibleFile(c.TargetID)
	if err != nil || target.OwnerID != "user:recipient" || target.Published {
		t.Fatal("worker did not promote private target", target, err)
	}
	cleanup, err := db.ClaimIdleTask(time.Now().Unix(), 100)
	if err != nil || cleanup.Type != dbx.TaskCleanupConverted {
		t.Fatal("independent cleanup was not claimable", cleanup, err)
	}
	engine.run(cleanup)
	state, err := db.GetConversion(f.ID)
	if err != nil || state.CleanupPending {
		t.Fatal("worker did not finish cleanup", state, err)
	}
	if replicas, err := db.MediaReplicas(f.ID); err != nil || len(replicas) != 0 {
		t.Fatal("cleanup left retired bytes", replicas, err)
	}
	if _, err = svc.Verify(context.Background(), target.ID); err != nil {
		t.Fatal("worker cleanup damaged verified target", err)
	}
}
