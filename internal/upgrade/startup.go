// Package upgrade protects the first startup of a new data/config version.
package upgrade

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

type Manifest struct {
	CreatedAt    int64             `json:"created_at"`
	FromSchema   int               `json:"from_schema"`
	ToSchema     int               `json:"to_schema"`
	FromPolicy   int               `json:"from_policy"`
	ToPolicy     int               `json:"to_policy"`
	FromStartup  int               `json:"from_startup"`
	ToStartup    int               `json:"to_startup"`
	SourceDB     string            `json:"source_database"`
	SourceDigest string            `json:"source_digest"`
	Files        map[string]string `json:"sha256"`
}

// BeforeLayout is called with the data lock held, before layout/schema/config
// writes. Legacy layout guards already own a SQLite connection; reuse it.
func BeforeLayout(cfg *config.Config, path string, guarded *sql.DB) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db := guarded
	if db == nil {
		var err error
		db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(1000)")
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
	}
	version, err := dbx.ReadSchemaVersion(db)
	if err != nil {
		return err
	}
	if version == 0 {
		return nil
	}
	var policyText string
	err = db.QueryRow(`SELECT v FROM settings WHERE k=?`, store.StoragePolicyVersionKey).Scan(&policyText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	policy := 0
	if policyText != "" {
		policy, err = strconv.Atoi(policyText)
		if err != nil || policy < 0 || policy > store.StoragePolicyVersion {
			return fmt.Errorf("存储策略版本 %q 不受当前程序支持", policyText)
		}
	}
	layoutCurrent := false
	if b, e := os.ReadFile(filepath.Join(cfg.Paths.DataDir, ".layout-v2.json")); e == nil {
		var marker datalayout.Manifest
		layoutCurrent = json.Unmarshal(b, &marker) == nil && marker.Version == 2
	}
	if cfg.StartupVersion > config.CurrentStartupVersion {
		return errors.New("启动配置版本高于当前程序")
	}
	if version == dbx.CurrentSchemaVersion() && policy == store.StoragePolicyVersion && layoutCurrent && cfg.StartupVersion == config.CurrentStartupVersion {
		return nil
	}
	var check string
	if err = db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("升级前数据库校验失败: %s", check)
	}
	parent := filepath.Join(cfg.Paths.DataDir, "system", "upgrade-backups")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("不能创建升级备份，已停止启动: %w", err)
	}
	digest, err := sourceFingerprint(cfg, path)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		oldDir := filepath.Join(parent, entry.Name())
		b, e := os.ReadFile(filepath.Join(oldDir, "manifest.json"))
		var old Manifest
		if e != nil || json.Unmarshal(b, &old) != nil || old.SourceDigest != digest || old.ToSchema != dbx.CurrentSchemaVersion() || old.ToPolicy != store.StoragePolicyVersion || old.ToStartup != config.CurrentStartupVersion || old.Files["pimg.db"] == "" {
			continue
		}
		valid := true
		for name, sha := range old.Files {
			if !filepath.IsLocal(name) {
				valid = false
				break
			}
			got, e := datalayout.Hash(filepath.Join(oldDir, filepath.FromSlash(name)))
			if e != nil || got != sha {
				valid = false
				break
			}
		}
		if valid {
			fmt.Fprintf(os.Stderr, "复用已验证的升级前备份: %s\n", oldDir)
			return validateExistingConfig(cfg, path, db)
		}
	}
	dir, err := os.MkdirTemp(parent, fmt.Sprintf("v%d-to-v%d-%s-", version, dbx.CurrentSchemaVersion(), time.Now().UTC().Format("20060102T150405Z")))
	if err != nil {
		return fmt.Errorf("不能创建升级备份，已停止启动: %w", err)
	}
	manifest := Manifest{CreatedAt: time.Now().Unix(), FromSchema: version, ToSchema: dbx.CurrentSchemaVersion(), FromPolicy: policy, ToPolicy: store.StoragePolicyVersion, FromStartup: cfg.StartupVersion, ToStartup: config.CurrentStartupVersion, SourceDB: path, SourceDigest: digest, Files: map[string]string{}}
	backupDB := filepath.Join(dir, "pimg.db")
	if _, err = db.Exec(`VACUUM INTO '` + strings.ReplaceAll(filepath.ToSlash(backupDB), "'", "''") + `'`); err != nil {
		return fmt.Errorf("升级数据库备份失败，已停止启动（未完成备份 %s）: %w", dir, err)
	}
	if err = os.Chmod(backupDB, 0600); err != nil {
		return err
	}
	if manifest.Files["pimg.db"], err = datalayout.Hash(backupDB); err != nil {
		return err
	}
	verify, err := sql.Open("sqlite", "file:"+filepath.ToSlash(backupDB)+"?mode=ro")
	if err != nil {
		return err
	}
	err = verify.QueryRow(`PRAGMA integrity_check`).Scan(&check)
	verify.Close()
	if err != nil || check != "ok" {
		return errors.New("升级备份校验失败，已停止启动")
	}
	copyFile := func(source, name string) error {
		info, e := os.Lstat(source)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("升级备份只接受普通文件: %s", source)
		}
		in, e := os.Open(source)
		if e != nil {
			return e
		}
		defer in.Close()
		dest := filepath.Join(dir, name)
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		e = errors.Join(e, out.Sync(), out.Close())
		if e != nil {
			return e
		}
		manifest.Files[filepath.ToSlash(name)], e = datalayout.Hash(dest)
		return e
	}
	sourceRoot := filepath.Dir(path)
	if filepath.Base(sourceRoot) == "system" {
		sourceRoot = filepath.Dir(sourceRoot)
	}
	for _, name := range []string{"system/keys", "system/telegram", "keys", "telegram"} {
		base := filepath.Join(sourceRoot, filepath.FromSlash(name))
		if _, e := os.Stat(base); errors.Is(e, os.ErrNotExist) {
			continue
		} else if e != nil {
			return e
		}
		if err = filepath.WalkDir(base, func(p string, entry os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if entry.IsDir() || strings.HasSuffix(entry.Name(), ".lock") {
				return nil
			}
			rel, e := filepath.Rel(sourceRoot, p)
			if e != nil {
				return e
			}
			return copyFile(p, filepath.Join("data", rel))
		}); err != nil {
			return err
		}
	}
	for _, name := range []string{"system/startup.json", "dev-master.key", ".layout-v1.json", ".layout-v2.json"} {
		if err = copyFile(filepath.Join(sourceRoot, filepath.FromSlash(name)), filepath.Join("data", filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	for name, source := range map[string]string{"master.key": cfg.MasterKeyFile, "session.enc": cfg.Telegram.SessionFile} {
		if source != "" {
			if err = copyFile(source, filepath.Join("configured", name)); err != nil {
				return err
			}
		}
	}
	if cfg.MasterKey != "" {
		name := "configured/master-inline.key"
		if err = os.MkdirAll(filepath.Join(dir, "configured"), 0700); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(cfg.MasterKey), 0600); err != nil {
			return err
		}
		manifest.Files[name], err = datalayout.Hash(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
	}
	after, err := sourceFingerprint(cfg, path)
	if err != nil {
		return err
	}
	if after != digest {
		return errors.New("备份期间来源数据发生变化，已停止启动；请确认旧服务已停止")
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	// This final receipt, absent for incomplete backups, is the restore boundary.
	receipt, err := os.OpenFile(filepath.Join(dir, "manifest.json.pending"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = receipt.Write(b)
	if err = errors.Join(err, receipt.Sync(), receipt.Close()); err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(dir, "manifest.json.pending"), filepath.Join(dir, "manifest.json")); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "升级前备份已验证: %s（数据库 v%d → v%d，策略 v%d → v%d）\n", dir, version, dbx.CurrentSchemaVersion(), policy, store.StoragePolicyVersion)
	return validateExistingConfig(cfg, path, db)
}

func validateExistingConfig(cfg *config.Config, path string, db *sql.DB) error {
	kept := *cfg
	if kept.MasterKey == "" {
		root := filepath.Dir(path)
		if filepath.Base(root) == "system" {
			root = filepath.Dir(root)
		}
		found := false
		for _, p := range []string{cfg.MasterKeyFile, filepath.Join(root, "system/keys/master.key"), filepath.Join(root, "system/keys/dev-master.key"), filepath.Join(root, "keys/master.key"), filepath.Join(root, "dev-master.key")} {
			if p == "" {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				kept.MasterKeyFile = p
				found = true
				break
			}
		}
		if !found {
			return errors.New("已有数据库但未找到原主密钥，拒绝自动升级或生成新密钥")
		}
	}
	key, err := kept.ResolveMasterKey()
	if err != nil {
		return err
	}
	master, err := cryptox.NewMaster(key)
	if err != nil {
		return err
	}
	return store.ValidateStorageCredentials(&dbx.DB{DB: db}, master)
}

func sourceFingerprint(cfg *config.Config, path string) (string, error) {
	root := filepath.Dir(path)
	if filepath.Base(root) == "system" {
		root = filepath.Dir(root)
	}
	paths := []string{path, path + "-wal", cfg.MasterKeyFile, cfg.Telegram.SessionFile}
	for _, name := range []string{"system/startup.json", "dev-master.key", ".layout-v1.json", ".layout-v2.json", "system/keys", "system/telegram", "keys", "telegram"} {
		base := filepath.Join(root, filepath.FromSlash(name))
		e := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.IsDir() && !strings.HasSuffix(d.Name(), ".lock") {
				paths = append(paths, p)
			}
			return nil
		})
		if e != nil {
			return "", e
		}
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		if p == "" {
			continue
		}
		sha, err := datalayout.Hash(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\n", p, sha)
	}
	_, _ = h.Write([]byte(cfg.MasterKey))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// A retained database must never cause a missing key to be silently replaced.
func RequireExistingKey(cfg *config.Config) error {
	if cfg.MasterKey != "" {
		return nil
	}
	if info, err := os.Stat(cfg.Paths.DatabaseFile); errors.Is(err, os.ErrNotExist) || err == nil && info.Size() == 0 {
		return nil
	} else if err != nil {
		return err
	}
	if cfg.MasterKeyFile == "" {
		return errors.New("已有数据库但未配置原主密钥，拒绝生成新密钥")
	}
	if _, err := os.Stat(cfg.MasterKeyFile); err != nil {
		return fmt.Errorf("已有数据库但原主密钥不可用，拒绝生成新密钥: %w", err)
	}
	return nil
}
