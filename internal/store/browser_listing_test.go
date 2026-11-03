package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFolderSortingFollowsDateKeys(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	folders := []BrowserEntry{
		{ImportPath: ImportPath{Name: "old", Directory: true}, modifiedNS: base, createdNS: base},
		{ImportPath: ImportPath{Name: "new", Directory: true}, modifiedNS: base + 200, createdNS: base + 200},
		{ImportPath: ImportPath{Name: "mid", Directory: true}, modifiedNS: base + 100, createdNS: base + 100},
	}
	expect := func(ordering string, want ...string) {
		t.Helper()
		items := append([]BrowserEntry(nil), folders...)
		sortBrowserEntries(items, ordering)
		for i, item := range items {
			if item.Name != want[i] {
				t.Fatalf("%s: got %v, want %v", ordering, names(items), want)
			}
		}
	}
	expect("modified_desc", "new", "mid", "old")
	expect("modified_asc", "old", "mid", "new")
	expect("created_desc", "new", "mid", "old")
	expect("created_asc", "old", "mid", "new")
	expect("size_desc", "mid", "new", "old") // No folder sizes: name fallback.
	// Files still group after folders under every key.
	items := append([]BrowserEntry(nil), folders...)
	items = append(items, BrowserEntry{ImportPath: ImportPath{Name: "a.jpg"}, modifiedNS: base + 900})
	sortBrowserEntries(items, "modified_desc")
	if items[len(items)-1].Name != "a.jpg" {
		t.Fatal("directories must still group first", names(items))
	}
	// A folder without a birth time stays last among folders.
	items = append([]BrowserEntry(nil), folders...)
	items = append(items, BrowserEntry{ImportPath: ImportPath{Name: "unknown", Directory: true}})
	sortBrowserEntries(items, "created_desc")
	if items[len(items)-1].Name != "unknown" {
		t.Fatal("missing folder birth time must stay last", names(items))
	}
}

func names(items []BrowserEntry) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Name
	}
	return out
}

func TestBrowserWindowMatchesWholeDirectoryBeyondOldEntryLimit(t *testing.T) {
	for _, order := range []string{"name", "name_desc", "size_asc", "size_desc", "type", "type_desc", "modified_asc", "modified_desc", "created_asc", "created_desc"} {
		t.Run(order, func(t *testing.T) {
			window := browserWindow{ordering: order, limit: 400}
			all := make([]BrowserEntry, 60_001)
			for i := range all {
				e := BrowserEntry{ImportPath: ImportPath{Path: fmt.Sprintf("/source/%06d.jpg", i), Name: fmt.Sprintf("%06d.jpg", 60_000-i), Size: int64(i % 503), Directory: i%1000 == 0}, Ext: []string{"jpg", "png"}[i%2], modifiedNS: int64(i % 313), createdNS: int64(i % 17)}
				all[i] = e
				if err := window.add(e); err != nil {
					t.Fatal(err)
				}
			}
			sortBrowserEntries(all, order)
			sortBrowserEntries(window.items, order)
			if len(window.items) != 400 || window.bytes > 1<<20 {
				t.Fatal("unbounded page", len(window.items), window.bytes)
			}
			for i := range window.items {
				if window.items[i].Path != all[i].Path {
					t.Fatal("page differs from global sort", i)
				}
			}
		})
	}
}

func TestBrowserCreationSortUnknownLastAndDuplicateNamesStable(t *testing.T) {
	for _, ordering := range []string{"created_asc", "created_desc"} {
		items := []BrowserEntry{
			{ImportPath: ImportPath{Name: "unknown.jpg"}},
			{ImportPath: ImportPath{Name: "same.jpg"}, RelativePath: "z/same.jpg", createdNS: 100},
			{ImportPath: ImportPath{Name: "same.jpg"}, RelativePath: "a/same.jpg", createdNS: 100},
			{ImportPath: ImportPath{Name: "new.jpg"}, createdNS: 200},
		}
		sortBrowserEntries(items, ordering)
		if items[3].Name != "unknown.jpg" {
			t.Fatal("missing birth time must stay last", ordering, items)
		}
		paths := []string{}
		for _, item := range items {
			if item.Name == "same.jpg" {
				paths = append(paths, item.RelativePath)
			}
		}
		if strings.Join(paths, ",") != "a/same.jpg,z/same.jpg" {
			t.Fatal("duplicate names have unstable ordering", paths)
		}
	}
}

func TestBrowserSnapshotBudgetScopeAndExpiration(t *testing.T) {
	var cache browserCache
	for i := 0; i < 4; i++ {
		cache.save("scope", BrowserListing{}, browserCacheBytes/2)
	}
	if len(cache.snapshots) != 2 {
		t.Fatal("unbounded recursive cache", len(cache.snapshots))
	}
	saved := cache.snapshots[1]
	if _, ok := cache.get(saved.listing.Snapshot, "different-scope"); ok {
		t.Fatal("snapshot crossed scope")
	}
	cache.snapshots[1].expires = time.Now().Add(-time.Second)
	if _, ok := cache.get(saved.listing.Snapshot, "scope"); ok {
		t.Fatal("expired snapshot reused")
	}
	cache.save("large", BrowserListing{}, browserCacheBytes)
	if len(cache.snapshots) != 1 {
		t.Fatal("cache byte budget ignored")
	}
}
