package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pimg/internal/ids"
)

func (r *browserIndexer) addWatch(p string) {
	if r.watch == nil {
		return
	}
	if r.watched[p] {
		return
	}
	// fsnotify watches are not recursive. Bound OS handles independently from
	// indexed entry count; periodic reconciliation covers unwatched mounts.
	if len(r.watched) >= 8192 {
		r.mu.Lock()
		r.warning = "目录较多，部分目录通过每 10 分钟校对更新；可手动更新索引。"
		r.mu.Unlock()
		return
	}
	if err := r.watch.Add(p); err != nil {
		r.mu.Lock()
		r.warning = "部分目录无法接收系统变更通知，已保留每 10 分钟校对；可手动更新索引。"
		r.mu.Unlock()
	} else {
		r.watched[p] = true
	}
}

func (r *browserIndexer) refreshWatches() {
	if r.watch == nil {
		return
	}
	clear(r.watched)
	for _, p := range r.watch.WatchList() {
		r.watched[p] = true
	}
}

// Most notifications concern one file. Do not enumerate a 50,000-entry
// parent directory just to update that file's size, version or name.
func (r *browserIndexer) updatePath(base, p string) {
	if r.ctx.Err() != nil {
		return
	}
	if indexPathKey(base) == indexPathKey(p) {
		r.scan(base, base, true)
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	r.mu.Lock()
	r.updating[base] = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.updating[base] = false; r.mu.Unlock() }()
	err := r.updateEntry(ctx, base, p)
	if err != nil && r.ctx.Err() == nil {
		_, _ = r.db.Exec(`UPDATE roots SET error=? WHERE path=?`, "部分索引未能更新："+err.Error(), base)
	}
}

