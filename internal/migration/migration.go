// Package migration implements explicit, administrator-requested whole-instance
// transfer. It snapshots SQLite but does not freeze uploads or run a handoff protocol.
package migration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

var ErrRestart = errors.New("migration restart requested")
var AllScopes = []string{"telegram", "local", "work", "previews"}

const maxManifest = 64 << 20
const runtimeFile = "system/migration-runtime.json"

type Manifest struct {
	Format      int               `json:"format"`
	Version     string            `json:"version"`
	CreatedAt   int64             `json:"created_at"`
	Scopes      []string          `json:"scopes"`
	SourceRoot  string            `json:"source_root"`
	MasterPath  string            `json:"master_path"`
	SessionPath string            `json:"session_path"`
	Config      config.Config     `json:"startup"`
	External    map[string]string `json:"external"`
	Files       int               `json:"files"`
	Accounts    int               `json:"accounts"`
	Entries     map[string]string `json:"entries"`
}
type Preview struct {
	ID        string   `json:"id,omitempty"`
	Version   string   `json:"version"`
	CreatedAt int64    `json:"created_at"`
	Scopes    []string `json:"scopes"`
	Files     int      `json:"files"`
	Accounts  int      `json:"accounts"`
	Bytes     int64    `json:"bytes"`
}

func (m *Manifest) Preview() Preview {
	return Preview{Version: m.Version, CreatedAt: m.CreatedAt, Scopes: m.Scopes, Files: m.Files, Accounts: m.Accounts}
}

