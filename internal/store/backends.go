package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func (s *Service) initBackends() error {
	coldDir, hotDir := s.Cfg.Storage.LocalColdDir, s.Cfg.Storage.LocalHotDir
	if coldDir == "" {
		coldDir = filepath.Join(s.Cfg.Paths.DataDir, "cold")
	}
	if hotDir == "" {
		hotDir = filepath.Join(s.Cfg.Paths.DataDir, "hot")
	}
	// A migrated web configuration is authoritative, even with old file/env
	// paths present. Do not recreate the retired display signing key on restart.
	rows, err := s.DB.StorageInstances()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != "local" || row.Driver != "local" {
			continue
		}
		c, e := s.unsealConfig(row.ID, row.Config)
		if e != nil {
			return e
		}
		if c.LocalLayout == localFolderLayout || c.Bootstrap && c.Managed {
			if c.LocalLayout == localFolderLayout {
				if e = s.validateLocalFolder(c); e != nil {
					return e
				}
			}
			coldDir, hotDir = s.localDirectories(row.ID, c)
		}
	}
	disk, err := storage.NewDisk(coldDir)
	if err != nil {
		return err
	}
	s.localHot, err = storage.NewLocalHot(hotDir, s.PublicBaseURL())
	if err != nil {
		return err
	}
	s.localHot.BaseURLFunc = func() string { return s.PublicBaseURL() + "/media" }
	s.coldBackends = map[string]storage.Cold{"local": storage.LocalCold{Disk: disk}}
	s.hotBackends = map[string]storage.Hot{"local": s.localHot}
	if s.TG != nil {
		s.coldBackends["telegram"] = storage.Telegram{Client: s.TG}
	} else if s.UserTG != nil {
		s.coldBackends["telegram"] = userCold{s: s}
	}
	if s.R2 != nil {
		s.hotBackends["r2"] = storage.R2{Client: s.R2}
	}
	cold, hot := s.Cfg.Storage.Cold, s.Cfg.Storage.Hot
	if cold == "" {
		cold = "telegram"
	}
	if hot == "" {
		hot = "r2"
	}
	// Read choices here as CLI restore does not initialize an HTTP server.
	m, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	if m["site.cold_storage"] != "" {
		cold = m["site.cold_storage"]
	}
	if m["site.hot_storage"] != "" {
		hot = m["site.hot_storage"]
	}
	if s.Cfg.ValidateHot(hot) != nil {
		hot = "local"
	}
	return s.SetStorage(cold, hot)
}

