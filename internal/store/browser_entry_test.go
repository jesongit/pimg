package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserEntryInfoWithoutIndexAndDuringRebuild(t *testing.T) {
	s, source := indexTestService(t)
	p := filepath.Join(source, "saved.mp4")
	if err := os.WriteFile(p, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := s.BrowserEntryInfo(p)
	if err != nil || before.Path != p || before.Size != 5 || before.Kind != "video" || before.MIME != "video/mp4" || before.Version == "" || !before.Importable {
		t.Fatal(before, err)
	}
	if _, err := os.Stat(s.browserIndexPath()); !os.IsNotExist(err) {
		t.Fatal("metadata lookup created an index", err)
	}

	// Publish a fresh index before its scanner starts: no scheduling race or
	// large source tree is needed to exercise the unready rebuild state.
	r, err := s.newBrowserIndexer([]string{source})
	if err != nil {
		t.Fatal(err)
	}
	s.browserIndex.paths, s.browserIndex.runner = []string{source}, r
	t.Cleanup(func() {
		s.browserIndex.runner = nil
		r.cancel()
		if r.watch != nil {
			_ = r.watch.Close()
		}
		_ = r.db.Close()
	})
	listing, err := s.BrowseLocalFiles(t.Context(), source, "", 0, "name")
	if err != nil || !listing.Indexed || !listing.IndexBuilding || listing.Total != 0 {
		t.Fatal("test did not establish an empty rebuilding index", listing, err)
	}
	if err := os.WriteFile(p, []byte("changed metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := s.BrowserEntryInfo(p)
	if err != nil || after.Size != 16 || after.Version == before.Version || after.Path != p {
		t.Fatal("lookup depended on the rebuilding index or stale metadata", after, err)
	}
	state := s.BrowserIndexStatus()
	if len(state.Roots) != 1 || state.Roots[0].Ready || state.Roots[0].Entries != 0 {
		t.Fatal("lookup populated the index", state)
	}
}

func TestBrowserEntryInfoDirectoryRootAndInvalidSources(t *testing.T) {
	s, source := indexTestService(t)
	for _, p := range []string{source, filepath.VolumeName(source) + string(filepath.Separator)} {
		entry, err := s.BrowserEntryInfo(p)
		if err != nil || entry.Path != p || !entry.Directory || entry.Importable {
			t.Fatal(entry, err)
		}
	}
	for _, p := range []string{"", filepath.Join(source, "missing.mp4"), "pimg-path://invalid"} {
		if entry, err := s.BrowserEntryInfo(p); err == nil {
			t.Fatal("accepted invalid source", p, entry)
		}
	}
	p := filepath.Join(source, "removed.mp4")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BrowserEntryInfo(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BrowserEntryInfo(p); !os.IsNotExist(err) {
		t.Fatal("missing source reused metadata", err)
	}
}

func TestBrowserEntryInfoRejectsSymlinkSources(t *testing.T) {
	s, source := indexTestService(t)
	outside := t.TempDir()
	p := filepath.Join(outside, "saved.mp4")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fileLink := filepath.Join(source, "linked.mp4")
	if err := os.Symlink(p, fileLink); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, filepath.Join(link, "saved.mp4"), fileLink} {
		if _, err := s.BrowserEntryInfo(p); err == nil {
			t.Fatal("followed a symbolic link", p)
		}
	}
}
