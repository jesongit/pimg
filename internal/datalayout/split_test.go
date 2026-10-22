package datalayout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"pimg/internal/config"
	"pimg/internal/dbx"
	"testing"
)

func TestNestedV1SplitUsesCurrentDatabaseAndKeepsRetryFiles(t *testing.T) {
	cfg, root := legacyLayout(t)
	nested := filepath.Join(root, "pimg-data")
	for old, next := range map[string]string{"pimg.db": "system/pimg.db", "dev-master.key": "system/keys/dev-master.key", "telegram": "system/telegram", "cold": "storage/local/archives", "hot": "storage/local/display", "previews": "previews"} {
		if err := CopyTree(filepath.Join(root, old), filepath.Join(nested, filepath.FromSlash(next))); err != nil {
			t.Fatal(err)
		}
	}
	db, err := dbx.Open(filepath.Join(nested, "system/pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetSetting("latest-v1-data", "keep-current")
	db.Close()
	for name, body := range map[string]string{"work/uploads/pending.bin": "pending-original", "work/tmp/pending.ct": "retry-cipher", "work/tmp/pending.ct.done": "cipher-complete", "work/tmp/spill-1.bin": "scratch", "cache/originals/a.bin": "view-cache", "custom-store/pimg-data/storage/one/archives/a": "custom-storage"} {
		p := filepath.Join(nested, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte(body), 0600)
	}
	b, _ := json.Marshal(Manifest{Version: 1, Origin: root})
	os.WriteFile(filepath.Join(nested, ".layout-v1.json"), b, 0600)
	cfg.Paths.RuntimeDir = t.TempDir()
	original := *cfg
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db, err = dbx.Open(cfg.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := db.GetSetting("latest-v1-data")
	if value != "keep-current" {
		t.Fatal("selected retired outer database")
	}
	db.Close()
	for p, body := range map[string]string{filepath.Join(root, "work/uploads/pending.bin"): "pending-original", filepath.Join(root, "work/cipher/pending.ct"): "retry-cipher", filepath.Join(root, "work/cipher/pending.ct.done"): "cipher-complete", filepath.Join(cfg.Paths.RuntimeDir, "tmp/spill-1.bin"): "scratch", filepath.Join(cfg.Paths.RuntimeDir, "cache/originals/a.bin"): "view-cache", filepath.Join(cfg.Paths.RuntimeDir, "previews/image.jpg"): "preview", cfg.Telegram.SessionFile: "original-login", filepath.Join(root, "custom-store/pimg-data/storage/one/archives/a"): "custom-storage"} {
		data, e := os.ReadFile(p)
		if e != nil || string(data) != body {
			t.Fatal(p, e)
		}
	}
	release()
	// Rebuilding the runtime directory never replays stale caches from v1.
	os.RemoveAll(filepath.Join(cfg.Paths.RuntimeDir, "cache"))
	release, err = Prepare(&original)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = os.Stat(filepath.Join(cfg.Paths.RuntimeDir, "cache")); !os.IsNotExist(err) {
		t.Fatal("replayed old runtime cache")
	}
}

func TestSplitRefusesAmbiguousDatabase(t *testing.T) {
	cfg, root := legacyLayout(t)
	db, err := dbx.Open(filepath.Join(root, "system/pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetSetting("independent", "preserve")
	db.Close()
	if release, err := Prepare(cfg); err == nil {
		release()
		t.Fatal("accepted unrelated target database")
	}
	db, err = dbx.Open(filepath.Join(root, "system/pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	value, _ := db.GetSetting("independent")
	if value != "preserve" {
		t.Fatal("target overwritten")
	}
}

func TestSplitRejectsRuntimeWithinData(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{DataDir: root, RuntimeDir: filepath.Join(root, "runtime")}}
	if release, err := Prepare(cfg); err == nil {
		release()
		t.Fatal("runtime inside transferable data")
	}
}

func TestFormerDevelopmentDefaultMovesIntoData(t *testing.T) {
	_, old := legacyLayout(t)
	app := t.TempDir()
	legacy := filepath.Join(app, "data-dev")
	if err := os.Rename(old, legacy); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(app, "data")
	cfg := &config.Config{Paths: config.Paths{DataDir: root, RuntimeDir: app, TempDir: filepath.Join(app, "tmp")}, MasterKeyFile: filepath.Join(root, "keys/master.key"), Telegram: config.Telegram{SessionFile: filepath.Join(root, "telegram/session.enc")}, Storage: config.Storage{LocalColdDir: filepath.Join(root, "cold"), LocalHotDir: filepath.Join(root, "hot")}}
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	db, err := dbx.Open(cfg.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	value, _ := db.GetSetting("kept-setting")
	if value != "kept-value" {
		t.Fatal("former development database not carried forward")
	}
	if _, err = os.Stat(filepath.Join(legacy, "pimg.db")); err != nil {
		t.Fatal("former development source removed")
	}
	if cfg.Paths.DataDir != root {
		t.Fatal("extra data directory layer")
	}
}
