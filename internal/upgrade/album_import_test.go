package upgrade

import (
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/config"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestAlbumOwnerFeatureKeepsCurrentUpgradeWithoutExtraBackup(t *testing.T) {
	c, db := upgradeFixture(t)
	c.StartupVersion = config.CurrentStartupVersion
	if err := db.SetSetting(store.StoragePolicyVersionKey, "1"); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAdmin("kept-admin", "kept-password-hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAccount("user:legacy", "legacy", "legacy-password-hash"); err != nil {
		t.Fatal(err)
	}
	album, err := db.InsertOwnerAlbum("user:legacy", "既有相册")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.InsertOwnerSession("user:legacy", "kept-session", 9999999999, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.InsertToken(&dbx.APIToken{ID: "kept-token", Name: "kept", Scope: "admin", OwnerID: dbx.AdminOwner, TokenHash: "kept-token-hash"}); err != nil {
		t.Fatal(err)
	}
	before, err := dbx.ReadSchemaVersion(db.DB)
	if err != nil {
		t.Fatal(err)
	}
	// A current database must remain current without another startup backup.
	// The manual-date migration from released schema 27 is tested separately.
	if before != dbx.CurrentSchemaVersion() {
		t.Fatal("fixture is not at the current schema", before)
	}
	for i := 0; i < 2; i++ {
		if err = BeforeLayout(c, c.Paths.DatabaseFile, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(filepath.Join(c.Paths.DataDir, "system/upgrade-backups")); !os.IsNotExist(err) {
		t.Fatal("feature created an unnecessary upgrade backup", err)
	}
	after, err := dbx.ReadSchemaVersion(db.DB)
	if err != nil || after != before {
		t.Fatal("feature changed schema", before, after, err)
	}
	if a, err := db.OwnedAlbum("user:legacy", album); err != nil || a.Name != "既有相册" {
		t.Fatal(a, err)
	}
	if a, err := db.AccountByUsername("legacy"); err != nil || a.PasswordHash != "legacy-password-hash" {
		t.Fatal(a, err)
	}
	if _, err = db.GetSession("kept-session"); err != nil {
		t.Fatal("old session lost", err)
	}
	if _, err = db.GetTokenByHash("kept-token-hash"); err != nil {
		t.Fatal("old administrator token lost", err)
	}
}
