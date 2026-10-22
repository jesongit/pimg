package dbx

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"pimg/internal/filenames"
)

func TestNamingSequenceConcurrentAndNeverReused(t *testing.T) {
	d := telegramTestDB(t)
	var wg sync.WaitGroup
	errors := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := telegramTestFile(fmt.Sprintf("named-%d", i), 0)
			f.Naming = filenames.Capture(time.Date(2000, 1, 1, 9, 0, 0, 0, filenames.Location()), "exif")
			if i%2 == 0 {
				f.Storage = "direct"
			}
			errors <- d.InsertFile(f)
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total, distinct int
	if err := d.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT json_extract(naming_json,'$.sequence')) FROM files`).Scan(&total, &distinct); err != nil || total != 24 || distinct != 24 {
		t.Fatal("duplicate daily sequence", total, distinct, err)
	}
	if _, err := d.Exec(`DELETE FROM files`); err != nil {
		t.Fatal(err)
	}
	f := telegramTestFile("after-delete", 0)
	f.Naming = filenames.Capture(time.Date(2000, 1, 1, 9, 0, 0, 0, filenames.Location()), "exif")
	if err := d.InsertFile(f); err != nil || f.Naming.Sequence != 25 || f.Name != "PE_20000101_0025" {
		t.Fatal("sequence reused or wrong name", f.Name, f.Naming, err)
	}
}

func TestNamingSequenceExhaustionRollsBackWholeVideoBatch(t *testing.T) {
	d := telegramTestDB(t)
	if _, err := d.Exec(`INSERT INTO file_name_counters(date,last_sequence) VALUES('20000101',9998)`); err != nil {
		t.Fatal(err)
	}
	makeFile := func(id string) *File {
		f := telegramTestFile(id, 0)
		f.Naming = filenames.Capture(time.Date(2000, 1, 1, 0, 0, 0, 0, filenames.Location()), "video_metadata")
		return f
	}
	if err := d.InsertUploadBatch([]*File{makeFile("part-a"), makeFile("part-b")}); err == nil {
		t.Fatal("five-digit filename accepted")
	}
	var files, last int
	if err := d.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil || files != 0 {
		t.Fatal("partial batch committed", files, err)
	}
	if err := d.QueryRow(`SELECT last_sequence FROM file_name_counters`).Scan(&last); err != nil || last != 9998 {
		t.Fatal("counter committed without files", last, err)
	}
}
