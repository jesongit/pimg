package dbx

import (
	"encoding/json"
	"hash/fnv"
	"strings"
	"sync"
)

// Only fields affecting gallery membership or order invalidate its ID index.
// Progress, thumbnail URLs and task polling do not rebuild large libraries.
const fileOrderSchema = `CREATE INDEX files_owner_visible_id ON files(owner_id,hidden,id);
CREATE TABLE gallery_revisions(owner_id TEXT PRIMARY KEY,revision INTEGER NOT NULL DEFAULT 1);
INSERT INTO gallery_revisions(owner_id) SELECT DISTINCT owner_id FROM files;
CREATE TRIGGER gallery_file_insert AFTER INSERT ON files BEGIN
 INSERT INTO gallery_revisions(owner_id) VALUES(NEW.owner_id) ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
END;
CREATE TRIGGER gallery_file_delete AFTER DELETE ON files BEGIN
 INSERT INTO gallery_revisions(owner_id) VALUES(OLD.owner_id) ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
END;
CREATE TRIGGER gallery_file_update AFTER UPDATE OF owner_id,name,orig_name,kind,size,album_id,hidden,published,purge_state,archive_state,publish_error,naming_json,created_at ON files
 WHEN OLD.owner_id IS NOT NEW.owner_id OR OLD.name IS NOT NEW.name OR OLD.orig_name IS NOT NEW.orig_name OR OLD.kind IS NOT NEW.kind OR OLD.size IS NOT NEW.size OR OLD.album_id IS NOT NEW.album_id OR OLD.hidden IS NOT NEW.hidden OR OLD.published IS NOT NEW.published OR OLD.purge_state IS NOT NEW.purge_state OR OLD.archive_state IS NOT NEW.archive_state OR OLD.publish_error IS NOT NEW.publish_error OR OLD.naming_json IS NOT NEW.naming_json OR OLD.created_at IS NOT NEW.created_at
BEGIN
 INSERT INTO gallery_revisions(owner_id) VALUES(OLD.owner_id) ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
 INSERT INTO gallery_revisions(owner_id) SELECT NEW.owner_id WHERE NEW.owner_id!=OLD.owner_id ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
END;`

const fileOrderCacheEntries = 8
const fileOrderCacheBytes = 32 << 20

type fileOrderEntry struct {
	ids      []string
	revision int64
	bytes    int
	used     uint64
}

type fileOrderCache struct {
	sync.Mutex
	entries map[string]*fileOrderEntry
	bytes   int
	clock   uint64
}

func fileOrderSQL(sort string) string {
	switch sort {
	case "oldest":
		return "(" + mediaTimeSQL + ") ASC, id ASC"
	case "name":
		return "name COLLATE NOCASE ASC, id ASC"
	case "size":
		return "size DESC, id DESC"
	case "__ids", "random":
		return "id ASC"
	default:
		return "(" + mediaTimeSQL + ") DESC, id DESC"
	}
}

func (db *DB) galleryRevision(owner string) (int64, error) {
	var revision int64
	query := `SELECT COALESCE(SUM(revision),0) FROM gallery_revisions`
	args := []any{}
	if owner != "" {
		query += ` WHERE owner_id=?`
		args = append(args, owner)
	}
	err := db.QueryRow(query, args...).Scan(&revision)
	return revision, err
}

// orderedFileIDs shares complete, stable result orders across paging and
// viewer navigation. The LRU has both entry and byte limits; oversized scopes
// remain usable but are transient. Holding this small cache lock also coalesces
// concurrent misses, without exposing one owner's IDs to another filter.
func (db *DB) orderedFileIDs(f FileFilter) ([]string, error) {
	cache := &db.fileOrders
	cache.Lock()
	defer cache.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[string]*fileOrderEntry)
	}
	revision, err := db.galleryRevision(f.OwnerID)
	if err != nil {
		return nil, err
	}
	f.Page, f.PerPage = 0, 0
	if f.Sort == "" {
		f.Sort = "newest"
	}
	keyJSON, _ := json.Marshal(f)
	key := string(keyJSON)
	cache.clock++
	if entry := cache.entries[key]; entry != nil && entry.revision == revision {
		entry.used = cache.clock
		return entry.ids, nil
	}
	// New shuffles reuse the canonical ID set, avoiding another database scan.
	base := f
	base.Sort, base.Seed = "__ids", ""
	baseJSON, _ := json.Marshal(base)
	baseKey := string(baseJSON)
	var ids []string
	if f.Sort == "random" {
		if entry := cache.entries[baseKey]; entry != nil && entry.revision == revision {
			entry.used = cache.clock
			ids = append([]string(nil), entry.ids...)
		}
	}
	if ids == nil {
		where, args := fileFilterSQL(f)
		rows, e := db.Query(`SELECT id FROM files WHERE `+where+` ORDER BY `+fileOrderSQL(f.Sort), args...)
		if e != nil {
			return nil, e
		}
		ids = make([]string, 0)
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if f.Sort == "random" {
			cache.put(baseKey, append([]string(nil), ids...), revision)
		}
	}
	if f.Sort == "random" {
		shuffleFileIDs(ids, f.Seed)
	}
	cache.put(key, ids, revision)
	return ids, nil
}