func Root(cfg *config.Config) string { p, _ := filepath.Abs(cfg.Paths.DataDir); return p }
func WorkDir(cfg *config.Config) string {
	p, _ := filepath.Abs(cfg.Paths.RuntimeRoot())
	return filepath.Join(p, "migration")
}
func NewID() string          { var b [24]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func ValidID(id string) bool { b, e := hex.DecodeString(id); return e == nil && len(b) == 24 }
func validPath(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\:\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}
func within(base, p string) bool {
	r, e := filepath.Rel(base, p)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func ScopeMap(scopes []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, v := range scopes {
		found := false
		for _, a := range AllScopes {
			if v == a {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("未知导出范围 %s", v)
		}
		out[v] = true
	}
	return out, nil
}
func relativeOr(root, p, fallback string) string {
	if within(root, p) && p != "" {
		r, _ := filepath.Rel(root, p)
		return filepath.ToSlash(r)
	}
	return fallback
}
func atomicJSON(p string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	tmp := p + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, p)
}

func Export(ctx context.Context, s *store.Service, version string, scopes []string, out io.Writer) (*Manifest, error) {
	selected, err := ScopeMap(scopes)
	if err != nil {
		return nil, err
	}
	root := Root(s.Cfg)
	work, err := os.MkdirTemp(WorkDir(s.Cfg), "snapshot-")
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(WorkDir(s.Cfg), 0700); err == nil {
			work, err = os.MkdirTemp(WorkDir(s.Cfg), "snapshot-")
		}
	}
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	snapshot := filepath.Join(work, "pimg.db")
	if _, err = s.DB.ExecContext(ctx, `VACUUM INTO ?`, snapshot); err != nil {
		return nil, err
	}
	m := &Manifest{Format: 1, Version: version, CreatedAt: time.Now().Unix(), Scopes: scopes, SourceRoot: root, Config: *s.Cfg, External: map[string]string{}, Entries: map[string]string{}}
	m.Config.MasterKey = ""
	m.Config.Admin.Password = ""
	m.Config.Server.PublicBaseURL = s.PublicBaseURL()
	m.MasterPath = relativeOr(root, s.Cfg.MasterKeyFile, "system/keys/master.key")
	m.SessionPath = relativeOr(root, s.Cfg.Telegram.SessionFile, "system/telegram/session.enc")
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(snapshot)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	err = d.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&m.Files)
	if err == nil {
		err = d.QueryRow(`SELECT (SELECT COUNT(*) FROM accounts)+(SELECT COUNT(*) FROM admin)`).Scan(&m.Accounts)
	}
	d.Close()
	if err != nil {
		return nil, err
	}
	paths := map[string]string{"system/pimg.db": snapshot}
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r0, _ := filepath.Rel(root, p)
		first := strings.Split(filepath.ToSlash(r0), "/")[0]
		legacy := first == "pimg-data" || first == ".pimg-migration" || first == "migration" || first == "cold" || first == "hot" || first == "telegram" || first == "keys" || first == "dev-master.key" || strings.HasPrefix(first, "pimg.db") || strings.HasPrefix(first, "pimg-phase1") || first == "diagnostics"
		if legacy {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		r, _ := filepath.Rel(root, p)
		r = filepath.ToSlash(r)
		if !validPath(r) || e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("迁移不支持符号链接或非法路径：%s", r)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		if strings.HasPrefix(r, ".layout") || strings.HasPrefix(r, "system/.layout") || strings.HasPrefix(r, "system/.source-db") || strings.HasSuffix(r, ".lock") || strings.HasSuffix(r, "-wal") || strings.HasSuffix(r, "-shm") || r == "system/pimg.db" || r == runtimeFile || r == m.MasterPath {
			return nil
		}
		if !selected["telegram"] && (strings.HasPrefix(r, "system/telegram/") || r == m.SessionPath) {
			return nil
		}
		if !selected["local"] && (strings.HasPrefix(r, "storage/") || strings.HasPrefix(r, "imported-local/")) && filepath.Base(p) != ".signing-key" {
			return nil
		}
		if !selected["work"] && (strings.HasPrefix(r, "work/") || strings.HasPrefix(r, "uploads/") || strings.HasPrefix(r, "tmp/")) {
			return nil
		}
		if !selected["previews"] && (strings.HasPrefix(r, "previews/") || strings.HasPrefix(r, "cache/") || strings.HasPrefix(r, "original-cache/")) {
			return nil
		}
		paths[r] = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	if selected["previews"] && filepath.Clean(s.Cfg.Paths.RuntimeRoot()) != filepath.Clean(root) {
		for _, name := range []string{"previews", "cache"} {
			base := filepath.Join(s.Cfg.Paths.RuntimeRoot(), name)
			err = filepath.WalkDir(base, func(p string, e fs.DirEntry, walkErr error) error {
				if errors.Is(walkErr, os.ErrNotExist) && p == base {
					return nil
				}
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if e.Type()&os.ModeSymlink != 0 {
					return errors.New("运行缓存不支持符号链接")
				}
				if e.IsDir() {
					return nil
				}
				if !e.Type().IsRegular() {
					return nil
				}
				rel, _ := filepath.Rel(base, p)
				paths[name+"/"+filepath.ToSlash(rel)] = p
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}

	if selected["telegram"] && s.Cfg.Telegram.SessionFile != "" && !within(root, s.Cfg.Telegram.SessionFile) {
		if _, e := os.Stat(s.Cfg.Telegram.SessionFile); e == nil {
			paths[m.SessionPath] = s.Cfg.Telegram.SessionFile
		}
	}
	{
		index := 0
		for _, dirs := range s.MigrationLocalDirectories() {
			for _, dir := range dirs {
				if dir == "" || within(root, dir) {
					continue
				}
				if _, ok := m.External[dir]; ok {
					continue
				}
				index++
				prefix := fmt.Sprintf("imported-local/%d", index)
				m.External[dir] = prefix
				if !selected["local"] {
					continue
				}
				err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if e.IsDir() {
						return nil
					}
					if e.Type()&os.ModeSymlink != 0 {
						return errors.New("外部本地存储包含符号链接")
					}
					if !e.Type().IsRegular() || strings.HasSuffix(p, ".lock") {
						return nil
					}
					r, _ := filepath.Rel(dir, p)
					paths[prefix+"/"+filepath.ToSlash(r)] = p
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
		}
	}
	keyPath := filepath.Join(work, "master.key")
	if err = os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(s.Master.Raw())), 0600); err != nil {
		return nil, err
	}
	paths[m.MasterPath] = keyPath
	// Hash the exact staged bytes that are written to the stream. Files received
	// after this export starts are intentionally outside this transfer's scope.
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	write := func(name string, body io.Reader, size int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size, ModTime: time.Unix(m.CreatedAt, 0), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, body, size)
		return err
	}
	for rel, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, e := os.Open(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, e
		}
		h := sha256.New()
		e = write("data/"+rel, io.TeeReader(f, h), info.Size())
		f.Close()
		if e != nil {
			return nil, e
		}
		m.Entries["data/"+rel] = hex.EncodeToString(h.Sum(nil))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err = write("manifest.json", strings.NewReader(string(b)), int64(len(b))); err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

// Stage validates into a private sibling directory. No running database, key or
// TG session is replaced by an uploaded package.
func Stage(ctx context.Context, cfg *config.Config, src io.Reader) (Preview, error) {
	id := NewID()
	dir := filepath.Join(WorkDir(cfg), "import-"+id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Preview{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	gz, err := gzip.NewReader(src)
	if err != nil {
		return Preview{}, errors.New("不是有效的 Pimg 迁移包")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := map[string]string{}
	var m Manifest
	var total int64
	for count := 0; ; count++ {
		if count > 1000000 {
			return Preview{}, errors.New("迁移包文件过多")
		}
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Preview{}, err
		}
		if !validPath(h.Name) || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 1<<40 {
			return Preview{}, errors.New("迁移包包含非法路径或文件类型")
		}
		if h.Name == "manifest.json" {
			if m.Format != 0 || h.Size > maxManifest {
				return Preview{}, errors.New("迁移清单无效")
			}
			b, e := io.ReadAll(io.LimitReader(tr, maxManifest+1))
			if e != nil {
				return Preview{}, e
			}
			if json.Unmarshal(b, &m) != nil {
				return Preview{}, errors.New("迁移清单无效")
			}
			continue
		}
		if !strings.HasPrefix(h.Name, "data/") || entries[h.Name] != "" {
			return Preview{}, errors.New("迁移包文件重复或越界")
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return Preview{}, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return Preview{}, err
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(tr, h.Size))
		closeErr := f.Close()
		if e != nil || closeErr != nil || n != h.Size {
			return Preview{}, errors.New("迁移包文件不完整")
		}
		total += n
		if total > 1<<40 {
			return Preview{}, errors.New("迁移包过大")
		}
		entries[h.Name] = hex.EncodeToString(hash.Sum(nil))
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return Preview{}, errors.New("迁移包压缩数据不完整")
	}
	if m.Format != 1 || !validPath(m.MasterPath) || !validPath(m.SessionPath) || len(entries) != len(m.Entries) || len(entries) == 0 {
		return Preview{}, errors.New("迁移包格式或清单不完整")
	}
	if _, err := ScopeMap(m.Scopes); err != nil {
		return Preview{}, err
	}
	for p, hash := range entries {
		if m.Entries[p] != hash {
			return Preview{}, errors.New("迁移包校验失败：" + p)
		}
	}
	if entries["data/system/pimg.db"] == "" || entries["data/"+m.MasterPath] == "" {
		return Preview{}, errors.New("缺少数据库或主密钥")
	}
	for _, rel := range m.External {
		if !validPath(rel) || !strings.HasPrefix(rel, "imported-local/") {
			return Preview{}, errors.New("外部目录映射无效")
		}
	}
	if m.MasterPath == "system/pimg.db" || m.MasterPath == runtimeFile || m.SessionPath == m.MasterPath || m.SessionPath == "system/pimg.db" || m.SessionPath == runtimeFile {
		return Preview{}, errors.New("迁移关键路径冲突")
	}
	key, e := os.ReadFile(filepath.Join(dir, "data", filepath.FromSlash(m.MasterPath)))
	if e != nil {
		return Preview{}, e
	}
	raw, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(key)))
	if e != nil || len(raw) != 32 {
		return Preview{}, errors.New("迁移主密钥无效")
	}
	if err := validateDB(filepath.Join(dir, "data/system/pimg.db")); err != nil {
		return Preview{}, err
	}
	master, e := cryptox.NewMaster(raw)
	if e != nil {
		return Preview{}, e
	}
	stagedDB, e := dbx.Open(filepath.Join(dir, "data/system/pimg.db"))
	if e != nil {
		return Preview{}, e
	}
	e = store.ValidateMigrationKeys(stagedDB, master)
	closeErr := stagedDB.Close()
	if e != nil {
		return Preview{}, e
	}
	if closeErr != nil {
		return Preview{}, closeErr
	}
	if err := atomicJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		return Preview{}, err
	}
	ok = true
	v := m.Preview()
	v.ID = id
	v.Bytes = total
	return v, nil
}

func validateDB(p string) error {
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro")
	if err != nil {
		return err
	}
	defer d.Close()
	var result string
	if err = d.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		return errors.New("迁移数据库校验失败")
	}
	var n int
	if err = d.QueryRow(`SELECT COUNT(*) FROM admin WHERE id=1`).Scan(&n); err != nil || n != 1 {
		return errors.New("迁移数据库缺少管理员")
	}
	return nil
}
func CanRestore(db *dbx.DB) bool {
	var n int
	err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM files)+(SELECT COUNT(*) FROM accounts)+(SELECT COUNT(*) FROM albums WHERE is_default=0 AND is_home=0)`).Scan(&n)
	return err == nil && n == 0
}
func Commit(cfg *config.Config, id string) error {
	if !ValidID(id) {
		return errors.New("迁移编号无效")
	}
	if _, err := os.Stat(filepath.Join(WorkDir(cfg), "import-"+id, "manifest.json")); err != nil {
		return errors.New("迁移包尚未校验")
	}
	return atomicJSON(filepath.Join(WorkDir(cfg), "pending.json"), map[string]string{"id": id})
}

func rebase(p, oldRoot, newRoot string) string {
	a, b := strings.ReplaceAll(p, "\\", "/"), strings.TrimRight(strings.ReplaceAll(oldRoot, "\\", "/"), "/")
	if strings.EqualFold(a, b) {
		return newRoot
	}
	if len(a) > len(b) && strings.EqualFold(a[:len(b)], b) && a[len(b)] == '/' {
		return filepath.Join(newRoot, filepath.FromSlash(a[len(b)+1:]))
	}
	return p
}

type runtimeState struct {
	Manifest Manifest `json:"manifest"`
}

// ApplyPending runs only after the old process has closed SQLite and Telegram.
// The previous empty installation is retained next to the imported directory.
func ApplyPending(cfg *config.Config) error {
	work := WorkDir(cfg)
	pending := filepath.Join(work, "pending.json")
	b, err := os.ReadFile(pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &p) != nil || !ValidID(p.ID) {
		return errors.New("待恢复迁移编号无效")
	}
	dir := filepath.Join(work, "import-"+p.ID)
	b, err = os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var m Manifest
	if json.Unmarshal(b, &m) != nil || m.Format != 1 {
		return errors.New("待恢复清单无效")
	}
	root := Root(cfg)
	data := filepath.Join(dir, "data")
	previous := filepath.Join(work, "previous-"+p.ID)
	if _, e := os.Stat(data); e == nil {
		if err = validateDB(filepath.Join(data, "system/pimg.db")); err != nil {
			return err
		}
		key, err := os.ReadFile(filepath.Join(data, filepath.FromSlash(m.MasterPath)))
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(key)))
		if err != nil {
			return err
		}
		master, err := cryptox.NewMaster(raw)
		if err != nil {
			return err
		}
		db, err := dbx.Open(filepath.Join(data, "system/pimg.db"))
		if err != nil {
			return err
		}
		external := map[string]string{}
		for old, rel := range m.External {
			external[old] = filepath.Join(root, filepath.FromSlash(rel))
		}
		err = store.RebaseMigrationStorage(db, master, m.SourceRoot, root, external)
		if err == nil {
			_, err = db.Exec(`UPDATE tasks SET state='pending',lease_until=NULL WHERE state='running'`)
		}
		if err == nil {
			_, err = db.Exec(`INSERT INTO settings(k,v) VALUES('site.public_base_url',?) ON CONFLICT(k) DO NOTHING`, m.Config.Server.PublicBaseURL)
		}
		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		closeErr := db.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = atomicJSON(filepath.Join(data, runtimeFile), runtimeState{m}); err != nil {
			return err
		}
		// Both /pimg/data and /pimg/runtime may be mount points on different
		// filesystems. Keep the data root inode and copy across volumes.
		if err = os.MkdirAll(root, 0700); err != nil {
			return err
		}
		lock := flock.New(filepath.Join(root, ".layout.lock"))
		locked, err := lock.TryLock()
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("目标数据目录正在使用")
		}
		defer lock.Unlock()
		ready := filepath.Join(work, "previous-"+p.ID+".json")
		if _, e = os.Stat(ready); errors.Is(e, os.ErrNotExist) {
			if err = datalayout.CopyTreeExcept(root, previous, filepath.Join(root, ".layout.lock")); err != nil {
				return err
			}
			if err = atomicJSON(ready, map[string]bool{"complete": true}); err != nil {
				return err
			}
		} else if e != nil {
			return e
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == ".layout.lock" {
				continue
			}
			if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
		if err = datalayout.CopyTree(data, root); err != nil {
			return err
		}
	}
	if err = os.Rename(pending, filepath.Join(work, "applied-"+p.ID+".json")); err != nil {
		return err
	}
	return nil
}

// ApplyRuntime restores the saved startup configuration, keeping this machine's
// listening address and data root. No source-machine environment is required.
func ApplyRuntime(cfg *config.Config) error {
	root := Root(cfg)
	b, err := os.ReadFile(filepath.Join(root, runtimeFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state runtimeState
	if json.Unmarshal(b, &state) != nil || state.Manifest.Format != 1 {
		return errors.New("迁移运行配置无效")
	}
	m := state.Manifest
	listen, secure, dataDir, runtimeDir := cfg.Server.Listen, cfg.Server.CookieSecure, cfg.Paths.DataDir, cfg.Paths.RuntimeDir
	*cfg = m.Config
	cfg.Server.Listen = listen
	if secure != nil {
		cfg.Server.CookieSecure = secure
	}
	cfg.Paths.DataDir = dataDir
	cfg.Paths.RuntimeDir = runtimeDir
	cfg.Paths.LegacyRoots = nil
	cfg.Paths.DatabaseFile = ""
	cfg.Paths.InputDir = ""
	cfg.Paths.LegacyDir = ""
	cfg.MasterKey = ""
	cfg.MasterKeyFile = filepath.Join(root, filepath.FromSlash(m.MasterPath))
	cfg.Telegram.SessionFile = filepath.Join(root, filepath.FromSlash(m.SessionPath))
	cfg.Paths.TempDir = filepath.Join(cfg.Paths.RuntimeRoot(), "tmp")
	mapLocal := func(p string) string {
		for old, rel := range m.External {
			if p == old {
				return filepath.Join(root, filepath.FromSlash(rel))
			}
		}
		return rebase(p, m.SourceRoot, root)
	}
	cfg.Storage.LocalColdDir = mapLocal(cfg.Storage.LocalColdDir)
	cfg.Storage.LocalHotDir = mapLocal(cfg.Storage.LocalHotDir)
	return nil
}
