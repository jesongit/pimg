package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"

	"pimg/internal/dbx"
	"pimg/internal/storagehttp"
)

const connectionKey = "storage.connection.v1"

type StorageConnection struct {
	ProxyURL string `json:"proxy_url"`
	APIID    int    `json:"api_id"`
	APIHash  string `json:"api_hash,omitempty"`
}
type connectionState struct {
	mu    sync.RWMutex
	value StorageConnection
}

func (s *Service) storageConnection() StorageConnection {
	s.connection.mu.RLock()
	defer s.connection.mu.RUnlock()
	return s.connection.value
}
func (s *Service) initStorageConnection() error {
	c := StorageConnection{s.Cfg.Storage.ProxyURL, s.Cfg.Telegram.APIID, s.Cfg.Telegram.APIHash}
	m, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	if raw := m[connectionKey]; raw != "" {
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return errors.New("存储连接配置损坏")
		}
		sealed, err := s.unsealConfig(connectionKey, b)
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(sealed.Options["connection"]), &c); err != nil {
			return err
		}
	} else if err = s.persistStorageConnection(c); err != nil {
		return err
	}
	s.connection.value = c
	return nil
}
func (s *Service) persistStorageConnection(c StorageConnection) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := s.sealConfig(connectionKey, InstanceConfig{Options: map[string]string{"connection": string(b)}})
	if err != nil {
		return err
	}
	return s.DB.SetSetting(connectionKey, base64.StdEncoding.EncodeToString(sealed))
}
func (s *Service) StorageConnectionView() map[string]any {
	c := s.storageConnection()
	proxy := c.ProxyURL
	if u, err := url.Parse(proxy); err == nil && u.User != nil {
		proxy = ""
	}
	return map[string]any{"proxy_url": proxy, "has_proxy": c.ProxyURL != "", "api_id": c.APIID, "has_api_hash": c.APIHash != "", "proxy_traffic": s.ProxyTrafficView()}
}

// SaveStorageConnection never replaces the master key or the Telegram session.
// Existing operations finish on their old client; subsequent operations use the
// new transport. Telegram reconnect waits for the old session owner to exit.
func (s *Service) SaveStorageConnection(ctx context.Context, proxy *string, apiID *int, apiHash *string) error {
	c := s.storageConnection()
	old := c
	if proxy != nil {
		c.ProxyURL = strings.TrimSpace(*proxy)
	}
	if apiID != nil {
		c.APIID = *apiID
	}
	if apiHash != nil && strings.TrimSpace(*apiHash) != "" {
		c.APIHash = strings.TrimSpace(*apiHash)
	}
	if _, err := storagehttp.ParseProxyURL(c.ProxyURL); err != nil {
		return err
	}
	if apiID != nil || apiHash != nil {
		if c.APIID <= 0 || len(c.APIHash) != 32 {
			return errors.New("请填写有效的 API ID 和 32 位 API Hash；Hash 留空保留原值")
		}
		for _, r := range c.APIHash {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return errors.New("API Hash 必须是 32 位十六进制字符")
			}
		}
	}
	if err := s.persistStorageConnection(c); err != nil {
		return err
	}
	s.connection.mu.Lock()
	s.connection.value = c
	s.connection.mu.Unlock()
	if old.ProxyURL != c.ProxyURL {
		s.storages.mu.Lock()
		next := make(map[string]*instanceRuntime, len(s.storages.instances))
		for id, i := range s.storages.instances {
			if i.row.Driver != "r2" {
				next[id] = i
				continue
			}
			cold, hot, local, source, err := s.r2Factory(id, i.config)
			if err != nil {
				s.storages.mu.Unlock()
				return err
			}
			next[id] = &instanceRuntime{i.row, i.config, cold, hot, local, source}
		}
		s.storages.instances = next
		s.storages.mu.Unlock()
	}
	if s.telegram != nil && c != old {
		r := s.telegram
		r.lifecycle.Lock()
		defer r.lifecycle.Unlock()
		if err := r.stopLocked(ctx); err != nil {
			return err
		}
		r.mu.Lock()
		r.apiID, r.apiHash = c.APIID, c.APIHash
		root := r.root
		r.mu.Unlock()
		r.makeClientLocked()
		if root != nil && root.Err() == nil {
			if err := r.launchLocked(); err != nil {
				return err
			}
		}
	}
	if old.ProxyURL != c.ProxyURL {
		return s.wakeProxyTrafficTasks()
	}
	return nil
}

// Keep default IDs and object layout while importing file/env values once.
func (s *Service) migrateInstance(row dbx.StorageInstance, c InstanceConfig) (dbx.StorageInstance, InstanceConfig, error) {
	if !c.Bootstrap || c.Managed {
		return row, c, nil
	}
	c.Managed = true
	switch row.Driver {
	case "local":
		c.Directory = s.Cfg.Storage.LocalHotDir
		c.ColdDirectory = s.Cfg.Storage.LocalColdDir
		if c.Directory == "" {
			c.Directory = s.localHot.Disk.Dir
		}
		if c.ColdDirectory == "" {
			c.ColdDirectory = s.Cfg.Paths.DataDir + "/cold"
		}
	case "r2":
		r := s.Cfg.R2
		c.Endpoint, c.AccessKey, c.SecretKey = r.Endpoint, r.AccessKeyID, r.SecretAccessKey
		c.Bucket, c.PublicBase, c.CDNZone, c.CDNToken = r.PermBucket, r.PublicBaseURL, r.CDNZoneID, r.CDNAPIToken
		if row.ID == defaultR2Temporary {
			c.Bucket, c.PublicBase = r.TempBucket, ""
			c.TempPublicBase, c.CleanupBucket, c.TempCacheSeconds = r.TempPublicBaseURL, r.TempCleanupBucket, r.TempCacheSeconds
			if r.TempCDNZoneID != "" {
				c.CDNZone = r.TempCDNZoneID
			}
		}
	}
	b, err := s.sealConfig(row.ID, c)
	if err != nil {
		return row, c, err
	}
	row.Config = b
	err = s.DB.SaveStorageInstance(row)
	return row, c, err
}