func (cache *fileOrderCache) put(key string, ids []string, revision int64) {
	if old := cache.entries[key]; old != nil {
		cache.bytes -= old.bytes
		delete(cache.entries, key)
	}
	bytes := len(key) + len(ids)*16
	for _, id := range ids {
		bytes += len(id)
	}
	if bytes > fileOrderCacheBytes {
		return
	}
	for len(cache.entries) >= fileOrderCacheEntries || cache.bytes+bytes > fileOrderCacheBytes {
		var oldest string
		var last uint64 = ^uint64(0)
		for k, entry := range cache.entries {
			if entry.used < last {
				oldest, last = k, entry.used
			}
		}
		cache.bytes -= cache.entries[oldest].bytes
		delete(cache.entries, oldest)
	}
	cache.entries[key] = &fileOrderEntry{ids: ids, revision: revision, bytes: bytes, used: cache.clock}
	cache.bytes += bytes
}

// SplitMix64 and rejection sampling make a seed reproducible independently of
// the standard library's PRNG, and give a uniform Fisher-Yates permutation.
func shuffleFileIDs(ids []string, seed string) {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(seed))
	state := hash.Sum64()
	next := func() uint64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	for i := len(ids) - 1; i > 0; i-- {
		bound := uint64(i + 1)
		threshold := -bound % bound
		value := next()
		for value < threshold {
			value = next()
		}
		j := int(value % bound)
		ids[i], ids[j] = ids[j], ids[i]
	}
}

type FileNavigation struct {
	Previous string `json:"previous"`
	Next     string `json:"next"`
	Position int    `json:"position"`
	Total    int    `json:"total"`
}

func (db *DB) FileNavigation(f FileFilter, id string) (*FileNavigation, error) {
	ids, err := db.orderedFileIDs(f)
	if err != nil {
		return nil, err
	}
	for i, candidate := range ids {
		if candidate != id {
			continue
		}
		out := &FileNavigation{Position: i + 1, Total: len(ids)}
		if i > 0 {
			out.Previous = ids[i-1]
		}
		if i+1 < len(ids) {
			out.Next = ids[i+1]
		}
		return out, nil
	}
	return nil, ErrNotFound
}

func (db *DB) listRandomFiles(f FileFilter) ([]*File, int, error) {
	ids, err := db.orderedFileIDs(f)
	if err != nil {
		return nil, 0, err
	}
	if f.PerPage <= 0 {
		f.PerPage = 40
	}
	if f.PerPage > 200 {
		f.PerPage = 200
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	start := (f.Page - 1) * f.PerPage
	if start < 0 || start >= len(ids) {
		return []*File{}, len(ids), nil
	}
	end := start + f.PerPage
	if end > len(ids) {
		end = len(ids)
	}
	page := ids[start:end]
	args := make([]any, len(page))
	for i, id := range page {
		args[i] = id
	}
	where, filterArgs := fileFilterSQL(f)
	args = append(args, filterArgs...)
	// CROSS JOIN keeps the tiny requested page on the outside of the plan.
	// Otherwise SQLite may prefer an owner index and scan that owner's entire
	// library before applying the IN list on each cached page request.
	rows, err := db.Query(`WITH wanted(wanted_id) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(page)), ",")+`) SELECT `+fileCols+` FROM wanted CROSS JOIN files WHERE files.id=wanted.wanted_id AND `+where, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	byID := make(map[string]*File, len(page))
	for rows.Next() {
		file, e := scanFile(rows)
		if e != nil {
			return nil, 0, e
		}
		byID[file.ID] = file
	}
	out := make([]*File, 0, len(page))
	for _, id := range page {
		if file := byID[id]; file != nil {
			out = append(out, file)
		}
	}
	return out, len(ids), rows.Err()
}