func (s *Service) ValidateStorage(cold, hot string) error {
	if s.storages.instances != nil {
		c, e := s.instance(cold)
		if e != nil {
			return e
		}
		h, e := s.instance(hot)
		if e != nil {
			return e
		}
		if c.cold == nil || h.hot == nil {
			return errors.New("所选存储未配置")
		}
		if c.row.Driver == "telegram" {
			if s.TG != nil && (s.Cfg.Telegram.BotToken == "" || s.Cfg.Telegram.ChannelID == "") {
				return errors.New("模拟 TG 客户端未配置")
			}
			if current, _ := s.StorageSelection(); current != "" && current != cold && s.TG == nil {
				if err := s.TGReady(); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if _, ok := s.coldBackends[cold]; !ok {
		return fmt.Errorf("未知冷存储方案 %q", cold)
	}
	if _, ok := s.hotBackends[hot]; !ok {
		return fmt.Errorf("未知热存储方案 %q", hot)
	}
	if err := s.Cfg.ValidateCold(cold); err != nil {
		return err
	}
	if cold == "telegram" {
		if s.TG != nil {
			if s.Cfg.Telegram.BotToken == "" || s.Cfg.Telegram.ChannelID == "" {
				return errors.New("模拟 TG 客户端未配置")
			}
		} else if current, _ := s.StorageSelection(); current != "" && current != "telegram" {
			if err := s.TGReady(); err != nil {
				return err
			}
		}
	}
	return s.Cfg.ValidateHot(hot)
}
func (s *Service) SetStorage(cold, hot string) error {
	if err := s.ValidateStorage(cold, hot); err != nil {
		return err
	}
	s.dyn.mu.Lock()
	defer s.dyn.mu.Unlock()
	s.dyn.coldStorage, s.dyn.hotStorage = cold, hot
	if s.storages.instances != nil {
		m, _ := s.DB.AllSettings()
		if m["storage.policy"] == "" {
			s.storages.mu.Lock()
			s.storages.policy.Archives = []string{cold}
			s.storages.policy.Permanent = hot
			s.storages.policy.Temporary = hot
			if hot == "r2" {
				if i := s.storages.instances[defaultR2Temporary]; i != nil && i.config.Bootstrap {
					s.storages.policy.Temporary = defaultR2Temporary
				}
			}
			if err := s.DB.SetSetting("storage.bootstrap_policy", dbx.JSONValue(s.storages.policy)); err != nil {
				s.storages.mu.Unlock()
				return err
			}
			s.storages.mu.Unlock()
		} else {
			s.storages.mu.RLock()
			s.dyn.hotStorage = s.storages.policy.Permanent
			s.storages.mu.RUnlock()
		}
	}
	return nil
}
func (s *Service) StorageSelection() (string, string) {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.coldStorage, s.dyn.hotStorage
}
func (s *Service) cold(f *dbx.File) (storage.Cold, error) {
	if s.storages.instances != nil {
		return s.archiveBackend(f.ColdBackend)
	}
	b, ok := s.coldBackends[f.ColdBackend]
	if !ok {
		return nil, fmt.Errorf("归档存储 %q 不可用", f.ColdBackend)
	}
	return b, nil
}
func (s *Service) hot(provider string) (storage.Hot, error) {
	if provider == defaultR2Temporary && s.R2 != nil && s.Cfg.DevMock {
		if i, err := s.instance(provider); err == nil && i.config.Bootstrap {
			return storage.R2{Client: s.R2, Bucket: s.R2.TempBucket}, nil
		}
	}
	if (s.Cfg.DevMock || s.storages.instances == nil) && provider == "local" && s.hotBackends[provider] != nil {
		return s.hotBackends[provider], nil
	}
	if provider == "r2" && s.R2 != nil && s.Cfg.DevMock {
		return storage.R2{Client: s.R2}, nil
	}
	if s.storages.instances != nil {
		i, e := s.instance(provider)
		if e != nil {
			return nil, e
		}
		if i.hot == nil {
			return nil, errors.New("展示存储不可用")
		}
		if provider == "r2" {
			if temp, err := s.instance(defaultR2Temporary); err == nil && temp.hot != nil {
				return legacyR2Hot{Hot: i.hot, temporary: temp.hot}, nil
			}
		}
		return i.hot, nil
	}
	b, ok := s.hotBackends[provider]
	if !ok {
		return nil, fmt.Errorf("展示存储 %q 不可用", provider)
	}
	return b, nil
}
func chunkRef(c *dbx.Chunk) storage.Reference {
	return storage.Reference{Key: c.ObjectKey, MessageID: c.MessageID.Int64, FileID: c.TgFileID.String}
}
func chunkLocated(c *dbx.Chunk) bool {
	return c.ObjectKey != "" || (c.MessageID.Int64 != 0 && c.TgFileID.Valid)
}
func (s *Service) putChunk(ctx context.Context, f *dbx.File, kind string, idx int, data []byte) error {
	if f.ColdBackend == "telegram" && s.UserTG != nil {
		return s.putTelegramChunk(ctx, f, kind, idx, data)
	}
	b, err := s.cold(f)
	if err != nil {
		return err
	}
	sum := sha256Hex(data)
	if kind != "data" || idx != 0 {
		return errors.New("归档仅接受独立完整原件")
	}
	ref, err := s.writeArchiveOriginal(ctx, b, f, &dbx.Chunk{Kind: kind, Idx: idx, Size: int64(len(data)), SHA256: sum}, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("写入 %s 冷存储 %s 分片失败: %w", f.ColdBackend, kind, err)
	}
	return s.DB.UpsertChunk(&dbx.Chunk{FileID: f.ID, Kind: kind, Idx: idx, ObjectKey: ref.Key,
		MessageID: sql.NullInt64{Int64: ref.MessageID, Valid: ref.MessageID != 0}, TgFileID: sql.NullString{String: ref.FileID, Valid: ref.FileID != ""}, Size: int64(len(data)), SHA256: sum})
}
func (s *Service) openChunk(ctx context.Context, f *dbx.File, c *dbx.Chunk) (io.ReadCloser, error) {
	return s.openPeerChunk(ctx, f, c)
}
func (s *Service) deleteTemp(ctx context.Context, obj *dbx.TempObject) error {
	b, err := s.hot(obj.Backend)
	if err != nil {
		return err
	}

	if s.publicR2Temporary(obj.Backend) && obj.ExpiresAt > time.Now().Unix() {
		if managed, ok := b.(interface {
			DeleteManagedTemp(context.Context, string) error
		}); ok {
			return managed.DeleteManagedTemp(ctx, obj.ObjectKey)
		}
	}
	return b.Delete(ctx, storage.Temporary, obj.ObjectKey)
}

func (s *Service) PublicTemporaryEnabled() bool {
	hot := s.TemporaryStorage()
	return s.publicR2Temporary(hot)
}
func (s *Service) tempAvailable(ctx context.Context, obj *dbx.TempObject) bool {
	b, err := s.hot(obj.Backend)
	if err != nil {
		return false
	}
	size, err := b.HeadSize(ctx, storage.Temporary, obj.ObjectKey)
	return err == nil && (!obj.Size.Valid || size == obj.Size.Int64)
}

type StorageOption struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
}

func (s *Service) StorageOptions() map[string][]StorageOption {
	if s.storages.instances != nil {
		out := map[string][]StorageOption{"cold": {}, "hot": {}}
		for _, i := range s.StorageInstances() {
			for _, cap := range i.Capabilities {
				if cap == "archive" {
					out["cold"] = append(out["cold"], StorageOption{ID: i.ID, Name: i.Name, Configured: i.Configured && i.Enabled})
				}
				if cap == "permanent" {
					out["hot"] = append(out["hot"], StorageOption{ID: i.ID, Name: i.Name, Configured: i.Configured && i.Enabled})
				}
			}
		}
		return out
	}
	tgConfigured := s.Cfg.DevMock || (s.UserTG != nil && s.UserTG.Status().State == "authorized")
	return map[string][]StorageOption{
		"cold": {{ID: "local", Name: "本地存储", Configured: true}, {ID: "telegram", Name: "Telegram", Configured: tgConfigured}},
		"hot":  {{ID: "local", Name: "本地存储", Configured: true}, {ID: "r2", Name: "Cloudflare R2", Configured: s.Cfg.ValidateHot("r2") == nil}},
	}
}
func (s *Service) CheckStorage(ctx context.Context, role, provider string) error {
	if role == "cold" {
		b, err := s.cold(&dbx.File{ColdBackend: provider})
		if err != nil {
			return err
		}
		return b.Check(ctx)
	}
	if s.storages.instances != nil {
		i, err := s.instance(provider)
		if err != nil {
			return err
		}
		if i.hot == nil {
			return errors.New("展示存储不可用")
		}
		return i.hot.Check(ctx)
	}
	b, err := s.hot(provider)
	if err != nil {
		return err
	}
	return b.Check(ctx)
}

// ServeLocalMedia gates disk reads against index state before checking signatures.
func (s *Service) ServeLocalMedia(w http.ResponseWriter, r *http.Request) {
	scope, key := r.PathValue("scope"), r.PathValue("key")
	instance := r.PathValue("instance")
	if instance == "" {
		instance = "local"
	}
	i, err := s.instance(instance)
	if err != nil || i.local == nil {
		http.NotFound(w, r)
		return
	}
	allowed := false
	if scope == storage.Permanent {
		id := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(key, "pimg/"), "thumbs/"), filepath.Ext(key)), ".blur")
		if strings.HasPrefix(key, "pimg/") || strings.HasPrefix(key, "thumbs/") {
			rest := strings.TrimPrefix(strings.TrimPrefix(key, "pimg/"), "thumbs/")
			if index := strings.IndexByte(rest, '/'); index >= 0 {
				id = rest[:index]
			}
		}
		f, err := s.DB.GetFile(id)
		allowed = err == nil && !f.Hidden && f.Storage == "direct" && f.Published && f.HotBackend == instance && !f.PurgeState.Valid && (key == origKey(f) || f.HasThumb && key == thumbKey(f) || f.HasBThumb && key == bthumbKey(f))
	} else if scope == storage.Temporary {
		var count int
		err := s.DB.QueryRow(`SELECT COUNT(*) FROM temp_objects t JOIN files f ON f.id=t.file_id WHERE t.object_key=? AND t.backend=? AND t.deleted_at IS NULL AND t.expires_at>? AND f.hidden=0 AND f.purge_state IS NULL`, key, instance, time.Now().Unix()).Scan(&count)
		allowed = err == nil && count > 0
	}
	if !allowed {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	i.local.Serve(w, r, scope, key)
}

func (s *Service) bindPublication(f *dbx.File) error {
	if f.HotBackend != "" {
		return nil
	}
	_, provider := s.StorageSelection()
	_, err := s.DB.Exec(`UPDATE files SET hot_backend=? WHERE id=? AND hot_backend=''`, provider, f.ID)
	if err != nil {
		return err
	}
	current, err := s.DB.GetFile(f.ID)
	if err != nil {
		return err
	}
	f.HotBackend = current.HotBackend
	return nil
}

func unavailableHot(err error) error {
	return errors.Join(errors.New("无法访问记录中的热存储，已保留数据以便重试"), err)
}
