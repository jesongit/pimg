package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/r2x"
	"pimg/internal/storage"
	"pimg/internal/storagehttp"
	"pimg/internal/tguser"
)

// InstanceConfig is adapter-owned configuration. Secrets are encrypted with a
// domain-separated key and are never sent back to administration clients.
type InstanceConfig struct {
	PolicyVersion    int               `json:"policy_version"`
	Priority         int               `json:"priority"`
	Encrypt          bool              `json:"encrypt"`
	SplitVideo       bool              `json:"split_video"`
	VideoPartMB      int               `json:"video_part_mb"`
	LocalLayout      int               `json:"local_layout,omitempty"`
	LocalPaths       *LocalFolderPaths `json:"local_paths,omitempty"`
	Options          map[string]string `json:"options,omitempty"`
	Directory        string            `json:"directory,omitempty"`
	Endpoint         string            `json:"endpoint,omitempty"`
	AccessKey        string            `json:"access_key,omitempty"`
	SecretKey        string            `json:"secret_key,omitempty"`
	Bucket           string            `json:"bucket,omitempty"`
	Prefix           string            `json:"prefix,omitempty"`
	PublicBase       string            `json:"public_base,omitempty"`
	TopicID          int               `json:"topic_id,omitempty"`
	ColdDirectory    string            `json:"cold_directory,omitempty"`
	TempPublicBase   string            `json:"temp_public_base,omitempty"`
	CleanupBucket    string            `json:"cleanup_bucket,omitempty"`
	TempCacheSeconds int               `json:"temp_cache_seconds,omitempty"`
	CDNZone          string            `json:"cdn_zone,omitempty"`
	CDNToken         string            `json:"cdn_token,omitempty"`
	Managed          bool              `json:"managed,omitempty"`
	Bootstrap        bool              `json:"bootstrap,omitempty"`
}
type StoragePolicy struct {
	Archives      []string `json:"archives"`
	Permanent     string   `json:"permanent"`
	Temporary     string   `json:"temporary"`
	NormalMB      int      `json:"normal_mbps"`
	AcceleratedMB int      `json:"accelerated_mbps"`
	LocalMB       int      `json:"local_mbps"`
	Start         string   `json:"accelerated_start"`
	End           string   `json:"accelerated_end"`
	SyncCacheMB   int      `json:"sync_cache_mb"`
}
type InstanceView struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Driver         string         `json:"driver"`
	Enabled        bool           `json:"enabled"`
	Config         InstanceConfig `json:"config"`
	Capabilities   []string       `json:"capabilities"`
	HasCredentials bool           `json:"has_credentials"`
	Configured     bool           `json:"configured"`
	Health         StorageHealth  `json:"health"`
}
type instanceRuntime struct {
	row    dbx.StorageInstance
	config InstanceConfig
	cold   storage.Cold
	hot    storage.Hot
	local  *storage.LocalHot
	source SourceDriver
}
type sourceScore struct {
	available      *bool
	latencyMS      float64
	lastError      string
	failures       int
	bytesPerSecond float64
	at             time.Time
}
type storageRuntime struct {
	mu        sync.RWMutex
	lastProbe time.Time
	instances map[string]*instanceRuntime
	policy    StoragePolicy
	scores    map[string]sourceScore
	factories map[string]InstanceFactory
}
type InstanceFactory func(string, InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error)

func (s *Service) configCipher() cipher.AEAD {
	k := sha256.Sum256(append([]byte("pimg/storage-config/v1\x00"), s.Master.Raw()...))
	b, _ := aes.NewCipher(k[:])
	g, _ := cipher.NewGCM(b)
	return g
}
func (s *Service) sealConfig(id string, c InstanceConfig) ([]byte, error) {
	if c.PolicyVersion == 0 {
		c.PolicyVersion = StoragePolicyVersion
	}
	b, e := json.Marshal(c)
	if e != nil {
		return nil, e
	}
	g := s.configCipher()
	n := make([]byte, g.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return g.Seal(n, n, b, []byte(id)), nil
}
func (s *Service) unsealConfig(id string, b []byte) (InstanceConfig, error) {
	var c InstanceConfig
	p, err := s.unsealConfigJSON(id, b)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(p, &c)
	if err == nil && c.PolicyVersion > StoragePolicyVersion {
		err = errors.New("存储配置版本高于当前程序，请使用新版本")
	}
	return c, err
}

func (s *Service) unsealConfigJSON(id string, b []byte) ([]byte, error) {
	g := s.configCipher()
	if len(b) < g.NonceSize() {
		return nil, errors.New("存储配置损坏")
	}
	p, e := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(id))
	if e != nil {
		return nil, errors.New("存储配置解密失败，请检查主密钥")
	}
	return p, nil
}

