package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/mediax"
	"pimg/internal/store"
)

func expandedBrowserListing(t *testing.T, s *Server, root, query, ordering, snapshot string, offset int) store.BrowserListing {
	t.Helper()
	w := fileBrowserRequest(t, s)("GET", "/api/admin/file-browser?"+url.Values{"path": {root}, "q": {query}, "sort": {ordering}, "offset": {fmt.Sprint(offset)}, "recursive": {"1"}, "snapshot": {snapshot}}.Encode(), nil, "")
	var listing store.BrowserListing
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return listing
}

func TestFileBrowserRecursiveMediaSnapshotAndRefresh(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for _, name := range []string{"one/deep", "two", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 207; i++ {
		sub, ext := "one/deep", "png"
		if i%2 == 1 {
			sub, ext = "two", "mp4"
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(sub), fmt.Sprintf("media-%03d.%s", i, ext)), bytes.Repeat([]byte{1}, i+1), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"one", "two"} {
		os.WriteFile(filepath.Join(root, sub, "same.jpg"), []byte("same"), 0600)
		os.WriteFile(filepath.Join(root, sub, "notes.txt"), []byte("excluded"), 0600)
	}
	// Links inside the tree must not include outside files or loop forever.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "outside.png"), []byte("private"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Log("symlink creation unavailable on this host", err)
	}
	first := expandedBrowserListing(t, s, root, "", "size_desc", "", 0)
	if !first.Recursive || first.Total != 209 || first.NextOffset != 200 || first.Snapshot == "" || len(first.Items) != 200 {
		t.Fatal("bad recursive page", first.Total, first.NextOffset, first.Snapshot)
	}
	os.WriteFile(filepath.Join(root, "new.png"), []byte("new"), 0600)
	second := expandedBrowserListing(t, s, root, "", "size_desc", first.Snapshot, first.NextOffset)
	all := append(first.Items, second.Items...)
	if second.Total != 209 || second.NextOffset != 0 || len(all) != 209 {
		t.Fatal("snapshot changed while paging", second)
	}
	seen := map[string]bool{}
	for i, entry := range all {
		if entry.Directory || entry.Kind != "image" && entry.Kind != "video" || seen[entry.Path] || entry.Name == "outside.png" || !strings.Contains(entry.RelativePath, "/") {
			t.Fatal("nonmedia, unsafe, duplicate or unlocated result", entry)
		}
		seen[entry.Path] = true
		if i > 0 && all[i-1].Size < entry.Size {
			t.Fatal("sort is not global across folders/pages")
		}
	}
	refreshed := expandedBrowserListing(t, s, root, "", "size_desc", "", 0)
	if refreshed.Total != 210 || refreshed.Snapshot == first.Snapshot {
		t.Fatal("refresh reused stale scan")
	}
	search := expandedBrowserListing(t, s, root, "one/deep", "name", "", 0)
	if search.Total != 104 {
		t.Fatal("recursive search did not include relative paths", search.Total)
	}
	request := fileBrowserRequest(t, s)
	for _, query := range []url.Values{
		{"path": {root}, "recursive": {"2"}},
		{"path": {root}, "recursive": {"1"}, "q": {"different"}, "sort": {"size_desc"}, "snapshot": {refreshed.Snapshot}, "offset": {"200"}},
	} {
		if w := request("GET", "/api/admin/file-browser?"+query.Encode(), nil, ""); w.Code != 400 {
			t.Fatal("invalid recursive scope accepted", w.Code)
		}
	}
	// Listing never registers a managed file or an album for the source tree.
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM files`).Scan(&count); err != nil || count != 0 {
		t.Fatal("browsing created managed media", count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Svc.BrowseLocalFiles(ctx, root, "", 0, "created_asc", store.BrowserOptions{Recursive: true}); err == nil {
		t.Fatal("canceled recursive scan continued")
	}
}

func TestFileBrowserCreationAndModificationOrdering(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "folder"), 0700)
	names := []string{"z-first.png", "a-second.png", "m-third.png"}
	created := map[string]int64{}
	base := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for i, name := range names {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, interactionPNG(t), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if birth, ok := mediax.FileCreatedTime(file); ok {
			created[name] = birth.UnixNano()
		}
		file.Close()
		mod := base.Add(time.Duration(2-i) * time.Hour)
		if err = os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		// Birth ordering differs from names and the deliberately reversed mtime.
		time.Sleep(20 * time.Millisecond)
	}
	for ordering, expected := range map[string][]string{
		"modified_asc":  {"m-third.png", "a-second.png", "z-first.png"},
		"modified_desc": {"z-first.png", "a-second.png", "m-third.png"},
	} {
		listing := browserListing(t, s, root, "", 0, ordering)
		if !listing.Items[0].Directory {
			t.Fatal("time sort moved folders behind files")
		}
		for i, name := range expected {
			if listing.Items[i+1].Name != name {
				t.Fatal(ordering, listing.Items)
			}
		}
	}
	if len(created) == len(names) {
		for _, ordering := range []string{"created_asc", "created_desc"} {
			listing := browserListing(t, s, root, "", 0, ordering)
			for i := 1; i < len(listing.Items); i++ {
				entry := listing.Items[i]
				if entry.CreatedAt != time.Unix(0, created[entry.Name]).Unix() {
					t.Fatal("creation time was replaced with modification time", entry)
				}
				if i > 1 {
					previous := listing.Items[i-1]
					if (created[previous.Name] > created[entry.Name]) == (ordering == "created_asc") {
						t.Fatal("creation ordering", ordering, listing.Items)
					}
				}
			}
		}
	}
}
