package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/dbx"
)

func indexTestService(t *testing.T) (*Service, string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	s := &Service{Cfg: &config.Config{Paths: config.Paths{DataDir: filepath.Join(base, "data"), RuntimeDir: filepath.Join(base, "runtime")}}}
	s.SetDynamic(24, 30, 0, []string{"png", "mp4"})
	t.Cleanup(s.StopBrowserIndex)
	return s, source
}

func indexWait(t *testing.T, s *Service, condition func(BrowserIndexRoot) bool) BrowserIndexRoot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var state BrowserIndexStatus
	for time.Now().Before(deadline) {
		state = s.BrowserIndexStatus()
		if state.Error != "" {
			t.Fatal(state.Error)
		}
		if len(state.Roots) == 1 && condition(state.Roots[0]) {
			return state.Roots[0]
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("index did not converge: %+v", state)
	return BrowserIndexRoot{}
}

func TestBrowserIndexDefaultIdleAndValidation(t *testing.T) {
	s, source := indexTestService(t)
	s.ApplyBrowserWatchPaths(nil)
	if len(s.BrowserWatchPaths()) != 0 || len(s.BrowserIndexStatus().Roots) != 0 || s.browserIndex.runner != nil {
		t.Fatal("empty configuration started work")
	}
	if _, err := os.Stat(s.browserIndexPath()); !os.IsNotExist(err) {
		t.Fatal("default created an index file", err)
	}
	child := filepath.Join(source, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{"relative"}, {source, ""}, {filepath.Dir(source)}, {filepath.VolumeName(source) + string(filepath.Separator)}, {filepath.Join(source, "missing")}, {filepath.Dir(s.browserIndexPath())}} {
		if _, err := s.ValidateBrowserWatchPaths(paths); err == nil {
			t.Fatalf("accepted invalid roots: %q", paths)
		}
	}
	paths, err := s.ValidateBrowserWatchPaths([]string{child, source, source})
	if err != nil || !reflect.DeepEqual(paths, []string{source}) {
		t.Fatal(paths, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(source, link); err == nil {
		if _, err := s.ValidateBrowserWatchPaths([]string{link}); err == nil {
			t.Fatal("accepted symbolic link")
		}
	}
}

func TestBrowserIndexQueriesMatchLiveSortsAndPageRevisions(t *testing.T) {
	s, source := indexTestService(t)
	for _, name := range []string{"z-folder", "A-folder", "中文_子目录"} {
		if err := os.Mkdir(filepath.Join(source, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 415; i++ {
		name := fmt.Sprintf("%03d.png", i)
		if err := os.WriteFile(filepath.Join(source, name), make([]byte, i%13+1), 0600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1700000000+int64(i%11), int64(i))
		if err := os.Chtimes(filepath.Join(source, name), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"z-folder/nested.mp4", "中文_子目录/中文_文件.png", "A-folder/skip.dat"} {
		if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(name)), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sorts := []string{"name", "name_desc", "size_asc", "size_desc", "type", "type_desc", "created_asc", "created_desc", "modified_asc", "modified_desc"}
	live := map[string][]BrowserEntry{}
	for _, order := range sorts {
		listing, err := s.BrowseLocalFiles(t.Context(), source, "", 0, order)
		if err != nil {
			t.Fatal(err)
		}
		live[order] = listing.Items
	}
	s.ApplyBrowserWatchPaths([]string{source})
	indexWait(t, s, func(root BrowserIndexRoot) bool {
		return root.Ready && !root.Updating && root.Error == "" && root.Entries == 421
	})
	var first BrowserListing
	for _, order := range sorts {
		indexed, err := s.BrowseLocalFiles(t.Context(), source, "", 0, order)
		if err != nil || !indexed.Indexed || indexed.Total != 418 || len(indexed.Items) != 200 {
			t.Fatal(order, indexed.Total, err)
		}
		for i, e := range indexed.Items {
			if e.Name != live[order][i].Name {
				t.Fatalf("%s position %d: indexed %q, live %q", order, i, e.Name, live[order][i].Name)
			}
		}
		first = indexed
	}
	page, err := s.BrowseLocalFiles(t.Context(), source, "", 200, "modified_desc", BrowserOptions{Snapshot: first.Snapshot})
	if err != nil || len(page.Items) != 200 {
		t.Fatal(err)
	}
	if _, err = s.BrowseLocalFiles(t.Context(), source, "", 200, "name", BrowserOptions{Snapshot: first.Snapshot}); err == nil {
		t.Fatal("accepted token from different ordering")
	}
	flat, err := s.BrowseLocalFiles(t.Context(), source, "中文_", 0, "name", BrowserOptions{Recursive: true})
	if err != nil || flat.Total != 1 || flat.Items[0].RelativePath != "中文_子目录/中文_文件.png" {
		t.Fatal(flat, err)
	}
	other := t.TempDir()
	outside, err := s.BrowseLocalFiles(t.Context(), other, "", 0, "name")
	if err != nil || outside.Indexed {
		t.Fatal("unconfigured directory used index", err)
	}
	if err := os.WriteFile(filepath.Join(source, "added.png"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Entries == 422 && !root.Updating })
	if _, err = s.BrowseLocalFiles(t.Context(), source, "", 200, "modified_desc", BrowserOptions{Snapshot: first.Snapshot}); err == nil {
		t.Fatal("accepted stale page revision")
	}
}

func TestBrowserIndexNativeChangesRestartAndDisable(t *testing.T) {
	s, source := indexTestService(t)
	db, err := dbx.Open(filepath.Join(s.Cfg.Paths.DataDir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	s.DB = db
	t.Cleanup(func() { s.StopBrowserIndex(); db.Close() })
	encoded, _ := json.Marshal([]string{source})
	if _, err := db.Exec(`INSERT INTO settings(k,v) VALUES('site.browser_watch_paths',?)`, string(encoded)); err != nil {
		t.Fatal(err)
	}
	s.restoreBrowserIndex()
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Ready && !r.Updating })
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, p), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("one.png", "first")
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Entries == 1 && !r.Updating })
	before, err := s.BrowseLocalFiles(t.Context(), source, "", 0, "name")
	if err != nil {
		t.Fatal(err)
	}
	write("one.png", "changed payload")
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Revision > 1 && !r.Updating })
	deadline := time.Now().Add(5 * time.Second)
	for {
		after, e := s.BrowseLocalFiles(t.Context(), source, "", 0, "name")
		if e != nil {
			t.Fatal(e)
		}
		if len(after.Items) == 1 && after.Items[0].Version != before.Items[0].Version {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("modified file version did not update")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if file, _, e := s.OpenBrowserSource(before.Items[0].Path, before.Items[0].Version); e == nil {
		file.Close()
		t.Fatal("stale index authorized source content")
	}
	if err := os.Rename(filepath.Join(source, "one.png"), filepath.Join(source, "renamed.png")); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Entries == 1 && !r.Updating })
	nested := filepath.Join(source, "newdir", "child")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("newdir", "child", "nested.png"), "nested")
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Entries == 4 && !r.Updating })
	// This test deletes only an explicitly owned test subtree.
	if err := os.Remove(filepath.Join(nested, "nested.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "newdir")); err != nil {
		t.Fatal(err)
	}
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Entries == 1 && !r.Updating })
	s.StopBrowserIndex()
	write("offline.mp4", "added while service stopped")
	s.restoreBrowserIndex()
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Ready && r.Entries == 2 && !r.Updating })
	s.ApplyBrowserWatchPaths([]string{})
	listing, err := s.BrowseLocalFiles(t.Context(), source, "", 0, "name")
	if err != nil || listing.Indexed || listing.Total != 2 || len(s.BrowserWatchPaths()) != 0 {
		t.Fatal("disable did not restore live browsing", listing, err)
	}
	body, err := os.ReadFile(filepath.Join(source, "renamed.png"))
	if err != nil || string(body) != "changed payload" {
		t.Fatal("source changed", err)
	}
}

