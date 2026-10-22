package datalayout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"pimg/internal/config"
)

// Prepare uses the configured data directory directly. Legacy sources remain
// guarded for the process lifetime and are never overwritten or removed.
func Prepare(cfg *config.Config) (func(), error) {
	return PrepareWithUpgrade(cfg, nil)
}

// PrepareWithUpgrade runs the startup preflight under the layout lock, before
// moving any files or writing a layout marker. The existing legacy DB guard is
// passed through so a backup cannot deadlock on its exclusive connection.
func PrepareWithUpgrade(cfg *config.Config, before func(string, *sql.DB) error) (func(), error) {
	root, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return nil, err
	}
	runtime := cfg.Paths.RuntimeDir
	if runtime == "" {
		runtime = filepath.Dir(root)
	}
	runtime, err = filepath.Abs(runtime)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, runtime)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("运行目录必须位于可迁移数据目录之外")
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	var locks []*flock.Flock
	var guards []*sql.DB
	release := func() {
		for _, db := range guards {
			_ = db.Close()
		}
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Unlock()
		}
	}
	failed := true
	defer func() {
		if failed {
			release()
		}
	}()
	lockPath := func(path string) error {
		l := flock.New(path)
		ok, e := l.TryLock()
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("数据或 TG 会话正在使用：%s", path)
		}
		locks = append(locks, l)
		return nil
	}
	if err = lockPath(filepath.Join(root, ".layout.lock")); err != nil {
		return nil, err
	}
	cfg.Paths.DataDir = root
	cfg.Paths.RuntimeDir = runtime
	cfg.Paths.InputDir = root
	marker := filepath.Join(root, ".layout-v2.json")
	manifest := Manifest{Version: 2, Origin: root}
	source := root
	complete := false
	if data, e := os.ReadFile(marker); e == nil {
		if json.Unmarshal(data, &manifest) != nil || manifest.Version != 2 {
			return nil, errors.New("数据目录迁移标记无效")
		}
		complete = true
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	} else {
		nested := filepath.Join(root, "pimg-data")
		if _, e = os.Stat(filepath.Join(nested, "system", "pimg.db")); e == nil {
			source = nested
		}
		// The development launcher now shares ./data with native deployment.
		// Adopt its former default only when no current data directory exists.
		if source == root && filepath.Base(root) == "data" {
			_, a := os.Stat(filepath.Join(root, "system/pimg.db"))
			_, b := os.Stat(filepath.Join(root, "pimg.db"))
			if errors.Is(a, os.ErrNotExist) && errors.Is(b, os.ErrNotExist) {
				legacy := filepath.Join(filepath.Dir(root), "data-dev")
				if _, e = os.Stat(filepath.Join(legacy, "pimg-data/system/pimg.db")); e == nil {
					source = filepath.Join(legacy, "pimg-data")
				} else if _, e = os.Stat(filepath.Join(legacy, "pimg.db")); e == nil {
					source = legacy
				}
			}
		}
		if data, e = os.ReadFile(filepath.Join(source, ".layout-v1.json")); e == nil {
			var old Manifest
			if json.Unmarshal(data, &old) != nil || old.Version != 1 {
				return nil, errors.New("旧数据目录迁移标记无效")
			}
			manifest.Origin = old.Origin
			manifest.Roots = append(manifest.Roots, old.Origin, filepath.Join(old.Origin, "pimg-data"))
		}
		manifest.Roots = append(manifest.Roots, source, root)
	}
	cfg.Paths.LegacyDir = manifest.Origin
	cfg.Paths.LegacyRoots = manifest.Roots
	canonicalDB := filepath.Join(root, "system", "pimg.db")
	if !complete && source == root {
		_, oldErr := os.Stat(filepath.Join(root, "pimg.db"))
		_, newErr := os.Stat(canonicalDB)
		_, proofErr := os.Stat(filepath.Join(root, "system/.layout-source.sha256"))
		if oldErr == nil && newErr == nil && errors.Is(proofErr, os.ErrNotExist) {
			return nil, errors.New("新旧数据库同时存在且无迁移凭据，保留两边数据，拒绝切换")
		}
	}
	sourceDB := filepath.Join(source, "system", "pimg.db")
	if _, e := os.Stat(sourceDB); errors.Is(e, os.ErrNotExist) {
		sourceDB = filepath.Join(source, "pimg.db")
	}
	// Hold both layout and SQLite/session guards so old binaries cannot fork
	// retained copies after the new installation starts.
	seen := map[string]bool{}
	var snapshotDB *sql.DB
	for _, base := range append([]string{root, source}, manifest.Roots...) {
		if base == "" {
			continue
		}
		if base != root && !seen["lock:"+base] {
			seen["lock:"+base] = true
			if _, e := os.Stat(filepath.Join(base, ".layout.lock")); e == nil {
				if err = lockPath(filepath.Join(base, ".layout.lock")); err != nil {
					return nil, err
				}
			}
		}
		for _, name := range []string{"pimg.db", "system/pimg.db"} {
			path := filepath.Join(base, filepath.FromSlash(name))
			if path == canonicalDB || seen[path] {
				continue
			}
			seen[path] = true
			if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) {
				continue
			} else if e != nil {
				return nil, e
			}
			db, e := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(1000)")
			if e != nil {
				return nil, e
			}
			guards = append(guards, db)
			db.SetMaxOpenConns(1)
			if _, e = db.Exec("PRAGMA locking_mode=EXCLUSIVE; BEGIN EXCLUSIVE; COMMIT;"); e != nil {
				return nil, fmt.Errorf("旧数据库正在使用：%w", e)
			}
			if path == sourceDB {
				snapshotDB = db
			}
		}
	}
	for _, path := range []string{cfg.Telegram.SessionFile, filepath.Join(source, "system/telegram/session.enc"), filepath.Join(source, "telegram/session.enc"), filepath.Join(root, "telegram/session.enc")} {
		if path == "" || seen[path] || filepath.Clean(Relocate(cfg, path)) == filepath.Clean(path) {
			continue
		}
		seen[path] = true
		if _, e := os.Stat(path); e == nil {
			if err = lockPath(path + ".lock"); err != nil {
				return nil, err
			}
		}
	}
	if before != nil {
		if err = before(sourceDB, snapshotDB); err != nil {
			return nil, err
		}
	}
	if !complete {
		if sourceDB != canonicalDB && snapshotDB != nil {
			if err = os.MkdirAll(filepath.Dir(canonicalDB), 0700); err != nil {
				return nil, err
			}
			if _, err = snapshotDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				return nil, err
			}
			digest, e := Hash(sourceDB)
			if e != nil {
				return nil, e
			}
			proof := filepath.Join(root, "system", ".layout-source.sha256")
			if _, e := os.Stat(canonicalDB); e == nil {
				if _, e = os.Stat(proof); errors.Is(e, os.ErrNotExist) {
					return nil, errors.New("新旧数据库同时存在且无迁移凭据，保留两边数据，拒绝切换")
				}
			}
			if previous, e := os.ReadFile(proof); e == nil && string(previous) != digest {
				return nil, errors.New("未完成迁移期间来源数据库发生变化，保留两边数据，拒绝切换")
			} else if e != nil && !errors.Is(e, os.ErrNotExist) {
				return nil, e
			}
			if err = os.WriteFile(proof, []byte(digest), 0600); err != nil {
				return nil, err
			}
			if _, e = os.Stat(canonicalDB); errors.Is(e, os.ErrNotExist) {
				if _, err = snapshotDB.Exec("VACUUM INTO '" + strings.ReplaceAll(canonicalDB, "'", "''") + "'"); err != nil {
					return nil, err
				}
			}
			db, e := sql.Open("sqlite", "file:"+filepath.ToSlash(canonicalDB))
			if e != nil {
				return nil, e
			}
			var check string
			e = db.QueryRow("PRAGMA integrity_check").Scan(&check)
			db.Close()
			if e != nil || check != "ok" {
				return nil, errors.New("迁移数据库校验失败")
			}
		}
		splitTemps := func(from string) error {
			entries, e := os.ReadDir(from)
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			for _, entry := range entries {
				path := filepath.Join(from, entry.Name())
				target := Relocate(cfg, path)
				if strings.HasSuffix(entry.Name(), ".ct") || strings.HasSuffix(entry.Name(), ".ct.done") {
					target = filepath.Join(root, "work/cipher", entry.Name())
				}
				if e = CopyTree(path, target); e != nil {
					return e
				}
			}
			return nil
		}
		// Enumerate only managed directories; never sweep a project/runtime root.
		names := []string{"system", "storage", "imported-local", "work", "cold", "hot", "telegram", "keys", "dev-master.key", "tmp", "uploads", "cache", "previews", "diagnostics"}
		if source != root {
			known := map[string]bool{}
			for _, name := range names {
				known[name] = true
			}
			entries, e := os.ReadDir(source)
			if e != nil {
				return nil, e
			}
			for _, entry := range entries {
				name := entry.Name()
				if !known[name] && !strings.HasPrefix(name, ".layout") && !strings.HasPrefix(name, "pimg.db") && !strings.HasPrefix(name, "pimg-phase1") && name != ".pimg-migration" {
					names = append(names, name)
				}
			}
		}
		for _, name := range names {
			from := filepath.Join(source, name)
			if name == "tmp" {
				if err = splitTemps(from); err != nil {
					return nil, err
				}
				continue
			}
			if name == "system" || name == "work" {
				entries, e := os.ReadDir(from)
				if errors.Is(e, os.ErrNotExist) {
					continue
				}
				if e != nil {
					return nil, e
				}
				for _, entry := range entries {
					if name == "system" && (entry.Name() == "pimg.db" || strings.HasPrefix(entry.Name(), "pimg.db-")) {
						continue
					}
					path := filepath.Join(from, entry.Name())
					if name == "work" && entry.Name() == "tmp" {
						if err = splitTemps(path); err != nil {
							return nil, err
						}
						continue
					}
					skip := cfg.Telegram.SessionFile + ".lock"
					if name == "system" && entry.Name() == "telegram" {
						skip = filepath.Join(path, "session.enc.lock")
					}
					if err = copyTree(context.Background(), path, Relocate(cfg, path), skip); err != nil {
						return nil, err
					}
				}
			} else if err = copyTree(context.Background(), from, Relocate(cfg, from), filepath.Join(source, "telegram/session.enc.lock")); err != nil {
				return nil, err
			}
		}
		for _, dir := range []string{"system/keys", "system/telegram", "storage/local/archives", "storage/local/display", "work/uploads", "work/cipher"} {
			if err = os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0700); err != nil {
				return nil, err
			}
		}
		data, e := json.MarshalIndent(manifest, "", "  ")
		if e != nil {
			return nil, e
		}
		if err = os.WriteFile(marker+".tmp", data, 0600); err != nil {
			return nil, err
		}
		if err = os.Rename(marker+".tmp", marker); err != nil {
			return nil, err
		}

		// Imported preview/cache files belong on the runtime volume. Their verified
		// copies are already in place; remove only these managed, rebuildable sources.
		if source == root {
			for _, name := range []string{"cache", "previews", "diagnostics", "tmp", "work/tmp"} {
				from := filepath.Join(root, filepath.FromSlash(name))
				if filepath.Clean(from) != filepath.Clean(Relocate(cfg, from)) {
					if err = os.RemoveAll(from); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	cfg.Paths.TempDir = Relocate(cfg, cfg.Paths.TempDir)
	cfg.Telegram.SessionFile = Relocate(cfg, cfg.Telegram.SessionFile)
	cfg.MasterKeyFile = Relocate(cfg, cfg.MasterKeyFile)
	if cfg.MasterKey == "" && filepath.Clean(cfg.MasterKeyFile) == filepath.Join(root, "system/keys/master.key") {
		if _, e := os.Stat(cfg.MasterKeyFile); errors.Is(e, os.ErrNotExist) {
			kept := filepath.Join(root, "system/keys/dev-master.key")
			if _, e = os.Stat(kept); e == nil {
				cfg.MasterKeyFile = kept
			}
		}
	}
	cfg.Storage.LocalColdDir = Relocate(cfg, cfg.Storage.LocalColdDir)
	cfg.Storage.LocalHotDir = Relocate(cfg, cfg.Storage.LocalHotDir)
	cfg.Paths.DatabaseFile = canonicalDB
	failed = false
	return release, nil
}
