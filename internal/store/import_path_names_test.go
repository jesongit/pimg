package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestImportRelativePathValidationMatchesHostFilesystem(t *testing.T) {
	for _, p := range []string{"", ".", "目录/photo.png", "space #[]%/x.png"} {
		if _, err := cleanImportPath(p); err != nil {
			t.Fatalf("rejected ordinary relative path %q: %v", p, err)
		}
	}
	for _, p := range []string{"..", "../outside", "a/../outside", "/absolute", "a//b", "a/./b", "a/", "nul\x00name"} {
		if _, err := cleanImportPath(p); err == nil {
			t.Fatalf("accepted invalid relative path %q", p)
		}
	}
	for _, p := range []string{"2026:10:24/photo.png", `back\slash/photo.png`, "file:stream", `..\literal-name.png`, `C:literal-name.png`, string([]byte{0xff})} {
		_, err := cleanImportPath(p)
		if (err != nil) != (runtime.GOOS == "windows") {
			t.Fatalf("host-specific filename handling for %q: %v", p, err)
		}
	}
}

func TestBrowserIndexLinuxFilenameCharactersInitialScanRestartAndEvents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux accepts colons and backslashes as filename characters")
	}
	s, source := indexTestService(t)
	directories := []string{"2026:10:24", `back\slash`, "中文:目录"}
	paths := []string{}
	for _, directory := range directories {
		child := filepath.Join(source, directory, "深层:目录")
		if err := os.MkdirAll(child, 0700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(child, `photo:one\two.png`)
		if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			s.ApplyBrowserWatchPaths([]string{source})
			indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Ready && !root.Updating && root.Error == "" })
		}
		listing, err := s.BrowseLocalFiles(t.Context(), source, `back\slash`, 0, "name", BrowserOptions{Recursive: true})
		if err != nil || listing.Total != 1 || listing.Items[0].Path != paths[1] {
			t.Fatal("literal backslash filename search", indexed, listing, err)
		}
	}
	// Scan the source's parent as well as directories containing special bytes.
	// Both forms must work with the same source-open/version checks.
	for _, watched := range append([]string{source}, filepath.Dir(paths[0])) {
		s.ApplyBrowserWatchPaths([]string{watched})
		indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Ready && !root.Updating && root.Error == "" })
		listing, err := s.BrowseLocalFiles(t.Context(), watched, "", 0, "name", BrowserOptions{Recursive: true})
		if err != nil || !listing.Indexed || len(listing.Items) == 0 {
			t.Fatal(listing, err)
		}
		for _, entry := range listing.Items {
			file, _, err := s.OpenBrowserSource(entry.Path, entry.Version)
			if err != nil {
				t.Fatal(entry.Path, err)
			}
			file.Close()
		}
	}
	s.ApplyBrowserWatchPaths([]string{source})
	indexWait(t, s, func(root BrowserIndexRoot) bool {
		return root.Ready && !root.Updating && root.Entries == 9 && root.Error == ""
	})
	if err := os.WriteFile(paths[0], []byte("modified original"), 0600); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(root BrowserIndexRoot) bool {
		listing, err := s.BrowseLocalFiles(t.Context(), filepath.Dir(paths[0]), "", 0, "name")
		return err == nil && len(listing.Items) == 1 && listing.Items[0].Size == 17 && root.Error == "" && !root.Updating
	})
	newDirectory := filepath.Join(source, "新增:目录", `子\目录`)
	if err := os.MkdirAll(newDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(newDirectory, "new:photo.png")
	if err := os.WriteFile(newFile, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Entries == 12 && !root.Updating && root.Error == "" })
	if err := os.Remove(newFile); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Entries == 11 && !root.Updating && root.Error == "" })
	renamed := filepath.Join(source, `更名:目录\新`)
	if err := os.Rename(filepath.Dir(newDirectory), renamed); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(root BrowserIndexRoot) bool {
		listing, err := s.BrowseLocalFiles(t.Context(), source, "更名", 0, "name")
		return err == nil && len(listing.Items) == 1 && listing.Items[0].Path == renamed && root.Entries == 11 && !root.Updating && root.Error == ""
	})
	s.StopBrowserIndex()
	s.ApplyBrowserWatchPaths([]string{source})
	indexWait(t, s, func(root BrowserIndexRoot) bool {
		return root.Ready && root.Entries == 11 && !root.Updating && root.Error == ""
	})
	for _, p := range paths[1:] {
		body, err := os.ReadFile(p)
		if err != nil || string(body) != "original" {
			t.Fatal("index changed original", p, err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "link:outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportPathInfo(filepath.Join(link, "secret.png")); err == nil {
		t.Fatal("special-name handling allowed a symbolic-link escape")
	}
	if _, err := cleanImportPath("../outside"); err == nil {
		t.Fatal("relative traversal accepted")
	}
}
