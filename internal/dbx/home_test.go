package dbx

import (
	"database/sql"
	"reflect"
	"testing"
)

func TestListHomeVisibilityAndStablePagination(t *testing.T) {
	d := telegramTestDB(t)
	guest, err := d.InsertAlbum("guest")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertAlbum("other")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"public-a", "public-b", "private", "unpublished", "outside", "hidden", "purging"} {
		f := telegramTestFile(id, guest)
		f.Storage = "direct"
		if id == "private" {
			f.Storage = "tg"
		}
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`UPDATE files SET published=1,published_at=100`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE files SET published=0 WHERE id='unpublished'`,
		`UPDATE files SET hidden=1 WHERE id='hidden'`,
		`UPDATE files SET purge_state='pending' WHERE id='purging'`,
	} {
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`UPDATE files SET album_id=? WHERE id='outside'`, other); err != nil {
		t.Fatal(err)
	}
	ids := func(page, perPage int) []string {
		t.Helper()
		files, total, err := d.ListHome(guest, page, perPage)
		if err != nil || total != 2 {
			t.Fatalf("home count = %d, err = %v", total, err)
		}
		out := []string{}
		for _, f := range files {
			out = append(out, f.ID)
		}
		return out
	}
	if got := ids(1, 1); !reflect.DeepEqual(got, []string{"public-b"}) {
		t.Fatal("wrong first page", got)
	}
	if got := ids(2, 1); !reflect.DeepEqual(got, []string{"public-a"}) {
		t.Fatal("unstable tied timestamp pagination", got)
	}
	if got := ids(1, 24); !reflect.DeepEqual(got, []string{"public-b", "public-a"}) {
		t.Fatal("private/unpublished/hidden/purging/outside file leaked", got)
	}
	// Moving a published file into the guest album makes it visible.
	if err := d.UpdateFileMeta("outside", "", sql.NullInt64{Int64: guest, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	files, total, err := d.ListHome(guest, 1, 24)
	if err != nil || total != 3 || len(files) != 3 {
		t.Fatal("published move did not reach home", total, err)
	}
	if _, err := d.Exec(`UPDATE files SET published=0 WHERE id='public-a'`); err != nil {
		t.Fatal(err)
	}
	files, total, err = d.ListHome(guest, 1, 24)
	if err != nil || total != 2 || len(files) != 2 {
		t.Fatal("revoked file remained on home", total, err)
	}
	for _, f := range files {
		if f.ID == "public-a" || f.Storage != "direct" || !f.Published {
			t.Fatal("invalid public item", f.ID)
		}
	}
}
