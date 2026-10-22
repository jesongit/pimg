package datalayout

import (
	"database/sql"
	"os"
	"path/filepath"
	"pimg/internal/config"
	"pimg/internal/dbx"
	"testing"
)

func legacyLayout(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.EnsureAdmin("kept-admin", "kept-hash"); err != nil {
		t.Fatal(err)
	}
	if err = db.InsertSession("kept-cookie", 9999999999, "ua", "ip"); err != nil {
		t.Fatal(err)
	}
	if err = db.SetSetting("kept-setting", "kept-value"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for name, data := range map[string]string{"dev-master.key": "original-key", "telegram/session.enc": "original-login", "cold/albums/1/file/data": "original-bytes", "hot/.signing-key": "original-signing", "previews/image.jpg": "preview"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0700)
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, MasterKeyFile: filepath.Join(dir, "dev-master.key"), Telegram: config.Telegram{SessionFile: filepath.Join(dir, "telegram", "session.enc")}, Storage: config.Storage{LocalColdDir: filepath.Join(dir, "cold"), LocalHotDir: filepath.Join(dir, "hot")}}
	return cfg, dir
}

func TestLayoutPreservesDatabaseConfigKeysAndLogin(t *testing.T) {
	cfg, source := legacyLayout(t)
	original := *cfg
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paths.DataDir != source {
		t.Fatal(cfg.Paths.DataDir)
	}
	for _, pair := range [][2]string{{cfg.MasterKeyFile, "original-key"}, {cfg.Telegram.SessionFile, "original-login"}, {filepath.Join(cfg.Storage.LocalColdDir, "albums/1/file/data"), "original-bytes"}, {filepath.Join(cfg.Storage.LocalHotDir, ".signing-key"), "original-signing"}} {
		data, e := os.ReadFile(pair[0])
		if e != nil || string(data) != pair[1] {
			t.Fatal(pair, e)
		}
	}
	db, err := dbx.Open(cfg.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	user, hash, err := db.GetAdmin()
	if err != nil || user != "kept-admin" || hash != "kept-hash" {
		t.Fatal(user, hash, err)
	}
	session, err := db.GetSession("kept-cookie")
	if err != nil || session.OwnerID != dbx.AdminOwner {
		t.Fatal(session, err)
	}
	value, _ := db.GetSetting("kept-setting")
	if value != "kept-value" {
		t.Fatal(value)
	}
	db.Close()
	if _, err = Prepare(&original); err == nil {
		t.Fatal("concurrent process entered layout")
	}
	release()
	release, err = Prepare(&original)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if original.Paths.DataDir != cfg.Paths.DataDir || original.Telegram.SessionFile != cfg.Telegram.SessionFile {
		t.Fatal("restart changed roots")
	}
	if _, err = os.Stat(filepath.Join(source, "pimg.db")); err != nil {
		t.Fatal("rollback source removed", err)
	}
}

func TestLayoutConflictsRemainRecoverableAndNeverOverwrite(t *testing.T) {
	cfg, source := legacyLayout(t)
	target := filepath.Join(source, "system", "keys", "dev-master.key")
	os.MkdirAll(filepath.Dir(target), 0700)
	os.WriteFile(target, []byte("different-key"), 0600)
	if _, err := Prepare(cfg); err == nil {
		t.Fatal("conflicting key overwritten")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "different-key" {
		t.Fatal("key changed")
	}
	if _, err := os.Stat(filepath.Join(source, ".layout-v2.json")); !os.IsNotExist(err) {
		t.Fatal("failed migration marked complete")
	}
	os.Remove(target)
	cfg.Paths.DataDir = source
	cfg.Paths.LegacyDir = ""
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestLayoutRefusesActiveLegacyDatabase(t *testing.T) {
	cfg, source := legacyLayout(t)
	live, err := dbx.Open(filepath.Join(source, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err = live.GetSetting("kept-setting"); err != nil {
		t.Fatal(err)
	}
	if release, err := Prepare(cfg); err == nil {
		release()
		t.Fatal("active legacy database was forked")
	}
}

func TestLayoutCopyRejectsNestedTarget(t *testing.T) {
	source := t.TempDir()
	if err := CopyTree(source, filepath.Join(source, "nested")); err == nil {
		t.Fatal("recursive target accepted")
	}
}

func TestLayoutMovesAsOneDirectoryWithoutRegeneratingKey(t *testing.T) {
	cfg, source := legacyLayout(t)
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	release()
	destination := filepath.Join(t.TempDir(), "pimg-data")
	if err = CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	moved := &config.Config{Paths: config.Paths{DataDir: destination, TempDir: filepath.Join(destination, "tmp")}, MasterKeyFile: filepath.Join(destination, "keys", "master.key"), Telegram: config.Telegram{SessionFile: filepath.Join(destination, "telegram", "session.enc")}, Storage: config.Storage{LocalColdDir: filepath.Join(destination, "cold"), LocalHotDir: filepath.Join(destination, "hot")}}
	release, err = Prepare(moved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	key, err := os.ReadFile(moved.MasterKeyFile)
	if err != nil || string(key) != "original-key" {
		t.Fatal("move selected a new master key", moved.MasterKeyFile, err)
	}
	session, err := os.ReadFile(moved.Telegram.SessionFile)
	if err != nil || string(session) != "original-login" {
		t.Fatal("move lost session", err)
	}
	data, err := os.ReadFile(filepath.Join(moved.Storage.LocalColdDir, "albums/1/file/data"))
	if err != nil || string(data) != "original-bytes" {
		t.Fatal("move lost local archive", err)
	}
}

func TestLayoutInterruptedSnapshotRejectsChangedSource(t *testing.T) {
	cfg, source := legacyLayout(t)
	target := filepath.Join(source, "system", "keys", "dev-master.key")
	os.MkdirAll(filepath.Dir(target), 0700)
	os.WriteFile(target, []byte("conflict"), 0600)
	if _, err := Prepare(cfg); err == nil {
		t.Fatal("expected conflict")
	}
	db, err := dbx.Open(filepath.Join(source, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetSetting("newer-data", "preserve")
	db.Close()
	cfg.Paths.DataDir = source
	if _, err = Prepare(cfg); err == nil {
		t.Fatal("stale partial snapshot replaced newer source")
	}
}

func TestLayoutKeepsLegacyDatabaseGuardAfterCutoverAndRestart(t *testing.T) {
	cfg, source := legacyLayout(t)
	original := *cfg
	release, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkLocked := func() {
		t.Helper()
		other, e := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(source, "pimg.db"))+"?_pragma=busy_timeout(50)")
		if e != nil {
			t.Fatal(e)
		}
		defer other.Close()
		var n int
		e = other.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&n)
		if e == nil {
			t.Fatal("older process could reopen retained database during cutover")
		}
	}
	checkLocked()
	release()
	release, err = Prepare(&original)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	checkLocked()
}
