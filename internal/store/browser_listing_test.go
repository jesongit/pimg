package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

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