func (s *Service) initInstances() error {
	s.storages.instances = map[string]*instanceRuntime{}
	s.storages.scores = map[string]sourceScore{}
	s.storages.factories = map[string]InstanceFactory{"local": s.localFactory, "r2": s.r2Factory, "telegram": s.telegramFactory}
	for _, driver := range s.extraDrivers {
		if driver.Open == nil {
			return errors.New("扩展存储驱动缺少构造器")
		}
		s.storages.factories[driver.ID] = func(id string, c InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) {
			return driver.Open(s, id, c)
		}
	}
	rows, e := s.DB.StorageInstances()
	if e != nil {
		return e
	}
	if len(rows) == 0 {
		for _, v := range []struct {
			id, name, driver string
			c                InstanceConfig
		}{{"local", "本地存储", "local", InstanceConfig{Directory: s.localHot.Disk.Dir, Bootstrap: true, VideoPartMB: 256}}, {"telegram", "Telegram", "telegram", InstanceConfig{Bootstrap: true, Encrypt: true, SplitVideo: true, VideoPartMB: 256}}, {"r2", "Cloudflare R2", "r2", InstanceConfig{Bootstrap: true, VideoPartMB: 256}}} {
			b, err := s.sealConfig(v.id, v.c)
			if err != nil {
				return err
			}
			i := dbx.StorageInstance{ID: v.id, Name: v.name, Driver: v.driver, Config: b, Enabled: true}
			if err = s.DB.SaveStorageInstance(i); err != nil {
				return err
			}
			rows = append(rows, i)
		}
	}
	rows, e = s.splitDefaultR2(rows)
	if e != nil {
		return e
	}
	for _, i := range rows {
		c, err := s.unsealConfig(i.ID, i.Config)
		if err != nil {
			return err
		}
		i, c, err = s.migrateInstance(i, c)
		if err != nil {
			return err
		}
		i, c, err = s.migrateLocalLayout(i, c)
		if err != nil {
			return err
		}
		if err = s.loadInstance(i, c); err != nil {
			return fmt.Errorf("加载存储 %s: %w", i.Name, err)
		}
	}
	m, e := s.DB.AllSettings()
	if e != nil {
		return e
	}
	cold, hot := s.StorageSelection()
	p := StoragePolicy{Archives: []string{cold}, Permanent: hot, Temporary: hot, NormalMB: 1, AcceleratedMB: 8, LocalMB: 64, Start: "02:00", End: "06:00", SyncCacheMB: 4096}
	if raw := m["storage.policy"]; raw != "" {
		if e = json.Unmarshal([]byte(raw), &p); e != nil {
			return e
		}
	} else if raw := m["storage.bootstrap_policy"]; raw != "" {
		if e = json.Unmarshal([]byte(raw), &p); e != nil {
			return e
		}
	}
	if i := s.storages.instances["r2"]; i != nil && i.config.Bootstrap && p.Temporary == "r2" {
		p.Temporary = defaultR2Temporary
		if m["storage.policy"] != "" {
			if e = s.DB.SetSetting("storage.policy", dbx.JSONValue(p)); e != nil {
				return e
			}
		}
	}
	if m["storage.policy"] == "" {
		if e = s.DB.SetSetting("storage.bootstrap_policy", dbx.JSONValue(p)); e != nil {
			return e
		}
	}
	s.storages.policy = p
	s.dyn.mu.Lock()
	s.dyn.hotStorage = p.Permanent
	if len(p.Archives) > 0 {
		s.dyn.coldStorage = p.Archives[0]
	}
	s.dyn.mu.Unlock()
	return s.restoreStorageHealth()
}

