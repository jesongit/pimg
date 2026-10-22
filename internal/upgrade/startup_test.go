package upgrade

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/config"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

func upgradeFixture(t *testing.T) (*config.Config, *dbx.DB) {
	t.Helper()
	root := t.TempDir()
	c := &config.Config{Paths: config.Paths{DataDir: root, DatabaseFile: filepath.Join(root, "system/pimg.db")}, MasterKeyFile: filepath.Join(root, "system/keys/master.key"), Telegram: config.Telegram{SessionFile: filepath.Join(root, "system/telegram/session.enc")}}
	db, err := dbx.Open(c.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for p, b := range map[string]string{c.MasterKeyFile: base64.StdEncoding.EncodeToString(make([]byte, 32)), c.Telegram.SessionFile: "original-session", filepath.Join(root, ".layout-v2.json"): `{"version":2}`} {
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return c, db
}

func TestStartupBackupPreservesDatabaseKeySessionAndReusesReceipt(t *testing.T) {
	c, db := upgradeFixture(t)
	if err := db.EnsureAdmin("kept", "kept-hash"); err != nil {
		t.Fatal(err)
	}
	if err := BeforeLayout(c, c.Paths.DatabaseFile, nil); err != nil {
		t.Fatal(err)
	}
	if err := BeforeLayout(c, c.Paths.DatabaseFile, nil); err != nil {
		t.Fatal(err)
	}
	dirs, _ := os.ReadDir(filepath.Join(c.Paths.DataDir, "system/upgrade-backups"))
	if len(dirs) != 1 {
		t.Fatal("unchanged restart duplicated backup", len(dirs))
	}
	base := filepath.Join(c.Paths.DataDir, "system/upgrade-backups", dirs[0].Name())
	b, _ := os.ReadFile(filepath.Join(base, "manifest.json"))
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for p, sha := range m.Files {
		got, err := datalayout.Hash(filepath.Join(base, filepath.FromSlash(p)))
		if err != nil || got != sha {
			t.Fatal("invalid receipt", p, err)
		}
	}
	if m.Files["data/system/keys/master.key"] == "" || m.Files["data/system/telegram/session.enc"] == "" {
		t.Fatal("missing protected files")
	}
	copy, err := dbx.Open(filepath.Join(base, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	u, h, err := copy.GetAdmin()
	if err != nil || u != "kept" || h != "kept-hash" {
		t.Fatal("backup data lost")
	}
	db.SetSetting(store.StoragePolicyVersionKey, "1")
	c.StartupVersion = config.CurrentStartupVersion
	if err = BeforeLayout(c, c.Paths.DatabaseFile, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadDir(filepath.Join(c.Paths.DataDir, "system/upgrade-backups"))
	if len(again) != 1 {
		t.Fatal("current startup made backup")
	}
}

func TestStartupBackupFailureAndFutureVersionStopBeforeLayoutWrites(t *testing.T) {
	for _, kind := range []string{"backup", "schema", "policy"} {
		t.Run(kind, func(t *testing.T) {
			c, db := upgradeFixture(t)
			switch kind {
			case "backup":
				os.WriteFile(filepath.Join(c.Paths.DataDir, "system/upgrade-backups"), []byte("obstruction"), 0600)
			case "schema":
				db.Exec(`INSERT INTO schema_version VALUES(9999)`)
			case "policy":
				db.SetSetting(store.StoragePolicyVersionKey, "9999")
			}
			os.Remove(filepath.Join(c.Paths.DataDir, ".layout-v2.json"))
			_, err := datalayout.PrepareWithUpgrade(c, func(p string, g *sql.DB) error { return BeforeLayout(c, p, g) })
			if err == nil {
				t.Fatal("startup accepted invalid upgrade")
			}
			if _, err = os.Stat(filepath.Join(c.Paths.DataDir, ".layout-v2.json")); !os.IsNotExist(err) {
				t.Fatal("layout was written before preflight passed")
			}
		})
	}
}

func TestStartupMissingKeyCannotGenerateReplacement(t *testing.T) {
	c, _ := upgradeFixture(t)
	os.Remove(c.MasterKeyFile)
	if err := RequireExistingKey(c); err == nil {
		t.Fatal("missing original key accepted")
	}
	if _, err := os.Stat(c.MasterKeyFile); !os.IsNotExist(err) {
		t.Fatal("replacement generated")
	}
}

func TestStartupRetryReusesBackupAcrossClosedConnections(t *testing.T) {
	c, db := upgradeFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := BeforeLayout(c, c.Paths.DatabaseFile, nil); err != nil {
			t.Fatal(err)
		}
	}
	dirs, err := os.ReadDir(filepath.Join(c.Paths.DataDir, "system/upgrade-backups"))
	if err != nil || len(dirs) != 1 {
		t.Fatal("unchanged retry duplicated backup", len(dirs), err)
	}
}
