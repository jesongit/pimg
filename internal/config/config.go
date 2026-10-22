// Package config 加载并校验 pimg 的运行配置（config.yaml + 环境变量覆盖）。
package config

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pimg/internal/storagehttp"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Listen        string `yaml:"listen"`
	PublicBaseURL string `yaml:"-" json:"-"` // Web-managed; retained only for source compatibility.
	CookieSecure  *bool  `yaml:"cookie_secure"`
}

type Admin struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Telegram struct {
	APIID       int    `yaml:"api_id"`
	APIHash     string `yaml:"api_hash"`
	ChatID      int64  `yaml:"chat_id"`
	SessionFile string `yaml:"session_file"`
	// Only the explicitly injected development mock uses the old test transport.
	BotToken  string `yaml:"-"`
	ChannelID string `yaml:"-"`
	APIBase   string `yaml:"-"`
	ChunkMB   int    `yaml:"-"`
}

type R2 struct {
	AccountID         string `yaml:"account_id"`
	Endpoint          string `yaml:"endpoint"`
	AccessKeyID       string `yaml:"access_key_id"`
	SecretAccessKey   string `yaml:"secret_access_key"`
	TempBucket        string `yaml:"temp_bucket"`
	PermBucket        string `yaml:"perm_bucket"`
	PublicBaseURL     string `yaml:"public_base_url"`
	TempPublicBaseURL string `yaml:"temp_public_base_url"`
	TempCleanupBucket string `yaml:"temp_cleanup_bucket"`
	TempCacheSeconds  int    `yaml:"temp_cache_seconds"`
	TempCDNZoneID     string `yaml:"temp_cdn_zone_id"`
	RetentionHours    int    `yaml:"-" json:"-"`
	PresignMinutes    int    `yaml:"-" json:"-"`
	CDNZoneID         string `yaml:"cdn_zone_id"`
	CDNAPIToken       string `yaml:"cdn_api_token"`
}

type Upload struct {
	MaxSizeMB   int      `yaml:"-" json:"-"`
	AllowedExts []string `yaml:"-" json:"-"`
	MaxPixels   int64    `yaml:"max_pixels"`
}

type Paths struct {
	DatabaseFile string   `yaml:"-"`
	LegacyDir    string   `yaml:"-"`
	InputDir     string   `yaml:"-"`
	LegacyRoots  []string `yaml:"-"`
	DataDir      string   `yaml:"data_dir"`
	RuntimeDir   string   `yaml:"runtime_dir"`
	TempDir      string   `yaml:"temp_dir"`
	ImportDir    string   `yaml:"import_dir"`
}

// RuntimeRoot also supports isolated services constructed directly by tests or tools.
func (p Paths) RuntimeRoot() string {
	if p.RuntimeDir != "" {
		return p.RuntimeDir
	}
	return p.DataDir
}

// Storage keeps the two independent storage roles separate from provider credentials.
type Storage struct {
	Cold           string `yaml:"cold"`
	Hot            string `yaml:"hot"`
	ProxyURL       string `yaml:"proxy_url"`
	LocalColdDir   string `yaml:"local_cold_dir"`
	LocalHotDir    string `yaml:"local_hot_dir"`
	RetentionHours int    `yaml:"-" json:"-"`
	PresignMinutes int    `yaml:"-" json:"-"`
}

type Config struct {
	StartupVersion int         `yaml:"-" json:"startup_version"`
	Server         Server      `yaml:"server"`
	Diagnostics    Diagnostics `yaml:"diagnostics" json:"-"`
	MasterKey      string      `yaml:"master_key"`
	MasterKeyFile  string      `yaml:"master_key_file"`
	Admin          Admin       `yaml:"admin"`
	Telegram       Telegram    `yaml:"telegram"`
	R2             R2          `yaml:"r2"`
	Upload         Upload      `yaml:"upload"`
	Paths          Paths       `yaml:"paths"`
	Storage        Storage     `yaml:"storage"`
	Workers        int         `yaml:"workers"`
	DevMock        bool        `yaml:"dev_mock"`
}

