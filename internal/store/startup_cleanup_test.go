package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStartupCleanupReclaimsCrashScratchAndKeepsRetryData(t *testing.T) {
	s, f := readinessFixture(t)
	if err := s.DB.SetArchiveState(f.ID, "failed", "interrupted upload"); err != nil {
		t.Fatal(err)
	}
	// Durable ciphertext and plaintext belong to the retryable archive. Scratch
	// copies have no persisted resume position and must be regenerated instead.
	durable := []string{s.ctPath(f.ID), s.ctDonePath(f.ID), s.plainPath(f.ID)}
	for _, path := range durable {
		if err := os.WriteFile(path, []byte("archive retry checkpoint"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var scratch []string
	for _, pattern := range []string{"spill-*.bin", "peer-*.part"} {
		file, err := os.CreateTemp(s.ctDir, pattern)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(bytes.Repeat([]byte("interrupted scratch"), 100)); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		scratch = append(scratch, file.Name())
	}
	untouched := []string{
		filepath.Join(s.ctDir, "notes.bin"),
		filepath.Join(s.ctDir, "peer-not-scratch.bin"),
		filepath.Join(s.ctDir, "spill-not-scratch.txt"),
		filepath.Join(s.ctDir, "peer-directory.part", "retained.bin"),
		filepath.Join(s.ctDir, "nested", "spill-123.bin"),
	}
	for _, path := range untouched {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("retain"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A new Service plus its startup cleanup simulates a process that never ran
	// the old process's deferred Close/Remove calls.
	restarted, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	restarted.CleanupLocalTemps()
	restarted.CleanupLocalTemps() // Another restart must be harmless.
	for _, path := range scratch {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("crash scratch remained after startup: %s (%v)", filepath.Base(path), err)
		}
	}
	for _, path := range durable {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "archive retry checkpoint" {
			t.Fatalf("startup discarded archive retry data: %s (%v)", filepath.Base(path), err)
		}
	}
	for _, path := range untouched {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "retain" {
			t.Fatalf("startup touched unrelated or nested data: %s (%v)", path, err)
		}
	}
}
