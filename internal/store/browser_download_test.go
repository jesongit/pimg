package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBrowserDownloadAllowsRegularFilesWithoutEnablingPreview(t *testing.T) {
	s, source := indexTestService(t)
	for _, name := range []string{"notes.txt", "archive.zip", "photo.png", "movie.mp4"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(source, name)
			original := []byte("original bytes for " + name)
			if err := os.WriteFile(p, original, 0600); err != nil {
				t.Fatal(err)
			}
			entry, err := s.BrowserEntryInfo(p)
			if err != nil {
				t.Fatal(err)
			}
			file, opened, err := s.OpenBrowserDownload(p, entry.Version)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(data, original) || opened.Path != p || opened.Version != entry.Version {
				t.Fatal("download changed original bytes", opened, readErr, closeErr)
			}
			if entry.Kind == "other" {
				if file, _, err := s.OpenBrowserSource(p, entry.Version); err == nil {
					file.Close()
					t.Fatal("nonmedia preview source opened")
				}
				for _, mode := range []string{"", "image", "remux", "transcode"} {
					if file, _, err := s.OpenBrowserContent(p, entry.Version, mode); err == nil {
						file.Close()
						t.Fatal("nonmedia content opened", mode)
					}
				}
			}
			if err := os.WriteFile(p, append(original, '!'), 0600); err != nil {
				t.Fatal(err)
			}
			if file, _, err := s.OpenBrowserDownload(p, entry.Version); !errors.Is(err, ErrSourceChanged) {
				if file != nil {
					file.Close()
				}
				t.Fatal("download accepted an outdated version", err)
			}
		})
	}
}

func TestOpenBrowserDownloadRejectsDirectoryMissingAndEmptyVersion(t *testing.T) {
	s, source := indexTestService(t)
	p := filepath.Join(source, "notes.txt")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, version string }{{source, "directory"}, {filepath.Join(source, "missing.txt"), "missing"}, {p, ""}} {
		if file, _, err := s.OpenBrowserDownload(test.path, test.version); err == nil {
			file.Close()
			t.Fatal("invalid download source opened", test)
		}
	}
}

func TestOpenBrowserDownloadRejectsSymlinkLeafAndAncestor(t *testing.T) {
	s, source := indexTestService(t)
	outside := t.TempDir()
	p := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := s.BrowserEntryInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fileLink := filepath.Join(source, "linked.txt")
	if err := os.Symlink(p, fileLink); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(link, "notes.txt"), fileLink} {
		if file, _, err := s.OpenBrowserDownload(p, entry.Version); err == nil {
			file.Close()
			t.Fatal("download followed a symbolic link", p)
		}
	}
}
