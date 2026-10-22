package dbx

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLegacyStorageMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:2] {
		if _, err := old.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL); INSERT INTO schema_version VALUES(2);
INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,storage,created_at,updated_at)
VALUES('legacy','photo','photo.png','png','image/png',10,'hash','image',X'01',X'02','done','direct',1,1);
INSERT INTO temp_objects(id,file_id,kind,object_key,created_at,expires_at) VALUES('temp','legacy','original','tmp/old',1,9999999999);
INSERT INTO file_chunks(file_id,kind,idx,message_id,tg_file_id,size,sha256) VALUES('legacy','data',0,42,'tg-id',10,'hash');`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, err := db.GetFile("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if f.ColdBackend != "telegram" || f.HotBackend != "r2" || f.Storage != "direct" {
		t.Fatalf("legacy locations changed: %+v", f)
	}
	o, err := db.LiveTempObject(f.ID, "original", 1)
	if err != nil || o.Backend != "r2" {
		t.Fatal("legacy temporary location changed", err)
	}
	if o.ReadyAt.Valid {
		t.Fatal("migration invented verification for a legacy temporary record")
	}
	c, err := db.ListChunks(f.ID, "data")
	if err != nil || len(c) != 1 || c[0].TgFileID.String != "tg-id" || c[0].ObjectKey != "" {
		t.Fatal("legacy archive locator changed", err)
	}
}
