package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func TestCLIRetiredArchiveGate(t *testing.T) {
	if command := os.Getenv("PIMG_TEST_RETIRED_COMMAND"); command != "" {
		args := []string{"retired"}
		if command == "restore" {
			args = append(args, "restored.bin")
		}
		runOffline(command, args)
		return
	}
	dir := t.TempDir()
	data := []byte("retired stored bytes remain available on disk")
	digest := sha256.Sum256(data)
	db, err := dbx.Open(filepath.Join(dir, "data", "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	file := &dbx.File{ID: "retired", Name: "retired", OrigName: "retired.bin", Ext: "bin", Mime: "application/octet-stream", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", digest), Kind: "other", KeyWrapped: []byte{1}, KeyNonce: []byte{1}, Storage: "direct", ColdBackend: "local", ArchiveState: "done"}
	if err := db.InsertFile(file); err != nil {
		t.Fatal(err)
	}
	disk, err := storage.NewDisk(filepath.Join(dir, "data", "cold"))
	if err != nil {
		t.Fatal(err)
	}
	key := "retired/data-0000.bin"
	if err := disk.Put(context.Background(), key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChunk(&dbx.Chunk{FileID: file.ID, Kind: "data", Idx: 0, ObjectKey: key, Size: int64(len(data)), SHA256: file.SHA256}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE files SET hidden=1 WHERE id=?", file.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	config := "storage:\n  cold: local\n  hot: local\npaths:\n  data_dir: ./data\n  temp_dir: ./data/tmp\nmaster_key: " + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var environment []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "PIMG_") {
			environment = append(environment, variable)
		}
	}
	for _, command := range []string{"verify", "restore"} {
		child := exec.Command(executable, "-test.run=^TestCLIRetiredArchiveGate$")
		child.Dir = dir
		child.Env = append(environment, "PIMG_CONFIG=config.yaml", "PIMG_TEST_RETIRED_COMMAND="+command)
		output, err := child.CombinedOutput()
		if err == nil || !strings.Contains(string(output), dbx.ErrNotFound.Error()) {
			t.Fatalf("CLI %s accessed retired storage or failed for an unrelated reason: %v %s", command, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "restored.bin")); !os.IsNotExist(err) {
		t.Fatal("CLI restore created an output for a retired archive", err)
	}
}
