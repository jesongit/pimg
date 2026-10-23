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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// The source tree is read-only. This separate, rebuildable database contains
// metadata only; it never creates gallery files, thumbnails or import tasks.
type browserIndexManager struct {
	apply  sync.Mutex
	mu     sync.RWMutex
	paths  []string
	runner *browserIndexer
	err    string
}

type BrowserIndexRoot struct {
	Path      string `json:"path"`
	Ready     bool   `json:"ready"`
	Updating  bool   `json:"updating"`
	Entries   int    `json:"entries"`
	Revision  int64  `json:"revision"`
	UpdatedAt int64  `json:"updated_at"`
	Error     string `json:"error,omitempty"`
}

type BrowserIndexStatus struct {
	Roots        []BrowserIndexRoot `json:"roots"`
	Error        string             `json:"error,omitempty"`
	WatchWarning string             `json:"watch_warning,omitempty"`
}

type browserIndexer struct {
	svc      *Service
	db       *sql.DB
	paths    []string
	watch    *fsnotify.Watcher
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	pending  map[string]bool
	full     bool
	updating map[string]bool
	warning  string
	watched  map[string]bool
}

func indexPathKey(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func indexContains(parent, p string) bool {
	a, b := indexPathKey(parent), indexPathKey(p)
	return a == b || strings.HasPrefix(b, strings.TrimRight(a, "/")+"/")
}

func (s *Service) browserIndexPath() string {
	return filepath.Join(s.Cfg.Paths.RuntimeRoot(), "cache", "browser-index.sqlite")
}

// Validate before committing settings. Nested roots are folded into their
// parent so a file is indexed once and has one change revision.
func (s *Service) ValidateBrowserWatchPaths(paths []string) ([]string, error) {
	if len(paths) > 16 {
		return nil, errors.New("最多设置 16 个监听目录")
	}
	out := []string{}
	bytes := 0
	for _, p := range paths {
		p = strings.TrimSpace(p)
		bytes += len(p)
		if p == "" || len(p) > 4096 || bytes > 32<<10 || !filepath.IsAbs(p) {
			return nil, errors.New("监听目录必须是服务器内的绝对路径，总长度不超过 32 KiB")
		}
		node, err := s.ImportPathInfo(p)
		if err != nil || !node.Directory {
			return nil, fmt.Errorf("监听目录不可读取或不是文件夹：%s", p)
		}
		p = node.Path
		if filepath.Dir(p) == p {
			return nil, errors.New("不能监听文件系统根目录，请指定媒体文件夹")
		}
		// Watching our own database/working tree would produce a feedback loop.
		for _, own := range []string{s.Cfg.Paths.DataDir, s.Cfg.Paths.RuntimeRoot(), filepath.Dir(s.browserIndexPath())} {
			abs, e := filepath.Abs(own)
			if e == nil && (indexContains(p, abs) || indexContains(filepath.Dir(s.browserIndexPath()), p)) {
				return nil, errors.New("监听目录不能包含 PIMG 数据目录或索引缓存目录")
			}
		}
		root, err := openImportRoot(p)
		if err != nil {
			return nil, fmt.Errorf("监听目录不可读取：%s", p)
		}
		dir, err := root.Open(".")
		if err == nil {
			_, err = dir.ReadDir(1)
			dir.Close()
		}
		root.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("监听目录不可读取：%s", p)
		}
		covered := false
		for _, old := range out {
			if indexContains(old, p) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		kept := out[:0]
		for _, old := range out {
			if !indexContains(p, old) {
				kept = append(kept, old)
			}
		}
		out = append(kept, p)
	}
	return out, nil
}

func (s *Service) restoreBrowserIndex() {
	var raw string
	if err := s.DB.QueryRow(`SELECT v FROM settings WHERE k='site.browser_watch_paths'`).Scan(&raw); err != nil {
		return
	}
	var paths []string
	if json.Unmarshal([]byte(raw), &paths) != nil {
		return
	}
	// A removable/offline mount must not prevent application startup. The
	// worker reports its error and retries during periodic reconciliation.
	s.ApplyBrowserWatchPaths(paths)
}

func (s *Service) BrowserWatchPaths() []string {
	s.browserIndex.mu.RLock()
	defer s.browserIndex.mu.RUnlock()
	return append([]string{}, s.browserIndex.paths...)
}

func (s *Service) ApplyBrowserWatchPaths(paths []string) {
	m := &s.browserIndex
	m.apply.Lock()
	defer m.apply.Unlock()
	s.applyBrowserWatchPaths(paths)
}

func (s *Service) applyBrowserWatchPaths(paths []string) {
	m := &s.browserIndex
	m.mu.Lock()
	old := m.runner
	m.runner, m.err = nil, ""
	m.paths = append([]string{}, paths...)
	m.mu.Unlock()
	if old != nil {
		old.stop()
	}
	if len(paths) == 0 {
		return
	} // Default: no DB, watcher, timer or scan.
	runner, err := s.newBrowserIndexer(paths)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.err = "文件索引无法启动，请检查运行目录权限及空间：" + err.Error()
		return
	}
	m.runner = runner
	runner.wg.Add(1)
	go runner.run()
}

func (s *Service) StopBrowserIndex() {
	m := &s.browserIndex
	m.apply.Lock()
	defer m.apply.Unlock()
	m.mu.Lock()
	old := m.runner
	m.runner = nil
	m.mu.Unlock()
	if old != nil {
		old.stop()
	}
}

func (s *Service) newBrowserIndexer(paths []string) (*browserIndexer, error) {
	cache := s.browserIndexPath()
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return nil, err
	}
	// One writer and one reader; a small SQLite page cache and a bounded disk
	// budget keep a large tree from becoming an unbounded in-memory snapshot.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(2000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-4096)&_pragma=temp_store(FILE)&_pragma=mmap_size(0)", filepath.ToSlash(cache))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	_, err = db.Exec(`PRAGMA max_page_count=262144;