func defaults() *Config {
	return &Config{
		StartupVersion: CurrentStartupVersion,
		Server:         Server{Listen: "0.0.0.0:8787"},
		Admin:          Admin{Username: "admin"},
		Telegram: Telegram{
			APIBase: "https://api.telegram.org",
			ChunkMB: 16,
		},
		R2: R2{
			RetentionHours: 24,
			PresignMinutes: 30,
		},
		Upload: Upload{
			MaxSizeMB: 0,
			AllowedExts: []string{
				"jpg", "jpeg", "png", "gif", "webp", "avif", "bmp", "heic", "heif",
				"mp4", "m4v", "webm", "mov", "mkv", "ts", "m2ts", "mts", "avi", "flv",
			},
			MaxPixels: 40_000_000,
		},
		Paths:   Paths{DataDir: "./data", RuntimeDir: "."},
		Storage: Storage{Cold: "telegram", Hot: "r2"},
		Workers: 2,
	}
}

// Load 读取配置文件（若存在）并应用环境变量覆盖，最后做一致性校验。
func Load(path string) (*Config, error) {
	c := defaults()
	var source []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件: %w", err)
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("解析配置文件: %w", err)
		}
		source = b
	}
	applyEnv(c)
	// Locate by this machine's deployment paths before restoring portable settings.
	dataDir, runtimeDir, listen, secure := c.Paths.DataDir, c.Paths.RuntimeDir, c.Server.Listen, c.Server.CookieSecure
	if b, err := os.ReadFile(filepath.Join(dataDir, "system", "startup.json")); err == nil {
		var saved Config
		if err = json.Unmarshal(b, &saved); err != nil {
			return nil, fmt.Errorf("读取已保存启动配置：%w", err)
		}
		if saved.StartupVersion < 0 || saved.StartupVersion > CurrentStartupVersion {
			return nil, fmt.Errorf("启动配置版本 %d 不受当前程序支持", saved.StartupVersion)
		}
		if saved.StartupVersion == 0 && isLegacyDefaultExtensions(saved.Upload.AllowedExts) {
			saved.Upload.AllowedExts = defaults().Upload.AllowedExts
		}
		oldData, oldRuntime := saved.Paths.DataDir, saved.Paths.RuntimeRoot()
		move := func(p, oldRoot, newRoot string) string {
			a, b := strings.ReplaceAll(p, "\\", "/"), strings.TrimRight(strings.ReplaceAll(oldRoot, "\\", "/"), "/")
			if p != "" && b != "" {
				if strings.EqualFold(a, b) {
					return newRoot
				}
				if len(a) > len(b) && strings.EqualFold(a[:len(b)], b) && a[len(b)] == '/' {
					return filepath.Join(newRoot, filepath.FromSlash(a[len(b)+1:]))
				}
			}
			return p
		}
		saved.MasterKeyFile = move(saved.MasterKeyFile, oldData, dataDir)
		saved.Telegram.SessionFile = move(saved.Telegram.SessionFile, oldData, dataDir)
		saved.Storage.LocalColdDir = move(saved.Storage.LocalColdDir, oldData, dataDir)
		saved.Storage.LocalHotDir = move(saved.Storage.LocalHotDir, oldData, dataDir)
		saved.Paths.TempDir = move(saved.Paths.TempDir, oldRuntime, runtimeDir)
		saved.Paths.DataDir = dataDir
		saved.Paths.RuntimeDir = runtimeDir
		saved.Server.Listen = listen
		if secure != nil {
			saved.Server.CookieSecure = secure
		}
		c = &saved
		if len(source) > 0 {
			if err = yaml.Unmarshal(source, c); err != nil {
				return nil, err
			}
		}
		applyEnv(c)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// SaveStartup makes the data directory self-contained without copying deployment
// ports or runtime directories back over this machine on the next start.
func SaveStartup(c *Config, master []byte) error {
	saved := *c
	saved.StartupVersion = CurrentStartupVersion
	saved.MasterKey = ""
	saved.Admin.Password = ""
	saved.Paths.DatabaseFile = ""
	saved.Paths.LegacyDir = ""
	saved.Paths.InputDir = ""
	saved.Paths.LegacyRoots = nil
	root, err := filepath.Abs(c.Paths.DataDir)
	if err != nil {
		return err
	}
	keyPath := c.MasterKeyFile
	r, e := filepath.Rel(root, keyPath)
	if keyPath == "" || e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		keyPath = filepath.Join(root, "system/keys/master.key")
	}
	if raw, e := os.ReadFile(keyPath); e == nil {
		key, e := parseKey(strings.TrimSpace(string(raw)))
		if e != nil || !bytes.Equal(key, master) {
			return errors.New("保存启动配置时主密钥冲突")
		}
	} else if errors.Is(e, os.ErrNotExist) {
		if e = os.MkdirAll(filepath.Dir(keyPath), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(master)+"\n"), 0600); e != nil {
			return e
		}
	} else {
		return e
	}
	saved.MasterKeyFile = keyPath
	b, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(root, "system/startup.json")
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(p+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (c *Config) normalize() error {
	if _, err := ParseIPRanges(c.Diagnostics.AllowIPs); err != nil {
		return fmt.Errorf("diagnostics.allow_ips: %w", err)
	}
	if _, err := ParseIPRanges(c.Diagnostics.TrustedProxies); err != nil {
		return fmt.Errorf("diagnostics.trusted_proxies: %w", err)
	}
	c.Storage.ProxyURL = strings.TrimSpace(c.Storage.ProxyURL)
	if _, err := storagehttp.ParseProxyURL(c.Storage.ProxyURL); err != nil {
		return err
	}
	if c.Storage.Cold == "" {
		c.Storage.Cold = "telegram"
	}
	if c.Storage.Hot == "" {
		c.Storage.Hot = "r2"
	}
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:8787"
	}
	c.Server.PublicBaseURL = strings.TrimRight(c.Server.PublicBaseURL, "/")
	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
	c.Telegram.APIBase = strings.TrimRight(c.Telegram.APIBase, "/")
	if c.Telegram.ChunkMB <= 0 {
		c.Telegram.ChunkMB = 16
	}
	if c.DevMock && c.Telegram.ChunkMB > 20 {
		return errors.New("模拟 TG 传输的 chunk_mb 不能超过 20")
	}
	if c.R2.RetentionHours <= 0 {
		c.R2.RetentionHours = 24
	}
	if c.R2.PresignMinutes <= 0 {
		c.R2.PresignMinutes = 30
	}
	if len(c.Upload.AllowedExts) == 0 {
		c.Upload.AllowedExts = defaults().Upload.AllowedExts
	}
	if c.Paths.DataDir == "" {
		c.Paths.DataDir = "./data"
	}
	if c.Paths.TempDir == "" {
		c.Paths.TempDir = filepath.Join(c.Paths.RuntimeRoot(), "tmp")
	}
	if c.Telegram.SessionFile == "" {
		c.Telegram.SessionFile = filepath.Join(c.Paths.DataDir, "telegram", "session.enc")
	}
	if c.Storage.LocalColdDir == "" {
		c.Storage.LocalColdDir = filepath.Join(c.Paths.DataDir, "cold")
	}
	if c.Storage.LocalHotDir == "" {
		c.Storage.LocalHotDir = filepath.Join(c.Paths.DataDir, "hot")
	}
	c.Storage.RetentionHours = c.RetentionHours()
	c.Storage.PresignMinutes = c.PresignMinutes()
	if c.Storage.Cold != "local" && c.Storage.Cold != "telegram" {
		return errors.New("storage.cold 只支持 local|telegram")
	}
	if c.Storage.Hot != "local" && c.Storage.Hot != "r2" {
		return errors.New("storage.hot 只支持 local|r2")
	}
	if c.MasterKey == "" && c.MasterKeyFile == "" {
		c.MasterKeyFile = filepath.Join(c.Paths.DataDir, "keys", "master.key")
		// Preserve the key used by existing development databases.
		if _, err := os.Stat(filepath.Join(c.Paths.DataDir, "dev-master.key")); err == nil {
			c.MasterKeyFile = filepath.Join(c.Paths.DataDir, "dev-master.key")
		}
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	c.R2.PublicBaseURL = strings.TrimRight(c.R2.PublicBaseURL, "/")
	c.R2.TempPublicBaseURL = strings.TrimRight(c.R2.TempPublicBaseURL, "/")
	if c.R2.TempCacheSeconds <= 0 {
		c.R2.TempCacheSeconds = 3600
	}
	if c.R2.TempPublicBaseURL != "" {
		if err := c.ValidatePublicTemp(); err != nil {
			return err
		}
	}

	if c.R2.Endpoint == "" && c.R2.AccountID != "" {
		c.R2.Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", c.R2.AccountID)
	}
	return nil
}

// RetentionHours and PresignMinutes accept legacy R2 policy for existing configs.
func (c *Config) RetentionHours() int {
	if c.Storage.RetentionHours > 0 {
		return c.Storage.RetentionHours
	}
	if c.R2.RetentionHours > 0 {
		return c.R2.RetentionHours
	}
	return 24
}
func (c *Config) PresignMinutes() int {
	if c.Storage.PresignMinutes > 0 {
		return c.Storage.PresignMinutes
	}
	if c.R2.PresignMinutes > 0 {
		return c.R2.PresignMinutes
	}
	return 30
}

func (c *Config) ValidateCold(provider string) error {
	switch provider {
	case "local":
		return nil
	case "telegram":
		// Credentials and the persistent user session can be configured in the
		// admin UI after startup. Readiness is checked before archive operations.
		return nil
	default:
		return fmt.Errorf("未知冷存储方案 %q", provider)
	}
}

func (c *Config) ValidateHot(provider string) error {
	switch provider {
	case "local":
		return nil
	case "r2":
		if (c.R2.AccountID == "" && c.R2.Endpoint == "") || c.R2.AccessKeyID == "" || c.R2.SecretAccessKey == "" || c.R2.TempBucket == "" || c.R2.PermBucket == "" || c.R2.PublicBaseURL == "" {
			return errors.New("请先完整配置 R2 端点、凭据、临时桶、永久桶和公开域名，再选择 R2 热存储")
		}
		if c.R2.TempBucket == c.R2.PermBucket {
			return errors.New("R2 临时桶与永久桶必须不同")
		}
		if c.R2.TempPublicBaseURL != "" {
			return c.ValidatePublicTemp()
		}
		return nil
	default:
		return fmt.Errorf("未知热存储方案 %q", provider)
	}
}

func (c *Config) ValidatePublicTemp() error {
	u, err := url.Parse(c.R2.TempPublicBaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("r2.temp_public_base_url 必须是完整的 HTTPS 域名，不含路径、认证或查询参数")
	}

	return nil
}

// ResolveMasterKey 返回 32 字节主密钥。优先级：内联配置 > 密钥文件（不存在时可自动生成，仅当路径位于数据目录内）。
func (c *Config) ResolveMasterKey() ([]byte, error) {
	if c.MasterKey != "" {
		return parseKey(c.MasterKey)
	}
	if c.MasterKeyFile == "" {
		return nil, errors.New("未配置主密钥")
	}
	b, err := os.ReadFile(c.MasterKeyFile)
	if err == nil {
		return parseKey(strings.TrimSpace(string(b)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取主密钥文件: %w", err)
	}
	// 文件不存在：仅当密钥文件位于数据目录内才允许自动生成，避免悄悄在任意路径落盘。
	dataAbs, err1 := filepath.Abs(c.Paths.DataDir)
	keyAbs, err2 := filepath.Abs(filepath.Dir(c.MasterKeyFile))
	if err1 != nil || err2 != nil || !strings.HasPrefix(filepath.Clean(keyAbs)+string(os.PathSeparator), filepath.Clean(dataAbs)+string(os.PathSeparator)) {
		return nil, fmt.Errorf("主密钥文件 %s 不存在；出于安全考虑只在数据目录内自动生成，请手动执行 `pimg genkey` 并妥善保存", c.MasterKeyFile)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(c.MasterKeyFile), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.MasterKeyFile, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("写入主密钥文件: %w", err)
	}
	return key, nil
}

func parseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("主密钥格式错误：需要 32 字节，base64 或 hex 编码（用 `pimg genkey` 生成）")
}

// ParseKeyString 供 CLI 使用。
func ParseKeyString(s string) ([]byte, error) { return parseKey(s) }

func applyEnv(c *Config) {
	set := func(dst *string, env string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*dst = v
		}
	}
	setInt := func(dst *int, env string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	set(&c.Server.Listen, "PIMG_LISTEN")
	// An explicitly empty environment value also clears a YAML allowlist.
	if v, ok := os.LookupEnv("PIMG_DIAGNOSTICS_ALLOW_IPS"); ok {
		c.Diagnostics.AllowIPs = strings.Split(v, ",")
	}
	if v, ok := os.LookupEnv("PIMG_DIAGNOSTICS_TRUSTED_PROXIES"); ok {
		c.Diagnostics.TrustedProxies = strings.Split(v, ",")
	}
	set(&c.MasterKey, "PIMG_MASTER_KEY")
	set(&c.MasterKeyFile, "PIMG_MASTER_KEY_FILE")
	set(&c.Admin.Username, "PIMG_ADMIN_USERNAME")
	set(&c.Admin.Password, "PIMG_ADMIN_PASSWORD")
	setInt(&c.Telegram.APIID, "PIMG_TG_API_ID")
	set(&c.Telegram.APIHash, "PIMG_TG_API_HASH")
	set(&c.Telegram.SessionFile, "PIMG_TG_SESSION_FILE")
	if raw := os.Getenv("PIMG_TG_CHAT_ID"); raw != "" {
		c.Telegram.ChatID, _ = strconv.ParseInt(raw, 10, 64)
	}
	set(&c.R2.AccountID, "PIMG_R2_ACCOUNT_ID")
	set(&c.R2.Endpoint, "PIMG_R2_ENDPOINT")
	set(&c.R2.AccessKeyID, "PIMG_R2_ACCESS_KEY_ID")
	set(&c.R2.SecretAccessKey, "PIMG_R2_SECRET_ACCESS_KEY")
	set(&c.R2.TempBucket, "PIMG_R2_TEMP_BUCKET")
	set(&c.R2.PermBucket, "PIMG_R2_PERM_BUCKET")
	set(&c.R2.PublicBaseURL, "PIMG_R2_PUBLIC_BASE_URL")
	set(&c.R2.TempPublicBaseURL, "PIMG_R2_TEMP_PUBLIC_BASE_URL")
	set(&c.R2.TempCleanupBucket, "PIMG_R2_TEMP_CLEANUP_BUCKET")
	setInt(&c.R2.TempCacheSeconds, "PIMG_R2_TEMP_CACHE_SECONDS")
	set(&c.R2.TempCDNZoneID, "PIMG_R2_TEMP_CDN_ZONE_ID")
	set(&c.R2.CDNZoneID, "PIMG_CF_ZONE_ID")
	set(&c.R2.CDNAPIToken, "PIMG_CF_API_TOKEN")
	set(&c.Paths.DataDir, "PIMG_DATA_DIR")
	set(&c.Paths.RuntimeDir, "PIMG_RUNTIME_DIR")
	set(&c.Paths.ImportDir, "PIMG_IMPORT_DIR")
	set(&c.Storage.Cold, "PIMG_COLD_STORAGE")
	set(&c.Storage.Hot, "PIMG_HOT_STORAGE")
	set(&c.Storage.ProxyURL, "PIMG_STORAGE_PROXY_URL")
	set(&c.Storage.LocalColdDir, "PIMG_LOCAL_COLD_DIR")
	set(&c.Storage.LocalHotDir, "PIMG_LOCAL_HOT_DIR")
	setInt(&c.Workers, "PIMG_WORKERS")
	if v, ok := os.LookupEnv("PIMG_DEV_MOCK"); ok && v != "" {
		c.DevMock = v == "1" || strings.EqualFold(v, "true")
	}
}

// DefaultAllowedExts is the product format set; web settings may restrict it.
func DefaultAllowedExts() []string { return append([]string(nil), defaults().Upload.AllowedExts...) }

// ConstantTimeEquals 恒定时间字符串比较（用于密钥类比较）。
func ConstantTimeEquals(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
