package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitLocalStorageWithoutCloudCredentials(t *testing.T) {
	c := defaults()
	c.Storage.Cold, c.Storage.Hot = "local", "local"
	c.Paths.DataDir = t.TempDir()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Storage.Cold != "local" || c.Storage.Hot != "local" {
		t.Fatal("explicit local selection was replaced by remote defaults")
	}
	if c.Storage.LocalColdDir != filepath.Join(c.Paths.DataDir, "cold") || c.Storage.LocalHotDir != filepath.Join(c.Paths.DataDir, "hot") {
		t.Fatal("wrong local directories")
	}
	a, err := c.ResolveMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.ResolveMasterKey()
	if err != nil || len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatal("master key did not persist", err)
	}
	if c.ValidateCold("telegram") != nil || c.ValidateHot("r2") == nil {
		t.Fatal("Telegram must allow web onboarding; R2 still requires configuration")
	}
	if c.ValidateCold("bogus") == nil || c.ValidateHot("bogus") == nil {
		t.Fatal("unknown provider accepted")
	}
}
func TestPreserveExistingDevelopmentKey(t *testing.T) {
	c := defaults()
	c.Storage.Cold, c.Storage.Hot = "local", "local"
	c.Paths.DataDir = t.TempDir()
	oldPath := filepath.Join(c.Paths.DataDir, "dev-master.key")
	if err := os.WriteFile(oldPath, []byte("existing-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.MasterKeyFile != oldPath {
		t.Fatal("switching from mock must preserve existing key path")
	}
}
func TestStorageEnvironmentOverrides(t *testing.T) {
	t.Setenv("PIMG_COLD_STORAGE", "local")
	t.Setenv("PIMG_HOT_STORAGE", "local")
	t.Setenv("PIMG_LOCAL_COLD_DIR", "archive-dir")
	t.Setenv("PIMG_LOCAL_HOT_DIR", "serving-dir")
	c := defaults()
	applyEnv(c)
	if c.Storage.Cold != "local" || c.Storage.Hot != "local" || c.Storage.LocalColdDir != "archive-dir" || c.Storage.LocalHotDir != "serving-dir" {
		t.Fatal("storage environment ignored")
	}
}

func TestStorageProxyConfigAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  cold: local\n  hot: local\n  proxy_url: http://yaml-proxy:7890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIMG_STORAGE_PROXY_URL", "")
	c, err := Load(path)
	if err != nil || c.Storage.ProxyURL != "http://yaml-proxy:7890" {
		t.Fatal("YAML storage proxy not loaded", err)
	}
	t.Setenv("PIMG_STORAGE_PROXY_URL", "socks5://env-proxy:1080")
	c, err = Load(path)
	if err != nil || c.Storage.ProxyURL != "socks5://env-proxy:1080" {
		t.Fatal("environment did not override YAML storage proxy", err)
	}
	t.Setenv("PIMG_STORAGE_PROXY_URL", "vless://fixture-secret@proxy:443")
	if _, err := Load(path); err == nil {
		t.Fatal("unsupported proxy must fail before runtime initialization")
	}
}

func TestRemoteStorageDefaultsAllowWebOnboarding(t *testing.T) {
	for _, emptyRoles := range []bool{false, true} {
		name := "default-config"
		if emptyRoles {
			name = "omitted-storage-roles"
		}
		t.Run(name, func(t *testing.T) {
			c := defaults()
			c.Paths.DataDir = t.TempDir()
			if emptyRoles {
				c.Storage = Storage{}
			}
			if err := c.normalize(); err != nil {
				t.Fatal("web onboarding blocked", err)
			}
			if c.Storage.Cold != "telegram" || c.Storage.Hot != "r2" {
				t.Fatal("missing credentials silently changed the selected providers")
			}
			c.Telegram.BotToken, c.Telegram.ChannelID = "fixture-token", "fixture-channel"
			if err := c.normalize(); err != nil {
				t.Fatal("web onboarding blocked", err)
			}
			c.R2.AccountID = "fixture-account"
			c.R2.AccessKeyID, c.R2.SecretAccessKey = "fixture-key", "fixture-secret"
			c.R2.TempBucket, c.R2.PermBucket = "fixture-temp", "fixture-permanent"
			c.R2.PublicBaseURL = "https://images.example.invalid/"
			if err := c.normalize(); err != nil {
				t.Fatal("configured default providers were rejected", err)
			}
			if c.Storage.Cold != "telegram" || c.Storage.Hot != "r2" || c.R2.Endpoint != "https://fixture-account.r2.cloudflarestorage.com" || c.R2.PublicBaseURL != "https://images.example.invalid" {
				t.Fatal("remote defaults were not wired to their provider configuration")
			}
		})
	}
	c := defaults()
	c.Storage = Storage{}
	c.Storage.Hot = "local"
	c.Telegram.ChunkMB = 128
	if err := c.normalize(); err != nil {
		t.Fatal("user transport incorrectly used a Bot chunk limit", err)
	}
	if c.Telegram.SessionFile != filepath.Join(c.Paths.DataDir, "telegram", "session.enc") {
		t.Fatal("session path must follow data directory")
	}
	c.DevMock = true
	if err := c.normalize(); err == nil {
		t.Fatal("test Bot chunk limit missing")
	}
	if c.Telegram.APIID != 0 || c.Telegram.APIHash != "" {
		t.Fatal("onboarding populated user secrets")
	}
	if c.ValidateCold("telegram") != nil {
		t.Fatal("onboarding was blocked")
	}
}