CREATE TABLE IF NOT EXISTS roots(path TEXT PRIMARY KEY, ready INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS entries(root TEXT NOT NULL, path TEXT NOT NULL, parent TEXT, name TEXT NOT NULL, lower_name TEXT NOT NULL, lower_path TEXT NOT NULL, directory INTEGER NOT NULL, kind TEXT NOT NULL, ext TEXT NOT NULL, size INTEGER NOT NULL, modified_ns INTEGER NOT NULL, created_ns INTEGER NOT NULL, version TEXT NOT NULL, payload TEXT NOT NULL, seen TEXT NOT NULL, PRIMARY KEY(root,path), FOREIGN KEY(root) REFERENCES roots(path) ON DELETE CASCADE, FOREIGN KEY(root,parent) REFERENCES entries(root,path) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS browser_parent_name ON entries(root,parent,directory DESC,lower_name,name,path);
CREATE INDEX IF NOT EXISTS browser_kind_path ON entries(root,kind,path);
CREATE INDEX IF NOT EXISTS browser_parent_modified ON entries(root,parent,directory DESC,modified_ns);
CREATE INDEX IF NOT EXISTS browser_parent_created ON entries(root,parent,directory DESC,created_ns);
CREATE INDEX IF NOT EXISTS browser_parent_size ON entries(root,parent,directory DESC,size);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(cache, 0o600)
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT path FROM roots`)
	if err != nil {
		db.Close()
		return nil, err
	}
	var obsolete []string
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			break
		}
		keep := false
		for _, configured := range paths {
			keep = keep || configured == p
		}
		if !keep {
			obsolete = append(obsolete, p)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	for _, p := range obsolete {
		if err == nil {
			_, err = tx.Exec(`DELETE FROM roots WHERE path=?`, p)
		}
	}
	for _, p := range paths {
		if err == nil {
			_, err = tx.Exec(`INSERT INTO roots(path) VALUES(?) ON CONFLICT DO NOTHING`, p)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &browserIndexer{svc: s, db: db, paths: append([]string{}, paths...), ctx: ctx, cancel: cancel, pending: map[string]bool{}, updating: map[string]bool{}, watched: map[string]bool{}}
	r.watch, err = fsnotify.NewWatcher()
	if err != nil {
		r.warning = "系统监听不可用，已改为每 10 分钟校对；可手动更新索引。"
	}
	return r, nil
}

func (r *browserIndexer) stop() {
	r.cancel()
	if r.watch != nil {
		_ = r.watch.Close()
	}
	r.wg.Wait()
	_ = r.db.Close()
}

func (r *browserIndexer) queue(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) >= 4096 || p == "" {
		r.full = true
		clear(r.pending)
		return
	}
	if !r.full {
		r.pending[p] = true
	}
}

func (s *Service) RefreshBrowserIndex(p string) bool {
	s.browserIndex.apply.Lock()
	defer s.browserIndex.apply.Unlock()
	s.browserIndex.mu.RLock()
	r := s.browserIndex.runner
	if r == nil {
		paths := append([]string{}, s.browserIndex.paths...)
		s.browserIndex.mu.RUnlock()
		if len(paths) == 0 {
			return false
		}
		s.applyBrowserWatchPaths(paths)
		return true
	}
	defer s.browserIndex.mu.RUnlock()
	for _, root := range r.paths {
		if p == "" || indexContains(root, p) {
			r.queue("")
			return true
		}
	}
	return false
}

func (r *browserIndexer) run() {
	defer r.wg.Done()
	if r.watch != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case <-r.ctx.Done():
					return
				case event, ok := <-r.watch.Events:
					if !ok {
						return
					}
					if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) != 0 {
						r.queue(event.Name)
					}
				case _, ok := <-r.watch.Errors:
					if !ok {
						return
					}
					r.queue("")
				}
			}
		}()
	}
	for _, root := range r.paths {
		r.scan(root, root, true)
	}
	debounce := time.NewTicker(750 * time.Millisecond)
	reconcile := time.NewTicker(10 * time.Minute)
	defer debounce.Stop()
	defer reconcile.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-reconcile.C:
			r.refreshWatches()
			for _, root := range r.paths {
				r.scan(root, root, true)
			}
		case <-debounce.C:
			r.mu.Lock()
			pending, full := r.pending, r.full
			r.pending, r.full = map[string]bool{}, false
			r.mu.Unlock()
			r.refreshWatches()
			if full {
				for _, root := range r.paths {
					r.scan(root, root, true)
				}
				continue
			}
			for p := range pending {
				for _, root := range r.paths {
					if indexContains(root, p) {
						r.updatePath(root, p)
						break
					}
				}
			}
		}
	}
}

func (s *Service) BrowserIndexStatus() BrowserIndexStatus {
	m := &s.browserIndex
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := BrowserIndexStatus{Roots: []BrowserIndexRoot{}, Error: m.err}
	r := m.runner
	for _, p := range m.paths {
		item := BrowserIndexRoot{Path: p}
		if r != nil {
			if err := r.db.QueryRow(`SELECT ready,revision,updated_at,error,(SELECT count(*) FROM entries WHERE root=? AND parent IS NOT NULL) FROM roots WHERE path=?`, p, p).Scan(&item.Ready, &item.Revision, &item.UpdatedAt, &item.Error, &item.Entries); err != nil {
				item.Error = err.Error()
			}
			r.mu.Lock()
			item.Updating = r.updating[p]
			out.WatchWarning = r.warning
			r.mu.Unlock()
		}
		out.Roots = append(out.Roots, item)
	}
	return out
}
