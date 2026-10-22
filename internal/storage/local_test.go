package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalConfinementAndAtomicWrite(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"../outside", "/absolute", "..\\outside", "temp/../../outside"} {
		if err := d.Put(ctx, key, strings.NewReader("bad"), 3); err == nil {
			t.Fatalf("escaped disk root with %q", key)
		}
	}
	if err := d.Put(ctx, "safe/file", strings.NewReader("old"), 3); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "safe/file", strings.NewReader("short"), 100); err == nil {
		t.Fatal("truncated write accepted")
	}
	b, err := os.ReadFile(filepath.Join(d.Dir, "safe", "file"))
	if err != nil || string(b) != "old" {
		t.Fatal("failed write destroyed previous data", err)
	}
	if err := d.Delete("missing/object"); err != nil {
		t.Fatal("delete must be idempotent", err)
	}
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "outside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected file")
	}
}
