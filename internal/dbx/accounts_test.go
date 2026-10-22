package dbx

import (
	"database/sql"
	"path/filepath"
	"pimg/internal/filenames"
	"testing"
)

func TestAccountIsolationDedupMoveAndDefaults(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.EnsureOwnerDefaultAlbum("user:alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.EnsureOwnerDefaultAlbum("user:bravo")
	if err != nil || a == b {
		t.Fatal(a, b, err)
	}
	makeFile := func(id string, album int64) *File {
		return &File{ID: id, Name: "same", OrigName: "same.png", Kind: "image", Size: 4, SHA256: "same-hash", AlbumID: sql.NullInt64{Int64: album, Valid: true}, KeyWrapped: []byte{}, KeyNonce: []byte{}, ArchiveState: "done"}
	}
	first := makeFile("alpha-file", a)
	if err = db.InsertFile(first); err != nil {
		t.Fatal(err)
	}
	batch := []*File{makeFile("bravo-file", b)}
	if err = db.InsertUploadBatch(batch); err != nil {
		t.Fatal(err)
	}
	if batch[0].ID != "bravo-file" || batch[0].UploadDuplicate {
		t.Fatal("cross-account dedup reused another file")
	}
	sameOwner := []*File{makeFile("alpha-duplicate", a)}
	if err = db.InsertUploadBatch(sameOwner); err != nil || sameOwner[0].ID != first.ID || !sameOwner[0].UploadDuplicate {
		t.Fatal(sameOwner, err)
	}
	if _, err = db.OwnedFile("user:bravo", first.ID); err != ErrNotFound {
		t.Fatal("foreign file accessible", err)
	}
	if _, err = db.Exec(`UPDATE files SET album_id=? WHERE id=?`, b, first.ID); err == nil {
		t.Fatal("cross-account SQL move accepted")
	}
	if _, err = db.Exec(`UPDATE files SET owner_id=? WHERE id=?`, "user:bravo", first.ID); err == nil {
		t.Fatal("file ownership changed")
	}
	bad := makeFile("forged", a)
	bad.OwnerID = "user:bravo"
	if err = db.InsertFile(bad); err == nil {
		t.Fatal("forged file owner accepted")
	}
	list, total, err := db.ListFiles(FileFilter{OwnerID: "user:bravo", Page: 1, PerPage: 20})
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != "bravo-file" {
		t.Fatal(list, total, err)
	}
	if OwnerDirectory("user:a:b") == OwnerDirectory("user:a_b") {
		t.Fatal("owner path collision")
	}
}

func TestAccountNamingCapacityIsIndependent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`INSERT INTO file_name_counters(owner_id,date,last_sequence) VALUES('user:alpha','20261019',9999)`)
	album, _ := db.EnsureOwnerDefaultAlbum("user:bravo")
	f := &File{ID: "bravo", OwnerID: "user:bravo", Kind: "image", Name: "picture", OrigName: "image.png", KeyWrapped: []byte{}, KeyNonce: []byte{}, ArchiveState: "done", AlbumID: sql.NullInt64{Int64: album, Valid: true}, Naming: filenames.Metadata{Date: "20261019"}}
	if err = db.InsertFile(f); err != nil || f.Naming.Sequence != 1 {
		t.Fatal("another account exhausted this account's numbering", f.Naming, err)
	}
}

func TestAccountStorageMigrationKeepsLegacyCredentialsAndFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:len(migrations)-1] {
		if _, err = old.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = old.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL); INSERT INTO schema_version VALUES(?); INSERT INTO admin VALUES(1,'original','password-hash',1,1); INSERT INTO sessions(token_hash,created_at,expires_at) VALUES('cookie',1,9999999999); INSERT INTO albums(name,created_at,is_default) VALUES('未分类',1,1); INSERT INTO settings VALUES('telegram.account_id','123'),('telegram.chat_id','456'); INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,album_id,archive_state,created_at,updated_at) VALUES('kept','name','image.png','png','image/png',4,'hash','image',X'01',X'02',1,'done',1,1)`, len(migrations)-1)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, err := db.GetFile("kept")
	if err != nil || f.OwnerID != AdminOwner || f.AlbumID.Int64 != 1 {
		t.Fatal(f, err)
	}
	session, err := db.GetSession("cookie")
	if err != nil || session.OwnerID != AdminOwner {
		t.Fatal(session, err)
	}
	user, hash, err := db.GetAdmin()
	if err != nil || user != "original" || hash != "password-hash" {
		t.Fatal(user, hash, err)
	}
	group, _ := db.GetSetting("telegram.chat_id")
	if group != "456" {
		t.Fatal(group)
	}
}
