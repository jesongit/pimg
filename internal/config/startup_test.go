package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPortableStartupKeepsSettingsAndTargetDirectories(t *testing.T) {
	root := t.TempDir()
	source := defaults()
	source.Paths = Paths{DataDir: filepath.Join(root, "old/data"), RuntimeDir: filepath.Join(root, "old/runtime")}
	source.Paths.TempDir = filepath.Join(source.Paths.RuntimeDir, "tmp")
	source.MasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	source.Telegram.APIID = 123
	source.Telegram.APIHash = "kept-api-hash"
	source.Telegram.SessionFile = filepath.Join(source.Paths.DataDir, "system/telegram/session.enc")
	source.Storage.LocalColdDir = filepath.Join(source.Paths.DataDir, "storage/local/archives")
	source.Storage.LocalHotDir = filepath.Join(source.Paths.DataDir, "storage/local/display")
	source.Server.Listen = "127.0.0.1:9991"
	source.Admin.Password = "never-store-bootstrap-password"
	if err := SaveStartup(source, bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(source.Paths.DataDir, "system/startup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(source.MasterKey)) || bytes.Contains(b, []byte(source.Admin.Password)) {
		t.Fatal("startup snapshot leaked inline master/bootstrap password")
	}
	target := filepath.Join(root, "new/data")
	runtime := filepath.Join(root, "new/runtime")
	// Carry only the portable system subtree, then start without the old config.
	for _, name := range []string{"startup.json", "keys/master.key"} {
		raw, e := os.ReadFile(filepath.Join(source.Paths.DataDir, "system", name))
		if e != nil {
			t.Fatal(e)
		}
		p := filepath.Join(target, "system", name)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, raw, 0600)
	}
	t.Setenv("PIMG_DATA_DIR", target)
	t.Setenv("PIMG_RUNTIME_DIR", runtime)
	t.Setenv("PIMG_LISTEN", "127.0.0.1:9992")
	loaded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Telegram.APIID != 123 || loaded.Telegram.APIHash != "kept-api-hash" {
		t.Fatal("connection settings missing")
	}
	if loaded.Paths.DataDir != target || loaded.Paths.RuntimeDir != runtime || loaded.Server.Listen != "127.0.0.1:9992" {
		t.Fatal("source deployment paths restored")
	}
	if loaded.Telegram.SessionFile != filepath.Join(target, "system/telegram/session.enc") || loaded.Paths.TempDir != filepath.Join(runtime, "tmp") {
		t.Fatal("nonportable paths", loaded)
	}
	key, err := loaded.ResolveMasterKey()
	if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{7}, 32)) {
		t.Fatal("master changed", err)
	}
	// Explicit config and environment remain authoritative over saved defaults.
	path := filepath.Join(root, "override.yaml")
	os.WriteFile(path, []byte("telegram:\n  api_id: 456\n"), 0600)
	loaded, err = Load(path)
	if err != nil || loaded.Telegram.APIID != 456 {
		t.Fatal("explicit config ignored", err)
	}
	t.Setenv("PIMG_TG_API_ID", "789")
	loaded, err = Load(path)
	if err != nil || loaded.Telegram.APIID != 789 {
		t.Fatal("environment ignored", err)
	}
}

func TestPortableStartupReadsWindowsPathsOnAnyPlatform(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	run := filepath.Join(root, "runtime")
	saved := defaults()
	saved.Paths = Paths{DataDir: "Z:/pimg/data", RuntimeDir: "Z:/pimg/runtime", TempDir: "Z:/pimg/runtime/tmp"}
	saved.MasterKeyFile = "Z:/pimg/data/system/keys/master.key"
	saved.Telegram.SessionFile = "Z:/pimg/data/system/telegram/session.enc"
	b, _ := json.Marshal(saved)
	os.MkdirAll(filepath.Join(data, "system"), 0700)
	os.WriteFile(filepath.Join(data, "system/startup.json"), b, 0600)
	t.Setenv("PIMG_DATA_DIR", data)
	t.Setenv("PIMG_RUNTIME_DIR", run)
	loaded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MasterKeyFile != filepath.Join(data, "system/keys/master.key") || loaded.Telegram.SessionFile != filepath.Join(data, "system/telegram/session.enc") || loaded.Paths.TempDir != filepath.Join(run, "tmp") {
		t.Fatal("retained source-machine paths")
	}
}
