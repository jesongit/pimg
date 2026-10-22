package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStartupNoLongerRestoresRetiredWebPolicy(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "custom"}[custom], func(t *testing.T) {
			root := t.TempDir()
			saved := defaults()
			saved.StartupVersion = 0
			saved.Paths.DataDir = root
			saved.Upload.AllowedExts = strings.Fields("jpg jpeg png gif webp avif bmp mp4 m4v webm mov mkv")
			if custom {
				saved.Upload.AllowedExts = []string{"png", "jpeg"}
			}
			b, _ := json.Marshal(saved)
			var legacy map[string]any
			json.Unmarshal(b, &legacy)
			legacy["Upload"] = map[string]any{"MaxSizeMB": 512, "AllowedExts": saved.Upload.AllowedExts}
			b, _ = json.Marshal(legacy)
			os.MkdirAll(filepath.Join(root, "system"), 0700)
			os.WriteFile(filepath.Join(root, "system/startup.json"), b, 0600)
			t.Setenv("PIMG_DATA_DIR", root)
			t.Setenv("PIMG_COLD_STORAGE", "local")
			t.Setenv("PIMG_HOT_STORAGE", "local")
			loaded, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(loaded.Upload.AllowedExts, "heic") || loaded.Upload.MaxSizeMB != 0 {
				t.Fatal("retired startup policy replaced web defaults")
			}
			// Load stays read-only; backup must still see the original version/file.
			after, _ := os.ReadFile(filepath.Join(root, "system/startup.json"))
			if string(after) != string(b) {
				t.Fatal("load rewrote startup before backup")
			}
		})
	}
}

func TestStartupVersionRejectsFutureConfiguration(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "system"), 0700)
	os.WriteFile(filepath.Join(root, "system/startup.json"), []byte(`{"startup_version":9999}`), 0600)
	t.Setenv("PIMG_DATA_DIR", root)
	if _, err := Load(""); err == nil {
		t.Fatal("future startup configuration accepted")
	}
}
