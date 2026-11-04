package store

import (
	"errors"
	"os"
	"path/filepath"
)

// CreateBrowserDir creates a subdirectory under parent.
func (s *Service) CreateBrowserDir(parent, name string) error {
	st, err := os.Stat(parent)
	if err != nil || !st.IsDir() {
		return errors.New("目标目录不存在或不可写")
	}
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0755); err != nil {
		return errors.New("创建文件夹失败: " + err.Error())
	}
	return nil
}

// HardlinkBrowserItems hard-links the given files (or every file inside a
// directory) into target, preserving the item name. Directories recurse
// creating mirrored directory trees with hard-linked files.
func (s *Service) HardlinkBrowserItems(paths []string, target string) error {
	ti, err := os.Stat(target)
	if err != nil || !ti.IsDir() {
		return errors.New("目标目录不存在或不可写")
	}
	for _, src := range paths {
		si, err := os.Stat(src)
		if err != nil {
			return errors.New("源不存在: " + src)
		}
		if si.IsDir() {
			if err := linkDir(src, filepath.Join(target, filepath.Base(src))); err != nil {
				return err
			}
		} else {
			dest := filepath.Join(target, filepath.Base(src))
			if err := linkFile(src, dest); err != nil {
				return err
			}
		}
	}
	return nil
}

func linkFile(src, dest string) error {
	// Prefer hard link; if the destination already exists (e.g. re-running),
	// overwrite by removing existing entry first.
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(dest)
	}
	if err := os.Link(src, dest); err != nil {
		return errors.New("硬链接失败: " + dest + ": " + err.Error())
	}
	return nil
}

func linkDir(src, dest string) error {
	if err := os.MkdirAll(dest, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("创建文件夹失败: " + dest + ": " + err.Error())
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dest, e.Name())
		if e.IsDir() {
			if err := linkDir(sp, dp); err != nil {
				return err
			}
		} else {
			if err := linkFile(sp, dp); err != nil {
				return err
			}
		}
	}
	return nil
}