func TestBrowserIndexConcurrentApplyReadStop(t *testing.T) {
	s, source := indexTestService(t)
	if err := os.WriteFile(filepath.Join(source, "file.png"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for ctx.Err() == nil {
				_, _ = s.BrowseLocalFiles(ctx, source, "", 0, "name")
				_ = s.BrowserIndexStatus()
			}
		})
	}
	for range 3 {
		s.ApplyBrowserWatchPaths([]string{source})
		s.RefreshBrowserIndex(source)
		s.ApplyBrowserWatchPaths(nil)
	}
	cancel()
	wg.Wait()
}

func TestBrowserIndexRapidStopWhileScanning(t *testing.T) {
	s, source := indexTestService(t)
	for i := range 64 {
		if err := os.Mkdir(filepath.Join(source, fmt.Sprint(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range 30 {
			s.ApplyBrowserWatchPaths([]string{source})
			time.Sleep(time.Millisecond)
			s.ApplyBrowserWatchPaths(nil)
		}
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("stopping an active scan did not finish")
	}
	if len(s.BrowserIndexStatus().Roots) != 0 {
		t.Fatal("stopped scanner retained configured roots")
	}
}

func TestBrowserIndexRecursiveDuplicateNamesKeepOriginalPathOrder(t *testing.T) {
	s, source := indexTestService(t)
	for _, folder := range []string{"a-folder", "B-folder"} {
		if err := os.Mkdir(filepath.Join(source, folder), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, folder, "same.png"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	options := BrowserOptions{Recursive: true}
	want, err := s.BrowseLocalFiles(t.Context(), source, "", 0, "name", options)
	if err != nil {
		t.Fatal(err)
	}
	s.ApplyBrowserWatchPaths([]string{source})
	indexWait(t, s, func(root BrowserIndexRoot) bool { return root.Ready && !root.Updating && root.Entries == 4 })
	got, err := s.BrowseLocalFiles(t.Context(), source, "", 0, "name", options)
	if err != nil || len(got.Items) != 2 {
		t.Fatal(got, err)
	}
	for i := range want.Items {
		if got.Items[i].RelativePath != want.Items[i].RelativePath {
			t.Fatal("case-folded path changed ordering", got.Items, want.Items)
		}
	}
}
