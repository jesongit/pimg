package tasks

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestRetiredSourceTasksKeepOriginalAndDoNotRetry(t *testing.T) {
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
	source := filepath.Join(dir, "source.png")
	if err = os.WriteFile(source, []byte("source must remain"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := New(db, svc, 1, log)
	defer engine.Stop()
	for _, typ := range []string{dbx.TaskSourceScan, dbx.TaskSourceCleanup} {
		// Upgraded tasks can refer to removed rules, sources or malformed payloads.
		id, err := db.EnqueueUniqueTask(typ, "", "malformed legacy payload")
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.ClaimIdleTask(time.Now().Unix(), 100)
		if err != nil || task.ID != id {
			t.Fatal(task, err)
		}
		engine.run(task)
		current, err := db.GetTask(id)
		if err != nil || current.State != "done" || current.Result.String != `{"skipped":"source_import_rules_removed"}` {
			t.Fatal(current, err)
		}
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "source must remain" {
		t.Fatal("retired source task changed input", err)
	}
	album, err := db.EnsureDefaultAlbum()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SaveImportRule(dbx.ImportRule{Name: "legacy enabled rule", InstanceID: "local", Source: "inbox", AlbumID: album, Enabled: true, IntervalMinutes: 5}); err != nil {
		t.Fatal(err)
	}
	if err = svc.ReconcileStorage(); err != nil {
		t.Fatal(err)
	}
	var scheduled int
	if err = db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type IN ('source_scan','source_cleanup') AND state IN ('pending','running')`).Scan(&scheduled); err != nil || scheduled != 0 {
		t.Fatal("legacy rules were rescheduled", scheduled, err)
	}
}
