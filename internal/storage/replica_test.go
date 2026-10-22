package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalReplicaAtomicHashCheckedWritesAndStableAlbumReferences(t *testing.T) {
	l := LocalReplica{DataDir: t.TempDir(), Directory: "replicas/a"}
	ctx := context.Background()
	album, err := l.EnsureAlbum(ctx, ReplicaAlbum{ID: 7, Name: "旅行 / 照片"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("original encrypted bytes")
	sum := sha256.Sum256(data)
	obj := ReplicaObject{Name: "data-0000.png", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sum)}
	planned, err := l.Prepare(ctx, album, "file-1", obj)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := l.Put(ctx, album, "file-1", obj, bytes.NewReader(data))
	if err != nil || ref != planned {
		t.Fatal(ref, err)
	}
	if _, err = l.Put(ctx, album, "file-1", obj, bytes.NewReader([]byte("corrupt"))); err == nil {
		t.Fatal("partial write committed")
	}
	if _, err = l.EnsureAlbum(ctx, ReplicaAlbum{ID: 7, Name: "改名后的相册"}); err != nil {
		t.Fatal(err)
	}
	r, err := l.Open(ctx, ref)
	if err != nil {
		t.Fatal("rename invalidated a stable reference", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("failed replacement changed original")
	}
	entries, _ := os.ReadDir(filepath.Join(l.DataDir, l.Directory, "albums"))
	if len(entries) != 1 || entries[0].Name() != "7--改名后的相册" {
		t.Fatal("album classification/rename differs", entries)
	}
	if err = l.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err = l.Delete(ctx, ref); err != nil {
		t.Fatal("delete not idempotent", err)
	}
	if _, err = l.Open(ctx, "7/../../escape"); err == nil {
		t.Fatal("reference escaped its root")
	}
	if matches, _ := filepath.Glob(filepath.Join(l.DataDir, l.Directory, "albums", "*", "file-1", "*.part")); len(matches) != 0 {
		t.Fatal("failed write left temporary files")
	}
}
