package store

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/mediax"
)

func writeZip(t *testing.T, zipPath string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, body := range entries {
		wr, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = wr.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractSingleFolderCollapsesToTarget(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z unavailable")
	}
	s, _, _ := newTestEnv(t)
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "album.zip")
	writeZip(t, zipPath, map[string]string{"album/a.jpg": "a", "album/b.jpg": "b"})
	job, err := s.StartExtract(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	waitExtract(t, s, job)
	if !job.Ok {
		t.Fatal("extract failed", job.Error, job)
	}
	// Single folder "album" should land directly as <base>/album
	if _, err := os.Stat(filepath.Join(dir, "album", "a.jpg")); err != nil {
		t.Fatal("single-folder zip did not collapse to target", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".pimg-extract-album.zip")); err == nil {
		t.Fatal("queue temp not cleaned")
	}
}

func TestExtractMultiFileCreatesNamedFolder(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z unavailable")
	}
	s, _, _ := newTestEnv(t)
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "docs.zip")
	writeZip(t, zipPath, map[string]string{"a.jpg": "a", "b.jpg": "b"})
	job, err := s.StartExtract(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	waitExtract(t, s, job)
	if !job.Ok {
		t.Fatal("extract failed", job.Error)
	}
	// Multiple top entries -> create a folder named after the archive
	if _, err := os.Stat(filepath.Join(dir, "docs", "a.jpg")); err != nil {
		t.Fatal("multi-file zip did not create named folder", err)
	}
}

func waitExtract(t *testing.T, s *Service, job *extractJob) {
	t.Helper()
	if job == nil {
		t.Fatal("nil extract job")
	}
	select {
	case <-job.done:
	case <-time.After(30 * time.Second):
		t.Fatal("extract timeout")
	}
}

func TestAudioExtMapping(t *testing.T) {
	for _, ext := range []string{"mp3", "flac", "wav", "m4a", "aac", "ogg", "opus", "wma", "aiff"} {
		if !mediax.AudioExt(ext) {
			t.Fatalf("%s should be audio", ext)
		}
	}
	if mediax.AudioExt("mp4") || mediax.AudioExt("zip") {
		t.Fatal("non-audio wrongly reported")
	}
	if !ArchiveFile("rar") || !ArchiveFile("zip") || ArchiveFile("mp4") {
		t.Fatal("archive mapping")
	}
}