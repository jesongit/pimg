package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredWebPolicyInputsCannotOverrideWebsite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PIMG_DATA_DIR", root)
	for key, value := range map[string]string{"PIMG_UPLOAD_MAX_MB": "1", "PIMG_UPLOAD_EXTS": "png", "PIMG_PUBLIC_BASE_URL": "https://ignored.invalid", "PIMG_RETENTION_HOURS": "1", "PIMG_R2_PRESIGN_MINUTES": "1"} {
		t.Setenv(key, value)
	}
	p := filepath.Join(root, "config.yaml")
	if e := os.WriteFile(p, []byte("server:\n  public_base_url: https://ignored.invalid\nupload:\n  max_size_mb: 1\n  allowed_exts: [png]\nstorage:\n  retention_hours: 1\n  presign_minutes: 1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if c.Upload.MaxSizeMB != 0 || c.Server.PublicBaseURL != "" || len(c.Upload.AllowedExts) < 10 || c.RetentionHours() != 24 || c.PresignMinutes() != 30 {
		t.Fatal("retired policy remained an override")
	}
	body, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"MaxSizeMB", "AllowedExts", "RetentionHours", "PresignMinutes"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Fatal("retired web policy persisted in startup", key)
		}
	}
}
