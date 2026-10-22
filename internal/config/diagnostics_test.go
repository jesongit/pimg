package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticsNetworkTrustIsDeploymentLocal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("paths:\n  data_dir: "+filepath.ToSlash(dir)+"\ndiagnostics:\n  allow_ips: [192.168.2.0/24, '::1']\n  trusted_proxies: [10.1.0.2]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.ResolveMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveStartup(c, key); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "system/startup.json"))
	if strings.Contains(string(raw), "diagnostics") || strings.Contains(string(raw), "192.168.2") {
		t.Fatal("trust migrated", string(raw))
	}
	c, err = Load(p)
	if err != nil || len(c.Diagnostics.AllowIPs) != 2 {
		t.Fatal("YAML trust not reapplied", c, err)
	}
	t.Setenv("PIMG_DIAGNOSTICS_ALLOW_IPS", "")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ips, err := ParseIPRanges(c.Diagnostics.AllowIPs)
	if err != nil || len(ips) != 0 {
		t.Fatal("empty env did not clear trust")
	}
	for _, ip := range []string{"192.168.*", "localhost", "https://example.test", "10.0.0.1/99"} {
		if _, err := ParseIPRanges([]string{ip}); err == nil {
			t.Fatal("invalid allowlist accepted", ip)
		}
	}
}
