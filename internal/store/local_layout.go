package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"strings"
)

const localFolderLayout = 2

// LocalFolderPaths retains legacy physical locations. It is compatibility
// metadata, not a second user-configurable folder or a request to move data.
type LocalFolderPaths struct {
	Archives string `json:"archives"`
	Display  string `json:"display"`
}

func (s *Service) localPath(path string) string {
	if strings.HasPrefix(path, "@data/") {
		return filepath.Join(s.Cfg.Paths.DataDir, filepath.FromSlash(strings.TrimPrefix(path, "@data/")))
	}
	return datalayout.Relocate(s.Cfg, path)
}

// Adopt the single-folder configuration without copying, moving or hashing any
// media. Arbitrary old paths remain the actual read AND write locations.
func (s *Service) migrateLocalLayout(row dbx.StorageInstance, c InstanceConfig) (dbx.StorageInstance, InstanceConfig, error) {
	if row.Driver != "local" {
		return row, c, nil
	}
	if c.LocalLayout < 0 || c.LocalLayout > localFolderLayout {
		return row, c, fmt.Errorf("本地存储 %s 的目录版本 %d 不受当前程序支持", row.Name, c.LocalLayout)
	}
	if c.LocalLayout == localFolderLayout {
		return row, c, nil
	}
	if !c.Bootstrap && strings.TrimSpace(c.Directory) == "" {
		return row, c, errors.New("本地存储缺少存储文件夹，已保留原配置")
	}
	cold, hot := s.localDirectories(row.ID, c)
	root := filepath.Dir(cold)
	next := c
	if c.Bootstrap && !(filepath.Base(cold) == "archives" && filepath.Base(hot) == "display" && sameLocalPath(filepath.Dir(cold), filepath.Dir(hot))) {
		root = cold
		next.LocalPaths = &LocalFolderPaths{Archives: s.portableLocalPath(cold), Display: s.portableLocalPath(hot)}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return row, c, err
	}
	next.Directory, next.ColdDirectory, next.LocalLayout, next.Managed = s.portableLocalPath(root), "", localFolderLayout, true
	data, err := s.sealConfig(row.ID, next)
	if err != nil {
		return row, c, err
	}
	updated := row
	updated.Config = data
	if err = s.DB.SaveStorageInstance(updated); err != nil {
		return row, c, err
	}
	return updated, next, nil
}

func (s *Service) localDirectories(id string, c InstanceConfig) (string, string) {
	root := s.localPath(c.Directory)
	if c.LocalLayout == localFolderLayout {
		if c.LocalPaths != nil {
			return s.localPath(c.LocalPaths.Archives), s.localPath(c.LocalPaths.Display)
		}
		return filepath.Join(root, "archives"), filepath.Join(root, "display")
	}
	if c.Bootstrap {
		cold := s.localPath(c.ColdDirectory)
		if cold == "" {
			cold = s.Cfg.Storage.LocalColdDir
		}
		if cold == "" {
			cold = filepath.Join(s.Cfg.Paths.DataDir, "cold")
		}
		if root == "" {
			root = s.Cfg.Storage.LocalHotDir
		}
		if root == "" {
			root = filepath.Join(s.Cfg.Paths.DataDir, "hot")
		}
		return cold, root
	}
	if c.LocalLayout == 1 {
		root = filepath.Join(root, "pimg-data", "storage", id)
	}
	return filepath.Join(root, "archives"), filepath.Join(root, "display")
}

func localWithin(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameLocalPath(a, b string) bool { return localWithin(a, b) && localWithin(b, a) }

func (s *Service) portableLocalPath(path string) string {
	if localWithin(s.Cfg.Paths.DataDir, path) {
		rel, _ := filepath.Rel(s.Cfg.Paths.DataDir, path)
		return "@data/" + filepath.ToSlash(rel)
	}
	return path
}

func (s *Service) validateLocalFolder(c InstanceConfig) error {
	if strings.TrimSpace(c.Directory) == "" {
		return errors.New("请填写存储文件夹")
	}
	if c.LocalLayout != localFolderLayout {
		return errors.New("本地存储目录版本无效，请重新加载后重试")
	}
	if c.ColdDirectory != "" {
		return errors.New("本地存储只需填写存储文件夹，内部路径由系统管理")
	}
	if c.LocalPaths != nil && (strings.TrimSpace(c.LocalPaths.Archives) == "" || strings.TrimSpace(c.LocalPaths.Display) == "") {
		return errors.New("旧本地存储的兼容路径不完整，已保留配置")
	}
	return nil
}
