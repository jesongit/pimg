package store

import (
	"errors"
	"net/url"
	"path/filepath"
	"pimg/internal/config"
	"strings"
)

func (s *Service) validateInstanceConfig(driver string, c InstanceConfig) error {
	if c.Priority < 0 || c.Priority > 1000000 {
		return errors.New("存储优先级须为 0–1000000，数值越大越优先")
	}
	if c.VideoPartMB < 0 || c.VideoPartMB > 4096 {
		return errors.New("单存储视频分片大小须为 1–4096 MB")
	}
	if driver == "telegram" && c.Bootstrap && c.TopicID != 0 {
		return errors.New("默认 TG 实例按相册自动建立话题；如需独立输出，请新建 TG 实例")
	}
	if driver == "local" {
		if err := s.validateLocalFolder(c); err != nil {
			return err
		}
		directories := []string{c.Directory, c.ColdDirectory}
		if c.LocalPaths != nil {
			directories = append(directories, c.LocalPaths.Archives, c.LocalPaths.Display)
		}
		for _, dir := range directories {
			dir = s.localPath(dir)
			if dir == "" {
				continue
			}
			for _, p := range []string{s.upDir, s.ctDir, filepath.Join(s.Cfg.Paths.DataDir, "telegram"), filepath.Join(s.Cfg.Paths.DataDir, "keys"), filepath.Join(s.Cfg.Paths.DataDir, "system")} {
				if pathsOverlap(dir, p) {
					return errors.New("存储目录不能与上传、工作目录、密钥或 TG 会话目录重叠")
				}
			}
		}
	}
	if driver != "r2" {
		return nil
	}
	if c.Bootstrap && c.Prefix != "" {
		return errors.New("原有 R2 实例沿用原路径布局；如需前缀，请新建实例")
	}
	for _, raw := range []string{c.Endpoint, c.PublicBase, c.TempPublicBase} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("R2 端点和公开域名须为完整 HTTP(S) 地址，不含凭据、查询参数或片段")
		}
	}
	if strings.Contains(c.Prefix, "..") {
		return errors.New("R2 前缀不能包含 ..")
	}
	if c.TempPublicBase != "" {
		r := config.Config{R2: config.R2{TempBucket: c.Bucket, PermBucket: c.Bucket, TempPublicBaseURL: c.TempPublicBase, TempCleanupBucket: c.CleanupBucket, TempCDNZoneID: c.CDNZone, CDNAPIToken: c.CDNToken}}
		if err := r.ValidatePublicTemp(); err != nil {
			return err
		}
	}
	return nil
}
