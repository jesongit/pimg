package dbx

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedGalleryOrder(t *testing.T, db *DB, owner, prefix string, count int) {
	t.Helper()
	_, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,published,created_at,updated_at,owner_id)
SELECT ?||printf('%06d',x),'same-'||(x%7),'source-'||x||'.jpg','jpg','image/jpeg',x%11,'hash',CASE WHEN x%3=0 THEN 'video' ELSE 'image' END,X'01',X'02','done',x%2,1700000000+(x%13),1700000000,? FROM n`, count, prefix, owner)
	if err != nil {
		t.Fatal(err)
	}
}

func orderIDs(files []*File) []string {
	ids := make([]string, len(files))
	for i, file := range files {
		ids[i] = file.ID
	}
	return ids
}

func TestFileOrderFullScopePagingAndNavigation(t *testing.T) {
	db := activityDB(t)
	seedGalleryOrder(t, db, AdminOwner, "self-", 137)
	seedGalleryOrder(t, db, "user:other", "other-", 21)
	for _, sort := range []string{"newest", "oldest", "name", "size", "random"} {
		filter := FileFilter{OwnerID: AdminOwner, Sort: sort, Seed: "stable-seed", PerPage: 60}
		var ids []string
		for page := 1; page <= 3; page++ {
			filter.Page = page
			files, total, err := db.ListFiles(filter)
			if err != nil || total != 137 {
				t.Fatal(sort, total, err)
			}
			ids = append(ids, orderIDs(files)...)
		}
		seen := make(map[string]bool)
		for i, id := range ids {
			if seen[id] || !strings.HasPrefix(id, "self-") {
				t.Fatal("duplicate or foreign file", sort, id)
			}
			seen[id] = true
			nav, err := db.FileNavigation(filter, id)
			if err != nil || nav.Position != i+1 || nav.Total != 137 {
				t.Fatal(sort, i, nav, err)
			}
			previous, next := "", ""
			if i > 0 {
				previous = ids[i-1]
			}
			if i+1 < len(ids) {
				next = ids[i+1]
			}
			if nav.Previous != previous || nav.Next != next {
				t.Fatal("page boundary differs from gallery", sort, i, nav)
			}
		}
		if len(seen) != 137 {
			t.Fatal(sort, len(seen))
		}
		filter.Page = 1
		again, _, _ := db.ListFiles(filter)
		if !reflect.DeepEqual(orderIDs(again), ids[:60]) {
			t.Fatal("same seed/order changed", sort)
		}
		if sort == "random" {
			filter.Seed = "new-seed"
			shuffled, _, _ := db.ListFiles(filter)
			if reflect.DeepEqual(orderIDs(shuffled), ids[:60]) {
				t.Fatal("shuffle did not change the order")
			}
			filter.Seed = "stable-seed"
			db.fileOrders = fileOrderCache{} // Reproduce after eviction/restart.
			rebuilt, _, _ := db.ListFiles(filter)
			if !reflect.DeepEqual(orderIDs(rebuilt), ids[:60]) {
				t.Fatal("seed not reproducible from a cold index")
			}
		}
	}
	filter := FileFilter{OwnerID: AdminOwner, Sort: "random", Seed: "scoped", Query: "source-1", Kind: "image", State: "published", PerPage: 200}
	files, total, err := db.ListFiles(filter)
	if err != nil || total == 0 || len(files) != total {
		t.Fatal(total, err)
	}
	for i, file := range files {
		if file.OwnerID != AdminOwner || file.Kind != "image" || !file.Published || !strings.Contains(file.OrigName, "source-1") {
			t.Fatal("filter ignored", file)
		}
		nav, err := db.FileNavigation(filter, file.ID)
		if err != nil || nav.Position != i+1 || nav.Total != total {
			t.Fatal(nav, err)
		}
	}
	if _, err := db.FileNavigation(filter, "other-000001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign navigation leaked", err)
	}
}

func TestFileOrderInvalidationAndConcurrentRequests(t *testing.T) {
	db := activityDB(t)
	seedGalleryOrder(t, db, AdminOwner, "item-", 77)
	filter := FileFilter{OwnerID: AdminOwner, Sort: "name", PerPage: 200}
	initial, _, err := db.ListFiles(filter)
	if err != nil {
		t.Fatal(err)
	}
	nav, err := db.FileNavigation(filter, initial[0].ID)
	if err != nil || nav.Position != 1 {
		t.Fatal(nav, err)
	}
	revision, _ := db.galleryRevision(AdminOwner)
	db.Exec(`UPDATE files SET archive_attempts=archive_attempts+1,updated_at=updated_at+1 WHERE id='item-000001'`)
	afterProgress, _ := db.galleryRevision(AdminOwner)
	if afterProgress != revision {
		t.Fatal("progress invalidated gallery order")
	}
	db.Exec(`UPDATE files SET name='AAAA' WHERE id='item-000001'`)
	first, err := db.FileNavigation(filter, "item-000001")
	if err != nil || first.Position != 1 {
		t.Fatal("rename did not invalidate order", first, err)
	}
	db.Exec(`UPDATE files SET hidden=1 WHERE id='item-000001'`)
	if _, err := db.FileNavigation(filter, "item-000001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("hidden file remained navigable", err)
	}
	db.Exec(`DELETE FROM files WHERE id='item-000002'`)
	filter.Sort, filter.Seed = "random", "concurrent"
	var group sync.WaitGroup
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			files, total, e := db.ListFiles(filter)
			if e != nil || total != 75 || len(files) != total {
				errors <- fmt.Errorf("concurrent list: %d/%d: %v", len(files), total, e)
			}
		}()
	}
	group.Wait()
	close(errors)
	for e := range errors {
		t.Fatal(e)
	}
	otherRevision, _ := db.galleryRevision(AdminOwner)
	seedGalleryOrder(t, db, "user:other", "foreign-", 3)
	afterOther, _ := db.galleryRevision(AdminOwner)
	if otherRevision != afterOther {
		t.Fatal("other owner invalidated this owner's order")
	}
}

func TestFileOrderCacheBoundedByEntriesAndBytes(t *testing.T) {
	cache := fileOrderCache{entries: make(map[string]*fileOrderEntry)}
	largeID := strings.Repeat("x", 32768)
	ids := make([]string, 350)
	for i := range ids {
		ids[i] = largeID
	}
	for i := 0; i < 15; i++ {
		cache.clock++
		cache.put(fmt.Sprint(i), ids, 1)
		if cache.bytes > fileOrderCacheBytes || len(cache.entries) > fileOrderCacheEntries {
			t.Fatal("cache exceeded budget", cache.bytes, len(cache.entries))
		}
	}
	if len(cache.entries) >= fileOrderCacheEntries {
		t.Fatal("byte budget was not independently applied")
	}
	oversized := make([]string, 1100)
	for i := range oversized {
		oversized[i] = largeID
	}
	cache.put("too-large", oversized, 1)
	if cache.entries["too-large"] != nil {
		t.Fatal("oversized scope retained")
	}
}

func TestFileOrderUpgradePreservesExistingRecords(t *testing.T) {
	path, old := oldUpgradeDB(t, len(migrations)-1)
	_, err := old.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at) VALUES('legacy','kept','original.jpg','jpg','image/jpeg',4,'hash','image',X'01',X'02','done',12345,12346)`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	file, err := db.GetFile("legacy")
	if err != nil || file.Name != "kept" || file.CreatedAt != 12345 || file.UpdatedAt != 12346 {
		t.Fatal("upgrade changed original record", file, err)
	}
	nav, err := db.FileNavigation(FileFilter{OwnerID: AdminOwner, Sort: "random", Seed: "legacy"}, "legacy")
	if err != nil || nav.Total != 1 || nav.Position != 1 {
		t.Fatal("legacy record not indexed", nav, err)
	}
}

func TestFileOrderLargeLibraryPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("100k gallery performance fixture")
	}
	db := activityDB(t)
	start := time.Now()
	seedGalleryOrder(t, db, AdminOwner, "large-", 100000)
	t.Logf("100k database preparation: %s", time.Since(start))
	filter := FileFilter{OwnerID: AdminOwner, Sort: "random", Seed: "large-first", PerPage: 60}
	start = time.Now()
	first, total, err := db.ListFiles(filter)
	cold := time.Since(start)
	if err != nil || total != 100000 || len(first) != 60 {
		t.Fatal(total, len(first), err)
	}
	filter.Page = 1000
	start = time.Now()
	page, _, err := db.ListFiles(filter)
	warm := time.Since(start)
	if err != nil || len(page) != 60 {
		t.Fatal(len(page), err)
	}
	start = time.Now()
	nav, err := db.FileNavigation(filter, page[30].ID)
	neighbor := time.Since(start)
	if err != nil || nav.Position != 999*60+31 || nav.Total != 100000 {
		t.Fatal(nav, err)
	}
	filter.Seed = "large-second"
	filter.Page = 1
	start = time.Now()
	second, _, err := db.ListFiles(filter)
	reshuffle := time.Since(start)
	if err != nil || len(second) != 60 || reflect.DeepEqual(orderIDs(first), orderIDs(second)) {
		t.Fatal("large reshuffle failed", err)
	}
	t.Logf("100k cold random page=%s, cached page1000=%s, neighbour at59971=%s, new shuffle=%s, cache=%d bytes/%d entries", cold, warm, neighbor, reshuffle, db.fileOrders.bytes, len(db.fileOrders.entries))
	if cold > 10*time.Second || warm > time.Second || neighbor > time.Second || reshuffle > time.Second {
		t.Fatal("gallery order exceeded conservative latency budget")
	}
}