func TestRelativeStorageDirectoriesPreserveExplicitPaths(t *testing.T) {
	c := defaults()
	c.Storage.Cold, c.Storage.Hot = "local", "local"
	c.Paths.DataDir = "./data-dev"
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(c.Storage.LocalColdDir) || filepath.IsAbs(c.Storage.LocalHotDir) || c.Storage.LocalColdDir != filepath.Join("./data-dev", "cold") || c.Storage.LocalHotDir != filepath.Join("./data-dev", "hot") {
		t.Fatal("relative data directory was converted to an absolute storage path")
	}
	c = defaults()
	c.Storage.Cold, c.Storage.Hot = "local", "local"
	c.Paths.DataDir = "./data-dev"
	coldPath, hotPath := filepath.Join(t.TempDir(), "archive"), filepath.Join(t.TempDir(), "serving")
	c.Storage.LocalColdDir, c.Storage.LocalHotDir = coldPath, hotPath
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Storage.LocalColdDir != coldPath || c.Storage.LocalHotDir != hotPath {
		t.Fatal("explicit absolute storage paths were rewritten")
	}
}

func TestPublicTempConfiguration(t *testing.T) {
	base := R2{TempBucket: "temp", PermBucket: "perm", TempPublicBaseURL: "https://temp.example.com", TempCleanupBucket: "control", CDNZoneID: "zone", CDNAPIToken: "token"}
	for _, name := range []string{"valid", "http", "path", "query", "empty-query", "credentials", "missing-control", "shared-control", "permanent-control", "missing-token", "missing-zone"} {
		t.Run(name, func(t *testing.T) {
			c := defaults()
			c.R2 = base
			switch name {
			case "http":
				c.R2.TempPublicBaseURL = "http://temp.example.com"
			case "path":
				c.R2.TempPublicBaseURL += "/files"
			case "query":
				c.R2.TempPublicBaseURL += "?secret=x"
			case "empty-query":
				c.R2.TempPublicBaseURL += "?"
			case "credentials":
				c.R2.TempPublicBaseURL = "https://user:password@temp.example.com"
			case "missing-control":
				c.R2.TempCleanupBucket = ""
			case "shared-control":
				c.R2.TempCleanupBucket = "temp"
			case "permanent-control":
				c.R2.TempCleanupBucket = "perm"
			case "missing-token":
				c.R2.CDNAPIToken = ""
			case "missing-zone":
				c.R2.CDNZoneID = ""
			}
			err := c.ValidatePublicTemp()
			if (err == nil) != (name == "valid" || strings.HasSuffix(name, "control") || name == "missing-token" || name == "missing-zone") {
				t.Fatal("wrong validation result", err)
			}
		})
	}
	t.Setenv("PIMG_R2_TEMP_PUBLIC_BASE_URL", base.TempPublicBaseURL)
	t.Setenv("PIMG_R2_TEMP_CLEANUP_BUCKET", "control")
	t.Setenv("PIMG_R2_TEMP_CACHE_SECONDS", "300")
	t.Setenv("PIMG_R2_TEMP_CDN_ZONE_ID", "temp-zone")
	c := defaults()
	applyEnv(c)
	if c.R2.TempPublicBaseURL != base.TempPublicBaseURL || c.R2.TempCleanupBucket != "control" || c.R2.TempCacheSeconds != 300 || c.R2.TempCDNZoneID != "temp-zone" {
		t.Fatal("public temporary environment ignored")
	}
}
