package dbx

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestExhibitedAlbumsIncludePrivateThumbnailsAndRevokeImmediately(t *testing.T) {
	d := telegramTestDB(t)
	a, _ := d.InsertAlbum("display")
	b, _ := d.InsertAlbum("private")
	for _, id := range []string{"encrypted", "unpublished", "published", "hidden", "purging", "pending", "other"} {
		f := telegramTestFile(id, a)
		if id == "other" {
			f.AlbumID.Int64 = b
		}
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`UPDATE files SET published=1,storage='direct' WHERE id='published'`, `UPDATE files SET hidden=1 WHERE id='hidden'`, `UPDATE files SET purge_state='pending' WHERE id='purging'`, `UPDATE files SET archive_state='pending' WHERE id='pending'`} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := querySnapshot(t, d, `SELECT id,storage,published,key_wrapped,key_nonce,sha256 FROM files ORDER BY id`)
	if _, total, err := d.ListExhibitedFiles(1, 24); err != nil || total != 0 {
		t.Fatal(total, err)
	}
	if err := d.SetAlbumDisplayed(a, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"encrypted", "unpublished", "published"} {
		if !d.FileExhibited(id) {
			t.Fatal("missing", id)
		}
	}
	for _, id := range []string{"hidden", "purging", "pending", "other", "missing"} {
		if d.FileExhibited(id) {
			t.Fatal("exposed", id)
		}
	}
	files, total, err := d.ListExhibitedFiles(1, 2)
	if err != nil || total != 3 || len(files) != 2 {
		t.Fatal(total, err)
	}
	next, _, err := d.ListExhibitedFiles(2, 2)
	if err != nil || len(next) != 1 || next[0].ID == files[0].ID || next[0].ID == files[1].ID {
		t.Fatal("pagination", next, err)
	}
	if err := d.SetAlbumDisplayed(a, false); err != nil {
		t.Fatal(err)
	}
	if d.FileExhibited("encrypted") {
		t.Fatal("grant was retained")
	}
	if before != querySnapshot(t, d, `SELECT id,storage,published,key_wrapped,key_nonce,sha256 FROM files ORDER BY id`) {
		t.Fatal("display changed original state")
	}
	def, _ := d.EnsureDefaultAlbum()
	if err := d.UpdateFileMeta("other", "", sql.NullInt64{Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAlbumDisplayed(def, true); err != nil {
		t.Fatal(err)
	}
	if !d.FileExhibited("other") {
		t.Fatal("NULL unclassified not displayed")
	}
}

func TestExhibitionMigrationFromReleased28PreservesMediaAndRetiresGuest(t *testing.T) {
	path, old := oldUpgradeDB(t, 28)
	for _, q := range []string{
		`INSERT INTO albums(id,name,created_at,is_home) VALUES(19,'游客相册',1,1)`,
		`INSERT INTO accounts(id,username,password_hash,created_at) VALUES('user:kept','kept-user','hash',1)`,
		`INSERT INTO albums(id,name,created_at,is_home,owner_id) VALUES(20,'旧用户游客相册',1,1,'user:kept')`,
		`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,album_id,archive_state,storage,owner_id,created_at,updated_at) VALUES('user-kept','name','b.png','png','image/png',4,'user-hash','image',X'03',X'04',20,'done','tg','user:kept',1,2)`,
		`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,album_id,archive_state,storage,created_at,updated_at) VALUES('kept','name','a.png','png','image/png',3,'hash','image',X'01',X'02',19,'done','tg',1,2)`,
		`INSERT INTO file_chunks(file_id,kind,idx,message_id,size,sha256) VALUES('kept','data',0,123,3,'hash')`,
		`INSERT INTO archive_objects(file_id,instance_id,kind,idx,reference,size,sha256,verified,album_id) VALUES('kept','local','data',0,'{"key":"original-location"}',3,'hash',1,19)`,
		`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule) VALUES('synthetic','[]',19,1,0,'now')`,
		`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('local_import_scan','', '{"album_id":19,"batch_id":1}', 'pending',1,1,1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	before := querySnapshot(t, &DB{reader: old}, `SELECT id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,storage,published,created_at FROM files`)
	objects := querySnapshot(t, &DB{reader: old}, `SELECT * FROM archive_objects`)
	chunks := querySnapshot(t, &DB{reader: old}, `SELECT * FROM file_chunks`)
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var home, displayed, defaults int
	d.QueryRow(`SELECT COUNT(*) FROM albums WHERE is_home=1`).Scan(&home)
	d.QueryRow(`SELECT SUM(displayed),SUM(is_default) FROM albums`).Scan(&displayed, &defaults)
	f, err := d.GetFile("kept")
	if err != nil || !f.AlbumID.Valid || f.AlbumID.Int64 == 19 || home != 0 || displayed != 0 || defaults != 2 {
		t.Fatal(f, home, displayed, defaults, err)
	}
	userFile, err := d.GetFile("user-kept")
	if err != nil || userFile.OwnerID != "user:kept" || !userFile.AlbumID.Valid || userFile.AlbumID.Int64 == f.AlbumID.Int64 || userFile.AlbumID.Int64 == 20 {
		t.Fatal("owner-specific unclassified lost", userFile, err)
	}
	var imported, target int64
	d.QueryRow(`SELECT album_id FROM local_import_batches`).Scan(&imported)
	d.QueryRow(`SELECT json_extract(payload,'$.album_id') FROM tasks WHERE type='local_import_scan'`).Scan(&target)
	if imported != f.AlbumID.Int64 || target != f.AlbumID.Int64 {
		t.Fatal("pending intake destination lost", imported, target)
	}
	if before != querySnapshot(t, d, `SELECT id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,storage,published,created_at FROM files`) || objects != querySnapshot(t, d, `SELECT * FROM archive_objects`) || chunks != querySnapshot(t, d, `SELECT * FROM file_chunks`) {
		t.Fatal("original data changed")
	}
	var check string
	d.QueryRow(`PRAGMA integrity_check`).Scan(&check)
	if check != "ok" {
		t.Fatal(check)
	}
	rows := querySnapshot(t, d, `SELECT * FROM files`)
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !reflect.DeepEqual(rows, querySnapshot(t, d, `SELECT * FROM files`)) {
		t.Fatal("restart repeated migration")
	}
	if d.FileExhibited("kept") {
		t.Fatal("migration publicly displayed file")
	}
	var schema string
	d.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='albums'`).Scan(&schema)
	if !strings.Contains(schema, "displayed") {
		t.Fatal(schema)
	}
}