// RegisterInstanceDriver is the sole extension point needed by future adapters.
func (s *Service) RegisterInstanceDriver(name string, f InstanceFactory) {
	s.storages.mu.Lock()
	defer s.storages.mu.Unlock()
	s.storages.factories[name] = f
}
func (s *Service) loadInstance(row dbx.StorageInstance, c InstanceConfig) error {
	if row.Driver == "r2" && c.Bootstrap && !c.Managed {
		c.Endpoint = s.Cfg.R2.Endpoint
		c.Bucket, c.PublicBase = s.Cfg.R2.PermBucket, s.Cfg.R2.PublicBaseURL
		if row.ID == defaultR2Temporary {
			c.Bucket, c.PublicBase = s.Cfg.R2.TempBucket, ""
		}
	}
	factory := s.storages.factories[row.Driver]
	if factory == nil {
		return errors.New("未注册的存储驱动")
	}
	cold, hot, local, source, e := factory(row.ID, c)
	if e != nil {
		return e
	}
	s.storages.instances[row.ID] = &instanceRuntime{row: row, config: c, cold: cold, hot: hot, local: local, source: source}
	if row.ID == "local" && row.Driver == "local" {
		s.localHot = local
		s.coldBackends["local"], s.hotBackends["local"] = cold, hot
		s.Cfg.Storage.LocalColdDir, s.Cfg.Storage.LocalHotDir = s.localDirectories(row.ID, c)
	}
	return nil
}
func (s *Service) localFactory(id string, c InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) {
	c.Directory = s.localPath(c.Directory)
	c.ColdDirectory = s.localPath(c.ColdDirectory)
	if c.Bootstrap && c.LocalLayout != localFolderLayout {
		if cold, ok := s.coldBackends["local"].(storage.LocalCold); ok && cold.Disk.Dir == c.ColdDirectory && s.localHot.Disk.Dir == c.Directory {
			return cold, s.localHot, s.localHot, &localSource{dir: c.Directory, pace: s.localCopyReader}, nil
		}
		disk, err := storage.NewDisk(c.ColdDirectory)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		hot, err := storage.NewLocalHot(c.Directory, s.PublicBaseURL())
		if err != nil {
			return nil, nil, nil, nil, err
		}
		hot.BaseURLFunc = func() string { return s.PublicBaseURL() + "/media" }
		return storage.LocalCold{Disk: disk}, hot, hot, &localSource{dir: c.Directory, pace: s.localCopyReader}, nil
	}
	if strings.TrimSpace(c.Directory) == "" {
		return nil, nil, nil, nil, errors.New("请填写存储文件夹")
	}
	root, e := filepath.Abs(c.Directory)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	for _, protected := range []string{s.upDir, s.ctDir, filepath.Join(s.Cfg.Paths.DataDir, "telegram"), filepath.Join(s.Cfg.Paths.DataDir, "keys"), filepath.Join(s.Cfg.Paths.DataDir, "system")} {
		if pathsOverlap(root, protected) {
			return nil, nil, nil, nil, errors.New("存储根目录不能与运行数据或工作目录重叠")
		}
	}
	coldDir, hotDir := s.localDirectories(id, c)
	disk, e := storage.NewDisk(coldDir)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	hot, e := storage.NewLocalHot(hotDir, s.PublicBaseURL())
	if e != nil {
		return nil, nil, nil, nil, e
	}
	hot.BaseURLFunc = func() string { return s.PublicBaseURL() + "/media/storage/" + id }
	if c.Bootstrap {
		hot.BaseURLFunc = func() string { return s.PublicBaseURL() + "/media" }
	}
	return storage.LocalCold{Disk: disk}, hot, hot, &localSource{dir: root, pace: s.localCopyReader}, nil
}
func (s *Service) r2Factory(id string, c InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) {
	cl := s.R2
	if !c.Bootstrap || c.Managed && !s.Cfg.DevMock {
		if c.Endpoint == "" || c.AccessKey == "" || c.SecretKey == "" || c.Bucket == "" {
			if c.Bootstrap {
				return nil, nil, nil, nil, nil
			}
			return nil, nil, nil, nil, errors.New("请填写 R2 端点、存储桶和访问凭据")
		}
		transport, err := storagehttp.NewTransport(s.storageConnection().ProxyURL)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		cl = r2x.New(c.Endpoint, c.AccessKey, c.SecretKey, c.Bucket, c.Bucket, c.PublicBase, c.CDNZone, c.CDNToken, r2x.WithHTTPTransport(transport), r2x.WithPublicTemp(c.TempPublicBase, c.CleanupBucket, c.CDNZone, c.TempCacheSeconds))
	}
	if cl == nil {
		return nil, nil, nil, nil, nil
	}
	if c.Bootstrap && id == defaultR2Temporary {
		hot := storage.R2{Client: cl, Bucket: cl.TempBucket}
		return nil, hot, nil, &r2Source{client: cl, bucket: cl.TempBucket}, nil
	}
	bucket := c.Bucket
	if bucket == "" {
		bucket = cl.PermBucket
	}
	r := storage.ObjectStore{Client: cl, Bucket: bucket, Prefix: strings.Trim(c.Prefix, "/"), PublicBase: c.PublicBase}
	var hot storage.Hot = storage.DisplayObjectStore{ObjectStore: r}
	if c.Bootstrap {
		hot = storage.R2{Client: cl, Bucket: cl.PermBucket, PublicBase: cl.PublicBase}
	}
	return r, hot, nil, &r2Source{client: cl, bucket: bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
}
func (s *Service) telegramFactory(id string, c InstanceConfig) (storage.Cold, storage.Hot, *storage.LocalHot, SourceDriver, error) {
	if c.Bootstrap {
		if s.UserTG != nil {
			return topicCold{s: s, instance: id}, nil, nil, &telegramSource{s: s}, nil
		}
		return s.coldBackends["telegram"], nil, nil, &telegramSource{s: s}, nil
	}
	return topicCold{s: s, topic: c.TopicID, instance: id}, nil, nil, &telegramSource{s: s}, nil
}
func (s *Service) instance(id string) (*instanceRuntime, error) {
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	i := s.storages.instances[id]
	if i == nil {
		return nil, fmt.Errorf("存储实例 %q 不存在", id)
	}
	return i, nil
}
func (s *Service) StoragePolicy() StoragePolicy {
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	p := s.storages.policy
	p.Archives = append([]string{}, p.Archives...)
	return p
}
func (s *Service) TemporaryStorage() string { return s.StoragePolicy().Temporary }
func (s *Service) StorageInstanceName(id string) string {
	i, err := s.instance(id)
	if err != nil {
		return id
	}
	return i.row.Name
}
func (s *Service) StorageInstances() []InstanceView {
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	out := []InstanceView{}
	for _, i := range s.storages.instances {
		c := i.config
		if c.VideoPartMB == 0 {
			c.VideoPartMB = 256
		}
		if i.row.Driver == "local" {
			c.Directory = s.localPath(c.Directory)
			c.ColdDirectory = s.localPath(c.ColdDirectory)
			if c.LocalPaths != nil {
				c.LocalPaths = &LocalFolderPaths{Archives: s.localPath(c.LocalPaths.Archives), Display: s.localPath(c.LocalPaths.Display)}
			}
		}
		has := c.SecretKey != "" || (c.Bootstrap && i.row.Driver == "r2" && s.R2 != nil)
		c.AccessKey = ""
		c.SecretKey = ""
		c.CDNToken = ""
		c.Options = nil
		caps := []string{}
		if i.cold != nil {
			caps = append(caps, "archive")
		}
		if i.hot != nil {
			if !(i.row.ID == "r2" && i.config.Bootstrap) {
				caps = append(caps, "temporary")
			}
			if i.hot.PublicURL(".capability") != "" {
				caps = append(caps, "permanent")
			}
		}
		configured := true
		if i.row.Driver == "telegram" {
			configured = s.TG != nil || (s.UserTG != nil && s.UserTG.Status().State == "authorized")
		}
		if i.row.Driver == "r2" && i.config.Bootstrap {
			configured = i.hot != nil
		}
		v := s.storages.scores[i.row.ID]
		out = append(out, InstanceView{ID: i.row.ID, Name: i.row.Name, Driver: i.row.Driver, Enabled: i.row.Enabled, Config: c, Capabilities: caps, HasCredentials: has, Configured: configured, Health: StorageHealth{Available: v.available, LatencyMS: v.latencyMS, BytesPerSecond: v.bytesPerSecond, CheckedAt: v.at.Unix(), Error: v.lastError}})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}
func (s *Service) SaveStorageInstance(id, name, driver string, c InstanceConfig, enabled bool) (string, error) {
	if c.VideoPartMB == 0 {
		c.VideoPartMB = 256
	}
	s.storages.mu.Lock()
	defer s.storages.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 240 {
		return "", errors.New("存储名称不能为空且不能超过 80 个汉字")
	}
	if id != "" {
		old := s.storages.instances[id]
		if old == nil {
			return "", dbx.ErrNotFound
		}
		if old.row.Driver != driver {
			return "", errors.New("已有实例不能更换驱动，请新建实例")
		}

		c.Bootstrap, c.Managed = old.config.Bootstrap, true
		c.LocalLayout = old.config.LocalLayout
		if driver == "local" {
			// Legacy locators are server-owned compatibility metadata. Editing
			// the single visible folder or options must not redirect either path.
			c.LocalPaths = old.config.LocalPaths
			if c.Directory == s.localPath(old.config.Directory) {
				c.Directory = old.config.Directory
			}
			if c.ColdDirectory == s.localPath(old.config.ColdDirectory) {
				c.ColdDirectory = old.config.ColdDirectory
			}
			if old.config.LocalPaths != nil && c.Directory != old.config.Directory {
				return "", errors.New("旧布局存储沿用原位置；需要新的存储文件夹请新建存储，已有文件不会移动")
			}
		}
		if c.SecretKey == "" {
			c.SecretKey = old.config.SecretKey
		}
		if c.AccessKey == "" {
			c.AccessKey = old.config.AccessKey
		}
		if c.CDNToken == "" {
			c.CDNToken = old.config.CDNToken
		}
		if c.Bootstrap && driver == "local" && c.ColdDirectory == "" {
			c.ColdDirectory = old.config.ColdDirectory
		}
		moved := c.Directory != old.config.Directory || c.ColdDirectory != old.config.ColdDirectory || c.Endpoint != old.config.Endpoint || c.Bucket != old.config.Bucket || c.Prefix != old.config.Prefix || c.PublicBase != old.config.PublicBase || c.TempPublicBase != old.config.TempPublicBase || c.CleanupBucket != old.config.CleanupBucket || c.TopicID != old.config.TopicID
		if moved {
			var mediaRefs int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM media_replicas WHERE instance_id=?`, id).Scan(&mediaRefs); err != nil {
				return "", err
			}
			if mediaRefs > 0 {
				return "", errors.New("实例已有媒体副本，请新建实例后切换目录或位置")
			}
			var refs int
			if e := s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM archive_objects WHERE instance_id=?)+(SELECT COUNT(*) FROM archive_manifests WHERE instance_id=?)+(SELECT COUNT(*) FROM archive_manifest_jobs WHERE instance_id=?)+(SELECT COUNT(*) FROM archive_garbage WHERE instance_id=?)+(SELECT COUNT(*) FROM files WHERE cold_backend=? OR hot_backend=?)+(SELECT COUNT(*) FROM temp_objects WHERE (backend=? OR (?='r2-temp' AND backend='r2')) AND deleted_at IS NULL)+(SELECT COUNT(*) FROM import_rules WHERE instance_id=?)`, id, id, id, id, id, id, id, id, id).Scan(&refs); e != nil {
				return "", e
			}
			p := s.storages.policy
			selected := p.Permanent == id || p.Temporary == id
			for _, v := range p.Archives {
				selected = selected || v == id
			}
			if refs > 0 || selected {
				return "", errors.New("实例已用于存储、导入或当前策略，不能直接更改桶、目录、域名或话题；请新建实例后切换用途。名称和凭据仍可修改")
			}
		}

	}
	if id == "" {
		id = "s_" + ids.NewShort()
		c.Bootstrap = false
		c.Managed = true
		c.LocalPaths = nil
	}
	for _, old := range s.storages.instances {
		if old.row.ID == id {
			continue
		}
		if driver == "local" && old.row.Driver == "local" {
			locations := func(config InstanceConfig) []string {
				out := []string{config.Directory, config.ColdDirectory}
				if config.LocalPaths != nil {
					out = append(out, config.LocalPaths.Archives, config.LocalPaths.Display)
				}
				return out
			}
			for _, target := range locations(c) {
				for _, source := range locations(old.config) {
					if target != "" && source != "" && pathsOverlap(s.localPath(target), s.localPath(source)) {
						return "", errors.New("本地存储文件夹不能与已有存储位置重叠")
					}
				}
			}
		}
		if driver == "telegram" && old.row.Driver == "telegram" && c.TopicID > 0 && c.TopicID == old.config.TopicID {
			return "", errors.New("该输出话题已被其他实例使用")
		}
		if driver == "r2" && old.row.Driver == "r2" && (!old.config.Bootstrap || c.Bootstrap) && c.Endpoint == old.config.Endpoint && c.Bucket == old.config.Bucket && (pathOverlap(c.Prefix, old.config.Prefix)) {
			return "", errors.New("同一存储桶内的实例前缀不能重叠")
		}
		if driver == "r2" && old.row.Driver == "r2" && old.config.Bootstrap && strings.TrimRight(c.Endpoint, "/") == strings.TrimRight(old.config.Endpoint, "/") {
			var roots []string
			if c.Bucket == old.config.Bucket {
				if old.row.ID == defaultR2Temporary {
					roots = append(roots, "tmp")
				} else {
					roots = append(roots, "archives", "pimg", "thumbs")
				}
			}
			for _, root := range roots {
				if pathOverlap(c.Prefix, root) {
					return "", errors.New("实例前缀不能覆盖继承配置的 R2 归档或展示区域")
				}
			}
		}
	}
	if driver == "local" {
		c.LocalLayout = localFolderLayout
	}
	if e := s.validateInstanceConfig(driver, c); e != nil {
		return "", e
	}
	b, e := s.sealConfig(id, c)
	if e != nil {
		return "", e
	}
	row := dbx.StorageInstance{ID: id, Name: name, Driver: driver, Config: b, Enabled: enabled}
	factory := s.storages.factories[driver]
	if factory == nil {
		return "", errors.New("未注册的存储驱动")
	}
	cold, hot, local, source, e := factory(id, c)
	if e != nil {
		return "", e
	}
	if e = s.DB.SaveStorageInstance(row); e != nil {
		return "", e
	}
	s.storages.instances[id] = &instanceRuntime{row, c, cold, hot, local, source}
	return id, nil
}
func pathOverlap(a, b string) bool {
	a = strings.Trim(a, "/")
	b = strings.Trim(b, "/")
	return a == "" || b == "" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func (s *Service) SaveStoragePolicy(p StoragePolicy) error {
	// Older clients omit local_mbps. Keep the saved copy limit rather than
	// resetting it or treating their otherwise valid policy as an error.
	if p.LocalMB == 0 {
		p.LocalMB = s.StoragePolicy().LocalMB
		if p.LocalMB == 0 {
			p.LocalMB = 64
		}
	}
	p.Archives = append([]string(nil), p.Archives...)
	if len(p.Archives) == 0 {
		return errors.New("至少选择一个归档存储")
	}
	seen := map[string]bool{}
	for _, id := range p.Archives {
		if seen[id] {
			return errors.New("归档存储不能重复")
		}
		seen[id] = true
		i, e := s.instance(id)
		if e != nil {
			return e
		}
		if !i.row.Enabled || i.cold == nil {
			return errors.New("归档实例未启用或不支持归档")
		}
	}
	for _, id := range []string{p.Permanent, p.Temporary} {
		i, e := s.instance(id)
		if e != nil {
			return e
		}
		if !i.row.Enabled || i.hot == nil {
			return errors.New("发布和临时存储需要选择可用的本地或 R2 实例")
		}
	}
	if i, _ := s.instance(p.Temporary); i.row.ID == "r2" && i.config.Bootstrap {
		return errors.New("R2 永久存储不能用于新临时副本，请选择 R2 临时存储或其他独立实例")
	}
	publish, _ := s.instance(p.Permanent)
	if publish.hot.PublicURL(".capability") == "" {
		return errors.New("永久发布实例需要配置公开域名")
	}
	if p.LocalMB < 1 || p.LocalMB > 1024 {
		return errors.New("本地同步限速应为 1–1024 MiB/s")
	}
	if p.NormalMB < 1 || p.NormalMB > 1024 || p.AcceleratedMB < p.NormalMB || p.AcceleratedMB > 1024 || p.SyncCacheMB < 32 || p.SyncCacheMB > 16384 {
		return errors.New("同步速率应为 1–1024 MiB/s，加速速率不低于普通速率，工作缓存为 32–16384 MiB")
	}
	for _, v := range []string{p.Start, p.End} {
		if _, e := time.Parse("15:04", v); e != nil {
			return errors.New("时间段须使用 HH:mm（北京时间）")
		}
	}
	if e := s.DB.SetSetting("storage.policy", dbx.JSONValue(p)); e != nil {
		return e
	}
	s.storages.mu.Lock()
	s.storages.policy = p
	s.storages.mu.Unlock()
	s.dyn.mu.Lock()
	s.dyn.hotStorage = p.Permanent
	if len(p.Archives) > 0 {
		s.dyn.coldStorage = p.Archives[0]
	}
	s.dyn.mu.Unlock()
	s.InterruptIdle()
	return s.ReconcileAccountSpaces()
}
func (s *Service) rankedArchives() []string {
	p := s.StoragePolicy()
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	sort.SliceStable(p.Archives, func(a, b int) bool {
		return s.storageLessLocked(p.Archives[a], p.Archives[b])
	})
	return p.Archives
}
func (s *Service) storageLessLocked(a, b string) bool {
	x, y := s.storages.scores[a], s.storages.scores[b]
	xbad, ybad := x.available != nil && !*x.available, y.available != nil && !*y.available
	if xbad != ybad {
		return !xbad
	}
	var xp, yp int
	if i := s.storages.instances[a]; i != nil {
		xp = i.config.Priority
	}
	if i := s.storages.instances[b]; i != nil {
		yp = i.config.Priority
	}
	if xp != yp {
		return xp > yp
	}
	if x.bytesPerSecond > 0 && y.bytesPerSecond > 0 && x.bytesPerSecond != y.bytesPerSecond {
		return x.bytesPerSecond > y.bytesPerSecond
	}
	if x.latencyMS > 0 && y.latencyMS > 0 && x.latencyMS != y.latencyMS {
		return x.latencyMS < y.latencyMS
	}
	return false
}
func (s *Service) initialArchive() string {
	for _, id := range s.rankedArchives() {
		if i, e := s.instance(id); e == nil && i.row.Enabled && i.cold != nil {
			return id
		}
	}
	c, _ := s.StorageSelection()
	return c
}
func (s *Service) recordStorageResult(id string, n int64, start time.Time, err error) {
	// Shutdown, caller cancellation and canceled queue jobs say nothing about
	// provider availability. Keep the last completed observation intact.
	if errors.Is(err, context.Canceled) {
		return
	}
	s.storages.mu.Lock()
	driver := "storage"
	if instance := s.storages.instances[id]; instance != nil {
		driver = instance.row.Driver
	}
	v := s.storages.scores[id]
	available := err == nil
	v.available = &available
	v.lastError = ""
	if n == 0 {
		v.latencyMS = float64(time.Since(start).Microseconds()) / 1000
	}
	if err != nil {
		v.lastError = err.Error()
	}
	if time.Since(v.at) > 10*time.Minute {
		v.failures = 0
	}
	if err != nil {
		v.failures++
	} else {
		v.failures = 0
		if n > 0 {
			v.bytesPerSecond = float64(n) / time.Since(start).Seconds()
		}
	}
	v.at = time.Now()
	s.storages.scores[id] = v
	s.storages.mu.Unlock()
	if s.DB != nil {
		s.DB.Performance().ObserveResult("storage", driver, time.Since(start), n, err)
	}
	if s.DB != nil {
		_, _ = s.DB.Exec(`INSERT INTO storage_health(instance_id,available,latency_ms,bytes_per_second,checked_at,error) VALUES(?,?,?,?,?,?) ON CONFLICT(instance_id) DO UPDATE SET available=excluded.available,latency_ms=excluded.latency_ms,bytes_per_second=excluded.bytes_per_second,checked_at=excluded.checked_at,error=excluded.error`, id, available, v.latencyMS, v.bytesPerSecond, v.at.Unix(), v.lastError)
	}
}

type topicCold struct {
	s        *Service
	topic    int
	instance string
}

func (t topicCold) Put(ctx context.Context, key string, b []byte, caption string) (storage.Reference, error) {
	return t.PutReader(ctx, key, bytes.NewReader(b), int64(len(b)), caption)
}
func (t topicCold) PutReader(ctx context.Context, key string, r io.Reader, n int64, caption string) (storage.Reference, error) {
	if e := t.s.TGReady(); e != nil {
		return storage.Reference{}, e
	}
	_, fileID, e := archiveKeyIdentity(key)
	if e != nil {
		return storage.Reference{}, e
	}
	f, e := t.s.DB.GetFile(fileID)
	if e != nil {
		return storage.Reference{}, e
	}
	binding, e := t.s.telegramArchiveBinding(ctx, f)
	if e != nil {
		return storage.Reference{}, e
	}
	topic, chat := binding.TopicID, binding.ChatID
	h := sha256.New()
	r = io.TeeReader(r, h)
	var ref tguser.Reference
	random := stableTelegramRandomID(t.instance, key)
	wrapped := false
	mimeType := "application/octet-stream"
	var video *tguser.VideoMetadata
	var thumbnail []byte
	if fileID != "" {
		if f, err := t.s.DB.GetFile(fileID); err == nil {
			if upload, ok := ctx.Value(mediaUploadContext{}).(mediaArchiveUpload); ok && upload.File != nil {
				f = upload.File
				thumbnail = upload.Thumbnail
				if upload.Version != "" {
					random = stableTelegramRandomID(t.instance, key+"/cover:"+upload.Version)
				}
			}
			wrapped = f.Storage != "direct"
			if wrapped {
				mimeType = "image/png"
			} else {
				mimeType = f.Mime
				if f.Kind == "video" && mimeType == "video/mp4" {
					video = &tguser.VideoMetadata{Duration: f.Duration, Width: f.Width, Height: f.Height, SupportsStreaming: true}
				}
			}
		}
	}
	if stream, ok := t.s.UserTG.(tguser.StreamTransport); ok {
		ref, e = stream.UploadStream(ctx, chat, topic, path.Base(key), mimeType, r, n, thumbnail, "pimg-managed "+key, random, video)
	} else {
		var b []byte
		b, e = io.ReadAll(io.LimitReader(r, 32<<20))
		if e == nil && int64(len(b)) != n {
			e = errors.New("测试 TG 客户端仅支持 32 MiB 内对象")
		}
		if e == nil {
			ref, e = t.s.UserTG.Upload(ctx, chat, topic, path.Base(key), mimeType, b, thumbnail, "pimg-managed "+key, random)
		}
	}
	ref.Size = n
	if e == nil {
		if fileID != "" {
			e = t.s.recordManagedTelegram(ref, fileID, path.Base(key), mimeType)
		}
	}
	return storage.Reference{Key: encodeTelegramObject(telegramObject{Reference: ref, Carrier: wrapped, WireSHA256: hex.EncodeToString(h.Sum(nil))})}, e
}
func (t topicCold) Open(ctx context.Context, r storage.Reference) (io.ReadCloser, error) {
	return (userCold{s: t.s}).Open(ctx, r)
}
func (t topicCold) Delete(ctx context.Context, r storage.Reference) error {
	return (userCold{s: t.s}).Delete(ctx, r)
}
func (t topicCold) Check(ctx context.Context) error { return t.s.TGReady() }

func (s *Service) UsesStorageDriver(driver string) bool {
	for _, id := range s.StoragePolicy().Archives {
		if i, e := s.instance(id); e == nil && i.row.Driver == driver {
			return true
		}
	}
	return false
}

func (s *Service) selectInitialArchive(f *dbx.File) error {
	if f.ArchiveState == "done" {
		return nil
	}
	// Historical pending records keep their recorded physical representation.
	if f.MediaVersion != 2 {
		if i, e := s.instance(f.ColdBackend); e == nil && i.row.Enabled {
			return nil
		}
	}
	var count int
	if e := s.DB.QueryRow(`SELECT COUNT(*) FROM file_chunks WHERE file_id=?`, f.ID).Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		return nil
	}
	target := s.initialArchiveForOwner(f.OwnerID)
	if target == "" {
		return errors.New("账号归档空间尚未就绪")
	}
	if target == f.ColdBackend {
		return nil
	}
	if _, e := s.DB.Exec(`UPDATE files SET cold_backend=? WHERE id=?`, target, f.ID); e != nil {
		return e
	}
	f.ColdBackend = target
	return nil
}
