// Package datalayout relocates managed local data without deleting its source.
package datalayout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"pimg/internal/config"
	"sort"
	"strings"
)

type Manifest struct {
	Version int               `json:"version"`
	Origin  string            `json:"origin"`
	Files   map[string]string `json:"files"`
	Roots   []string          `json:"roots,omitempty"`
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

func Hash(path string) (string, error) { return hashContext(context.Background(), path) }
func hashContext(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, contextReader{ctx: ctx, source: f})
	return hex.EncodeToString(h.Sum(nil)), err
}

// CopyTree rejects symlinks and conflicting destinations, and is restartable.
func CopyTree(source, dest string) error { return CopyTreeContext(context.Background(), source, dest) }

// CopyTreeExcept omits a coordination file already locked by the caller.
func CopyTreeExcept(source, dest, skip string) error {
	return copyTree(context.Background(), source, dest, skip)
}

func CopyTreeContext(ctx context.Context, source, dest string) error {
	return copyTree(ctx, source, dest, "")
}

func copyTree(ctx context.Context, source, dest, skip string) error {
	if skip != "" {
		skip, _ = filepath.Abs(skip)
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	if source == dest {
		return nil
	}
	if rel, e := filepath.Rel(source, dest); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("迁移目标不能位于来源目录内部")
	}
	if _, err = os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == skip {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("迁移不跟随符号链接: %s", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			if info, e := os.Lstat(target); e == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("迁移目标目录不能是符号链接")
			}
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("迁移遇到非普通文件: %s", path)
		}
		digest, err := hashContext(ctx, path)
		if err != nil {
			return err
		}
		if info, e := os.Lstat(target); e == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("迁移目标文件不能是符号链接")
		}
		if existing, e := hashContext(ctx, target); e == nil {
			if existing != digest {
				return fmt.Errorf("迁移目标内容冲突: %s", target)
			}
			return nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target+".migrating", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, contextReader{ctx: ctx, source: input})
		syncErr := output.Sync()
		closeErr := output.Close()
		if err = errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		actual, err := hashContext(ctx, target+".migrating")
		if err != nil {
			return err
		}
		if actual != digest {
			return errors.New("迁移复制校验失败")
		}
		return os.Rename(target+".migrating", target)
	})
}

// Relocate maps only paths within the former data directory. Explicit external
// keys, sessions and source directories retain their configured locations.
func Relocate(cfg *config.Config, path string) string {
	if path == "" || cfg.Paths.LegacyDir == "" {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel := ""
	found := false
	bases := append([]string{cfg.Paths.DataDir, cfg.Paths.InputDir, cfg.Paths.LegacyDir}, cfg.Paths.LegacyRoots...)
	sort.SliceStable(bases, func(i, j int) bool { return len(bases[i]) > len(bases[j]) })
	for _, base := range bases {
		if base == "" {
			continue
		}
		candidate, e := filepath.Rel(base, abs)
		if e == nil && candidate != ".." && !strings.HasPrefix(candidate, ".."+string(filepath.Separator)) {
			rel = candidate
			found = true
			break
		}
	}
	if !found {
		return path
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for len(parts) > 1 && parts[0] == "pimg-data" {
		parts = parts[1:]
	}
	suffix := filepath.FromSlash(strings.Join(parts[1:], "/"))
	base := ""
	targetRoot := cfg.Paths.DataDir
	if parts[0] == "work" && len(parts) > 1 && parts[1] == "tmp" {
		parts = parts[1:]
		suffix = filepath.FromSlash(strings.Join(parts[1:], "/"))
	}
	switch parts[0] {
	case "cold":
		base = "storage/local/archives"
	case "hot":
		base = "storage/local/display"
	case "telegram":
		base = "system/telegram"
	case "keys":
		base = "system/keys"
	case "dev-master.key":
		base = "system/keys/dev-master.key"
	case "pimg.db":
		base = "system/pimg.db"
	case "tmp":
		base = "tmp"
		targetRoot = cfg.Paths.RuntimeRoot()
		if len(parts) > 1 && (strings.HasSuffix(parts[len(parts)-1], ".ct") || strings.HasSuffix(parts[len(parts)-1], ".ct.done")) {
			base = "work/cipher"
			targetRoot = cfg.Paths.DataDir
		}
	case "cache", "previews", "diagnostics":
		targetRoot = cfg.Paths.RuntimeRoot()
		base = parts[0]
	case "uploads":
		base = "work/uploads"
	default:
		base = parts[0]
	}
	return filepath.Join(targetRoot, filepath.FromSlash(base), suffix)
}
