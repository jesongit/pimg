package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaCacheMaintenanceReportsActualChanges(t *testing.T) {
	s, _, _ := newTestEnv(t)
	dir := s.originalCacheDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Duration(s.OriginalCacheHours()+1) * time.Hour)
	for _, name := range []string{"expired-a.bin", "expired-b.mp4", "fresh.bin"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		if name != "fresh.bin" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	removed, err := s.CleanupMediaCacheResult()
	if err != nil || removed != 2 {
		t.Fatal(removed, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "fresh.bin")); err != nil {
		t.Fatal("maintenance removed live cache", err)
	}
	if removed, err = s.CleanupMediaCacheResult(); err != nil || removed != 0 {
		t.Fatal("empty maintenance cannot report changes", removed, err)
	}
	if err = s.CleanupMediaCache(); err != nil {
		t.Fatal("existing wrapper changed", err)
	}
}
