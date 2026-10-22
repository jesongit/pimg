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
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"testing"
	"time"
)

func TestPressureSameFileForegroundQueuePreemptsIdleBeforeClaim(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Storage: config.Storage{Cold: "local", Hot: "local"}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	svc, err := store.New(cfg, db, master, nil, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	f, err := legacyUploadFixture(svc,context.Background(), &data, "same.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	_, err = db.EnqueueTask(dbx.TaskArchiveSync, f.ID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	background, err := db.ClaimIdleTask(time.Now().Unix(), 60)
	if err != nil {
		t.Fatal(err)
	}
	idle, finish, err := svc.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	eng := New(db, svc, 1, logger)
	defer eng.Stop()
	released := make(chan struct{})
	go func() { <-idle.Done(); finish(); _ = db.PostponeIdleTask(background.ID); eng.Wake(); close(released) }()
	foreground, err := db.EnqueueTask(dbx.TaskVerify, f.ID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	eng.wg.Add(1)
	go eng.workerLoop()
	select {
	case <-idle.Done():
	case <-time.After(time.Second):
		t.Error("foreground could not be claimed because idle work held the same file")
	}
	finish()
	<-released
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, e := db.GetTask(foreground)
		if e != nil {
			t.Fatal(e)
		}
		if task.State == "done" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("foreground task did not finish after idle yielded")
}
