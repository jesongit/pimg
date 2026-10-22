package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/config"
	"pimg/internal/store"
)

func TestCLIReviewRestoreUsesVerifiedHealthyReplica(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{MasterKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Storage: config.Storage{Cold: "local", Hot: "local"}, Upload: config.Upload{MaxSizeMB: 10, AllowedExts: []string{"png"}}}
	svc := mustService(cfg)
	peer, err := svc.SaveStorageInstance("", "second archive", "local", store.InstanceConfig{Directory: t.TempDir()}, true)
	if err != nil {
		t.Fatal(err)
	}
	p := svc.StoragePolicy()
	p.Archives = []string{"local", peer}
	p.NormalMB = 1024
	p.AcceleratedMB = 1024
	if err = svc.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err = png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	f, err := legacyUploadFixture(svc, context.Background(), bytes.NewReader(plain.Bytes()), "restore.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = svc.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = svc.DB.GetFile(f.ID)
	svc.ForgetMediaViews(f)
	cs, _ := svc.DB.ListChunks(f.ID, "data")
	if err = os.WriteFile(filepath.Join(cfg.Storage.LocalColdDir, filepath.FromSlash(cs[0].ObjectKey)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.StopMediaLoads()
	svc.DB.Close()
	output := filepath.Join(t.TempDir(), "restored.png")
	if err = runArchiveCLI(cfg, "restore", []string{f.ID, output}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, plain.Bytes()) {
		t.Fatal("CLI restored unverified or wrong bytes", err)
	}
}
