package main

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/config"
	"pimg/internal/dbx"
)

func TestCLIUserTelegramRequiresExistingAuthorizedSession(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{MasterKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Storage: config.Storage{Cold: "local", Hot: "local"}}
	db, err := dbx.Open(filepath.Join(dir, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &dbx.File{ID: "tg-file", Name: "fixture", OrigName: "fixture.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 1, SHA256: strings.Repeat("0", 64), KeyWrapped: []byte{1}, KeyNonce: []byte{1}, Storage: "direct", ColdBackend: "telegram", ArchiveState: "done"}
	if err := db.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for _, command := range []string{"verify", "restore"} {
		args := []string{f.ID}
		if command == "restore" {
			args = append(args, filepath.Join(dir, "restored.png"))
		}
		if err := runArchiveCLI(cfg, command, args); err == nil || !strings.Contains(err.Error(), "网页完成登录") {
			t.Fatal("CLI did not stop before real authentication", command, err)
		}
	}
}
