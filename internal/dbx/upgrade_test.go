package dbx

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func oldUpgradeDB(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range migrations[:version] {
		if _, err = db.Exec(step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL); INSERT INTO schema_version VALUES(?)`, version); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestUpgradeRollsBackAllPendingSchemaSteps(t *testing.T) {
	path, old := oldUpgradeDB(t, 2)
	// A late conflict must also roll back the earlier v3 provider columns.
	if _, err := old.Exec(`CREATE TABLE media_parts(conflict INTEGER)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if db, err := Open(path); err == nil {
		db.Close()
		t.Fatal("expected late migration failure")
	}
	check, _ := sql.Open("sqlite", path)
	defer check.Close()
	var v, count int
	check.QueryRow(`SELECT max(v) FROM schema_version`).Scan(&v)
	check.QueryRow(`SELECT count(*) FROM pragma_table_info('files') WHERE name='cold_backend'`).Scan(&count)
	if v != 2 || count != 0 {
		t.Fatalf("partial migration committed: version=%d provider columns=%d", v, count)
	}
}

func TestUpgradePreservesLegacyRuleEncryption(t *testing.T) {
	path, old := oldUpgradeDB(t, len(migrations)-1)
	if _, err := old.Exec(`INSERT INTO storage_instances VALUES('local','local','local',X'01',1); INSERT INTO albums(id,name,created_at) VALUES(1,'kept',1); INSERT INTO import_rules(name,instance_id,source,album_id) VALUES('kept','local','photos',1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rules, err := db.ImportRules()
	if err != nil || len(rules) != 1 || !rules[0].Encrypted {
		t.Fatal("old automatic rule lost encrypted intent", rules, err)
	}
}

func TestUpgradeRejectsFutureAndUnversionedDatabase(t *testing.T) {
	for _, future := range []bool{true, false} {
		path, old := oldUpgradeDB(t, 2)
		if future {
			old.Exec(`INSERT INTO schema_version VALUES(9999)`)
		} else {
			old.Exec(`DROP TABLE schema_version`)
		}
		old.Close()
		db, err := Open(path)
		if err == nil {
			db.Close()
			t.Fatal("unknown version accepted")
		}
		if future && !strings.Contains(err.Error(), "9999") {
			t.Fatal(err)
		}
	}
}
