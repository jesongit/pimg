package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func indexOrder(ordering string) string {
	tiePath := "path"
	if runtime.GOOS == "windows" {
		// Keys fold case for Windows lookup; ordering must retain the actual
		// relative folder spelling, just like the live filesystem comparator.
		tiePath = "json_extract(payload,'$.path')"
	}
	direction := "ASC"
	if strings.HasSuffix(ordering, "_desc") || ordering == "type_desc" {
		direction = "DESC"
	}
	name := "lower_name " + direction + ",name " + direction + "," + tiePath + " " + direction
	if ordering == "" || ordering == "name" || ordering == "name_desc" {
		return "directory DESC," + name
	}
	column := "size"
	switch ordering {
	case "type", "type_desc":
		column = "ext"
	case "created_asc", "created_desc":
		column = "created_ns"
	case "modified_asc", "modified_desc":
		column = "modified_ns"
	}
	// Folders always sort by name; date/size/type applies to files only.
	unknown := ""
	if column == "created_ns" {
		unknown = "CASE WHEN directory=0 AND created_ns=0 THEN 1 ELSE 0 END ASC,"
	}
	return "directory DESC," + unknown + "CASE WHEN directory=0 THEN " + column + " END " + direction + ",lower_name ASC,name ASC," + tiePath + " ASC"
}

func (s *Service) browseIndexedFiles(ctx context.Context, p, query string, offset int, ordering string, opts BrowserOptions) (BrowserListing, bool, error) {
	m := &s.browserIndex
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.runner
	if r == nil {
		return BrowserListing{}, false, nil
	}
	base := ""
	for _, root := range r.paths {
		if indexContains(root, p) {
			base = root
			break
		}
	}
	if base == "" {
		return BrowserListing{}, false, nil
	}
	out := BrowserListing{Path: p, Parent: filepath.Dir(p), Items: []BrowserEntry{}, Recursive: opts.Recursive, Indexed: true, IndexRoot: base}
	for current := p; ; current = filepath.Dir(current) {
		name := filepath.Base(current)
		if current == filepath.Dir(current) {
			name = current
		}
		out.Breadcrumbs = append([]BrowserLocation{{Path: current, Name: name}}, out.Breadcrumbs...)
		if current == filepath.Dir(current) {
			break
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return out, true, err
	}
	defer tx.Rollback()
	var revision int64
	var ready bool
	if err = tx.QueryRowContext(ctx, `SELECT revision,ready,error FROM roots WHERE path=?`, base).Scan(&revision, &ready, &out.IndexError); err != nil {
		return out, true, err
	}
	out.IndexBuilding = !ready
	r.mu.Lock()
	out.IndexUpdating = r.updating[base]
	r.mu.Unlock()
	query = strings.ToLower(strings.TrimSpace(query))
	if opts.Recursive && runtime.GOOS == "windows" {
		query = strings.ReplaceAll(query, `\`, "/")
	}
	viewKey := sha256Hex([]byte(base + "\x00" + p + "\x00" + query + "\x00" + ordering + fmt.Sprint(opts.Recursive)))[:16]
	out.Snapshot = fmt.Sprintf("idx:%s:%d", viewKey, revision)
	if offset > 0 && opts.Snapshot != out.Snapshot {
		return out, true, errors.New("索引已更新，请刷新目录后继续浏览")
	}
	key := indexPathKey(p)
	where, args := "root=? AND parent=?", []any{base, key}
	if opts.Recursive {
		prefix := strings.TrimRight(key, "/") + "/"
		where, args = "root=? AND path>=? AND path<? AND directory=0 AND kind IN ('image','video')", []any{base, prefix, strings.TrimSuffix(prefix, "/") + "0"}
	}
	if query != "" {
		if opts.Recursive {
			// Match paths relative to the current folder, including Unicode;
			// SQLite lower() alone handles only ASCII.
			prefixLength := len([]rune(filepath.ToSlash(p))) + 2
			where += " AND instr(substr(lower_path,?),?)>0"
			args = append(args, prefixLength, query)
		} else {
			where += " AND instr(lower_name,?)>0"
			args = append(args, query)
		}
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM entries WHERE "+where, args...).Scan(&out.Total); err != nil {
		return out, true, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT payload,path FROM entries WHERE "+where+" ORDER BY "+indexOrder(ordering)+" LIMIT 200 OFFSET ?", append(args, offset)...)
	if err != nil {
		return out, true, err
	}
	allowed := s.AllowedExts()
	for rows.Next() {
		var payload, storedPath string
		var entry BrowserEntry
		if err = rows.Scan(&payload, &storedPath); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(payload), &entry); err != nil {
			break
		}
		if runtime.GOOS != "windows" {
			// Old JSON payloads may already have replaced invalid UTF-8. The
			// native SQLite key retains those bytes even before reconciliation.
			entry.Path = storedPath
		}
		rel, e := filepath.Rel(p, entry.Path)
		if e != nil {
			err = e
			break
		}
		entry.RelativePath = filepath.ToSlash(rel)
		entry.Importable = false
		for _, ext := range allowed {
			if strings.EqualFold(ext, entry.Ext) && !entry.Directory && entry.Kind != "other" {
				entry.Importable = true
				break
			}
		}
		out.Items = append(out.Items, entry)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, true, err
	}
	if offset+len(out.Items) < out.Total {
		out.NextOffset = offset + len(out.Items)
	}
	return out, true, nil
}
