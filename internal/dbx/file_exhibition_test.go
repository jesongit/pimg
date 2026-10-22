package dbx

import (
	"database/sql"
	"errors"
	"testing"
)

func TestFilePublicOverridesAndGalleryMembership(t *testing.T) {
	d := telegramTestDB(t)
	a, _ := d.InsertAlbum("public")
	b, _ := d.InsertAlbum("private")
	for _, id := range []string{"inherit", "hidden", "explicit"} {
		if err := d.InsertFile(telegramTestFile(id, a)); err != nil {
			t.Fatal(err)
		}
	}
	before := querySnapshot(t, d, `SELECT id,sha256,key_wrapped,key_nonce,published,storage,album_id FROM files ORDER BY id`)
	if err := d.SetAlbumDisplayed(a, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFileHomeVisibility("hidden", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFileHomeVisibility("explicit", true, 0); err != nil {
		t.Fatal(err)
	}
	if d.FileExhibited("hidden") || !d.FileExhibited("inherit") || !d.FileExhibited("explicit") {
		t.Fatal("override priority")
	}
	filter := FileFilter{OwnerID: AdminOwner, State: "show_home", Sort: "name", Page: 1, PerPage: 60}
	if _, total, err := d.ListFiles(filter); err != nil || total != 2 {
		t.Fatal(total, err)
	}
	if err := d.SetAlbumDisplayed(a, false); err != nil {
		t.Fatal(err)
	}
	if _, total, err := d.ListFiles(filter); err != nil || total != 1 {
		t.Fatal("album revoke cache", total, err)
	}
	if d.FileExhibited("inherit") || !d.FileExhibited("explicit") {
		t.Fatal("independent grants")
	}
	if err := d.SetFileHomeVisibility("explicit", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, total, err := d.ListFiles(filter); err != nil || total != 0 {
		t.Fatal("file revoke cache", total, err)
	}
	if before != querySnapshot(t, d, `SELECT id,sha256,key_wrapped,key_nonce,published,storage,album_id FROM files ORDER BY id`) {
		t.Fatal("original metadata changed")
	}
	if err := d.SetAlbumDisplayed(b, true); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateFileMeta("hidden", "", sql.NullInt64{Int64: b, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	if d.FileExhibited("hidden") {
		t.Fatal("move lost explicit hide")
	}
}

func TestFilePublicBatchReceiptAndClosedInputAreAtomic(t *testing.T) {
	d := telegramTestDB(t)
	a, _ := d.InsertAlbum("album")
	if err := d.InsertFile(telegramTestFile("kept", a)); err != nil {
		t.Fatal(err)
	}
	op, err := d.BeginTaskOperation(AdminOwner, "show_home", "file-public", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetFileHomeVisibility("kept", true, op.ID); err != nil {
		t.Fatal(err)
	}
	if !d.FileExhibited("kept") {
		t.Fatal("explicit grant missing")
	}
	var count int
	d.QueryRow(`SELECT COUNT(*) FROM task_operation_items WHERE operation_id=? AND state='done'`, op.ID).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	closed, err := d.BeginTaskOperation(AdminOwner, "hide_home", "file-private", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SealTaskOperation(closed.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	if err = d.SetFileHomeVisibility("kept", false, closed.ID); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal(err)
	}
	if !d.FileExhibited("kept") {
		t.Fatal("closed input changed grant")
	}
}

func TestFilePublicMigrationFrom29PreservesExistingPolicyAndMedia(t *testing.T) {
	path, old := oldUpgradeDB(t, 29)
	_, err := old.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,show_home,created_at,updated_at) VALUES('legacy','kept','camera.jpg','jpg','image/jpeg',3,'hash','image',X'01',X'02','done',1,1,2)`)
	if err != nil {
		t.Fatal(err)
	}
	var cols string
	old.QueryRow(`SELECT group_concat(name,',') FROM pragma_table_info('files')`).Scan(&cols)
	q := `SELECT ` + cols + ` FROM files ORDER BY id`
	before := querySnapshot(t, &DB{reader: old}, q)
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if before != querySnapshot(t, d, q) {
		t.Fatal("migration changed existing columns")
	}
	f, err := d.GetFile("legacy")
	if err != nil || f.HomeVisibility != "inherit" || d.FileExhibited("legacy") {
		t.Fatal("legacy automatic flag became public", f, err)
	}
	var integrity string
	d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Fatal(integrity)
	}
}

func TestFilePublicConversionPreservesPrivateOverride(t *testing.T) {
	d := telegramTestDB(t)
	a, _ := d.InsertAlbum("visible")
	f := telegramTestFile("source", a)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFileHomeVisibility(f.ID, false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BeginConversion(f.ID, "replacement", false, []byte{1}, []byte{2}, 1); err != nil {
		t.Fatal(err)
	}
	replacement, err := d.GetFile("replacement")
	if err != nil || replacement.HomeVisibility != "private" {
		t.Fatal(replacement, err)
	}
}
