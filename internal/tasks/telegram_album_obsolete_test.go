package tasks

import (
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

func TestRemovedAlbumSyncTaskFinishesWithoutRetryOrTelegramLogin(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}}
	svc, err := store.New(cfg, db, master, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.EnqueueUniqueTask(dbx.TaskTelegramRenameAlbum, dbx.TelegramAlbumTaskKey(999999), `{"album_id":999999,"name":"removed"}`)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(db, svc, 1, log)
	defer engine.Stop()
	task, err := db.ClaimTask(time.Now().Unix(), 100)
	if err != nil || task.ID != id {
		t.Fatal(task, err)
	}
	engine.run(task)
	current, err := db.GetTask(id)
	if err != nil || current.State != "done" || current.Result.String != `{"skipped":"album_removed_or_deleting"}` {
		t.Fatal("obsolete task still waiting for retry", current, err)
	}
}