func (r *browserIndexer) updateEntry(ctx context.Context, base, p string) error {
	root, err := openImportRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return err
	}
	info, err := importInfo(root, filepath.ToSlash(rel))
	remove := errors.Is(err, os.ErrNotExist)
	if err != nil && !remove {
		// A symlink replacement is removed from the index, never followed.
		if leaf, e := root.Lstat(rel); e == nil && leaf.Mode()&os.ModeSymlink != 0 {
			remove = true
		} else {
			return err
		}
	}
	if err == nil && !info.IsDir() && !info.Mode().IsRegular() {
		remove = true
	}
	if !remove {
		var exists bool
		if e := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM entries WHERE root=? AND path=? AND directory=1)`, base, indexPathKey(filepath.Dir(p))).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			r.scan(base, base, true)
			return nil
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, newDirectory := false, false
	if remove {
		result, e := tx.ExecContext(ctx, `DELETE FROM entries WHERE root=? AND path=?`, base, indexPathKey(p))
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil {
			return e
		}
		changed = count > 0
	} else {
		newDirectory, changed, err = r.saveEntry(ctx, tx, base, root, rel, r.svc.browserEntry(p, info), "event")
		if err != nil {
			return err
		}
	}
	if changed {
		if _, err = tx.ExecContext(ctx, `UPDATE roots SET revision=revision+1,updated_at=? WHERE path=?`, time.Now().Unix(), base); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if newDirectory {
		return r.scanDirectory(ctx, base, root, rel, 0, true)
	}
	return nil
}

func (r *browserIndexer) scan(base, p string, recursive bool) {
	if r.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	r.updating[base] = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.updating[base] = false; r.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Minute)
	defer cancel()
	node, err := r.svc.ImportPathInfo(p)
	if err == nil && !node.Directory {
		err = errors.New("监听目录已被替换为文件")
	}
	var root *os.Root
	if err == nil {
		root, err = openImportRoot(base)
	}
	if err == nil {
		defer root.Close()
		rel, e := filepath.Rel(base, node.Path)
		err = e
		if err == nil {
			info, e := importInfo(root, filepath.ToSlash(rel))
			err = e
			if err == nil {
				entry := r.svc.browserEntry(node.Path, info)
				var parent any
				if indexPathKey(node.Path) != indexPathKey(base) {
					parent = indexPathKey(filepath.Dir(node.Path))
				}
				// A parent event normally inserted this directory already. The
				// root row is needed for cascade deletion of stale subtrees.
				if parent == nil {
					err = r.saveRoot(ctx, base, entry)
				}
				if err == nil {
					err = r.scanDirectory(ctx, base, root, rel, 0, recursive)
				}
			}
		}
	}
	if r.ctx.Err() != nil {
		return
	}
	message := ""
	if err != nil {
		message = "部分索引未能更新：" + err.Error()
	}
	if err == nil && p == base && recursive {
		_, err = r.db.ExecContext(ctx, `UPDATE roots SET ready=1,updated_at=?,error='' WHERE path=?`, time.Now().Unix(), base)
	} else if err == nil {
		// A successful incremental update must not hide a previous full-scan
		// failure in another subtree. A complete reconciliation clears it.
		_, err = r.db.ExecContext(ctx, `UPDATE roots SET updated_at=? WHERE path=?`, time.Now().Unix(), base)
	}
	if err != nil {
		if message == "" {
			message = "部分索引未能更新：" + err.Error()
		}
		_, _ = r.db.Exec(`UPDATE roots SET error=? WHERE path=?`, message, base)
	}
}

func (r *browserIndexer) saveRoot(ctx context.Context, base string, entry BrowserEntry) error {
	data, _ := json.Marshal(entry)
	_, err := r.db.ExecContext(ctx, `INSERT INTO entries(root,path,parent,name,lower_name,lower_path,directory,kind,ext,size,modified_ns,created_ns,version,payload,seen) VALUES(?,?,NULL,?,?,?,1,?,?,?,?,?,?,?,'root') ON CONFLICT(root,path) DO NOTHING`, base, indexPathKey(base), entry.Name, strings.ToLower(entry.Name), strings.ToLower(filepath.ToSlash(base)), entry.Kind, entry.Ext, entry.Size, entry.modifiedNS, entry.createdNS, entry.Version, string(data))
	return err
}

func (r *browserIndexer) scanDirectory(ctx context.Context, base string, root *os.Root, rel string, depth int, recursive bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return errors.New("目录超过 128 层，请单独设置更深的监听目录")
	}
	info, err := importInfo(root, filepath.ToSlash(rel))
	if err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return os.ErrNotExist
	}
	current, err := root.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer current.Close()
	opened, err := current.Lstat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return ErrSourceChanged
	}
	abs := filepath.Join(base, rel)
	r.addWatch(abs)
	dir, err := current.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	seen := ids.New()
	var partial error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, readErr := dir.ReadDir(256)
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		children := []string{}
		changed := false
		for _, item := range items {
			if item.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, e := item.Info()
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				partial = e
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			childRel := filepath.Join(rel, item.Name())
			entry := r.svc.browserEntry(filepath.Join(base, childRel), info)
			newDirectory, entryChanged, e := r.saveEntry(ctx, tx, base, root, childRel, entry, seen)
			if e != nil {
				err = e
				break
			}
			if entry.Directory && (recursive || newDirectory) {
				children = append(children, childRel)
			}
			changed = changed || entryChanged
		}
		if err == nil && changed {
			_, err = tx.ExecContext(ctx, `UPDATE roots SET revision=revision+1 WHERE path=?`, base)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return err
		}
		for _, child := range children {
			if e := r.scanDirectory(ctx, base, root, child, depth+1, recursive); e != nil {
				partial = e
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		// Yield between metadata batches; there is one scanner, never a worker
		// per media file, and no full media reads or ffprobe during indexing.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	if partial != nil {
		return partial
	} // Do not prune after incomplete reads.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE root=? AND parent=? AND seen<>?`, base, indexPathKey(abs), seen)
	if err != nil {
		return err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if removed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE roots SET revision=revision+1 WHERE path=?`, base); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("保存目录索引失败：%w", err)
	}
	return nil
}

func (r *browserIndexer) saveEntry(ctx context.Context, tx *sql.Tx, base string, root *os.Root, childRel string, entry BrowserEntry, seen string) (bool, bool, error) {
	key := indexPathKey(entry.Path)
	var oldVersion string
	var oldCreated int64
	var oldDirectory bool
	e := tx.QueryRowContext(ctx, `SELECT version,created_ns,directory FROM entries WHERE root=? AND path=?`, base, key).Scan(&oldVersion, &oldCreated, &oldDirectory)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false, false, e
	}
	if oldVersion == entry.Version {
		entry.createdNS = oldCreated
		if oldCreated != 0 {
			entry.CreatedAt = time.Unix(0, oldCreated).Unix()
		}
	} else if !entry.Directory {
		browserCreationTime(ctx, root, childRel, &entry)
	}
	if oldVersion != "" && oldDirectory != entry.Directory {
		// Replacing a folder with a file removes all old descendants.
		_, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE root=? AND path=?`, base, key)
		if err != nil {
			return false, false, err
		}
	}
	entry.RelativePath = "" // Calculated relative to the browsed folder.
	payload, _ := json.Marshal(entry)
	_, err := tx.ExecContext(ctx, `INSERT INTO entries(root,path,parent,name,lower_name,lower_path,directory,kind,ext,size,modified_ns,created_ns,version,payload,seen) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(root,path) DO UPDATE SET parent=excluded.parent,name=excluded.name,lower_name=excluded.lower_name,lower_path=excluded.lower_path,directory=excluded.directory,kind=excluded.kind,ext=excluded.ext,size=excluded.size,modified_ns=excluded.modified_ns,created_ns=excluded.created_ns,version=excluded.version,payload=excluded.payload,seen=excluded.seen`, base, key, indexPathKey(filepath.Dir(entry.Path)), entry.Name, strings.ToLower(entry.Name), strings.ToLower(filepath.ToSlash(entry.Path)), entry.Directory, entry.Kind, entry.Ext, entry.Size, entry.modifiedNS, entry.createdNS, entry.Version, string(payload), seen)
	if err != nil {
		return false, false, err
	}
	return entry.Directory && (oldVersion == "" || !oldDirectory), oldVersion != entry.Version, nil
}
