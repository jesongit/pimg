package dbx

import (
	"encoding/json"
	"pimg/internal/filenames"
	"reflect"
	"testing"
	"time"
)

func TestMediaTimeDrivesOrderingCalendarAndSeek(t *testing.T) {
	d := telegramTestDB(t)
	for i, item := range []struct{ id, date, source string }{{"photo", "2000-01-01T23:30:00-03:00", "exif"}, {"movie", "2001-04-05T10:00:00Z", "video_metadata"}, {"birth", "1999-06-01T00:00:00Z", "file_created"}, {"fallback", "1998-01-01T00:00:00Z", ""}} {
		f := telegramTestFile(item.id, 0)
		stamp, _ := time.Parse(time.RFC3339, item.date)
		if item.source != "" {
			f.Naming = filenames.Capture(stamp, item.source)
		}
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		intake := int64(1800000000 + i)
		if item.source == "" {
			intake = stamp.Unix()
		}
		if _, err := d.Exec(`UPDATE files SET created_at=? WHERE id=?`, intake, item.id); err != nil {
			t.Fatal(err)
		}
	}
	files, total, err := d.ListFiles(FileFilter{Sort: "oldest"})
	if err != nil || total != 4 {
		t.Fatal(err, total)
	}
	var ids []string
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	if !reflect.DeepEqual(ids, []string{"fallback", "birth", "photo", "movie"}) {
		t.Fatal(ids)
	}
	photo, _ := d.GetFile("photo")
	if photo.MediaAt == photo.CreatedAt || photo.MediaTimeSource() != "exif" {
		t.Fatal(photo)
	}
	groups, err := d.FileTimelineGroups(FileFilter{Query: "photo"}, 480, "day")
	if err != nil || len(groups) != 1 || groups[0].Key != "2000-01-02" {
		t.Fatal(groups, err)
	}
	for _, group := range []string{"day", "week", "month", "year"} {
		items, e := d.FileTimelineGroups(FileFilter{}, 480, group)
		if e != nil {
			t.Fatal(e)
		}
		sum := 0
		for _, g := range items {
			rows, n, e := d.ListFiles(FileFilter{After: g.Start, Before: g.End})
			if e != nil || n != g.Count || len(rows) != g.Count {
				t.Fatal(group, g, n, e)
			}
			sum += g.Count
		}
		if sum != 4 {
			t.Fatal(sum)
		}
	}
	files, _, err = d.ListFiles(FileFilter{Before: photo.MediaAt, Sort: "oldest"})
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
}

func TestMediaTimeUpgradeUsesExistingMetadataWithoutChangingRows(t *testing.T) {
	path, old := oldUpgradeDB(t, len(migrations)-2)
	naming := filenames.Capture(time.Date(2000, 1, 1, 9, 8, 7, 0, filenames.Location()), "exif")
	b, _ := json.Marshal(naming)
	_, err := old.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at,naming_json) VALUES('old','kept','camera.jpg','jpg','image/jpeg',3,'hash','image',X'01',X'02','done',1800000000,1800000001,?)`, string(b))
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f, err := d.GetFile("old")
	if err != nil || f.MediaAt != naming.OriginalTime || f.CreatedAt != 1800000000 || f.UpdatedAt != 1800000001 || f.Name != "kept" {
		t.Fatal(f, err)
	}
	var after string
	if err = d.QueryRow(`SELECT naming_json FROM files WHERE id='old'`).Scan(&after); err != nil || after != string(b) {
		t.Fatal("metadata rewritten", after, err)
	}
}

func TestUnclassifiedIncludesLegacyNullWithoutCrossAccountLeak(t *testing.T) {
	d := telegramTestDB(t)
	album, err := d.EnsureOwnerDefaultAlbum(AdminOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "legacy"} {
		f := telegramTestFile(id, album)
		if err = d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.Exec(`UPDATE files SET album_id=NULL WHERE id='legacy'`); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []int64{album, -1} {
		files, total, e := d.ListFiles(FileFilter{OwnerID: AdminOwner, AlbumID: filter})
		if e != nil || total != 2 || len(files) != 2 {
			t.Fatal(files, total, e)
		}
	}
	albums, err := d.ListOwnerAlbums(AdminOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range albums {
		if a.ID == album && a.Count != 2 {
			t.Fatal(a)
		}
	}
	if err = d.CreateAccount("user:another", "another", "hash"); err != nil {
		t.Fatal(err)
	}
	f := telegramTestFile("other", 0)
	f.OwnerID = "user:another"
	if err = d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE files SET album_id=NULL WHERE id='other'`); err != nil {
		t.Fatal(err)
	}
	_, total, err := d.ListFiles(FileFilter{AlbumID: album})
	if err != nil || total != 2 {
		t.Fatal("foreign null included", total, err)
	}
}
