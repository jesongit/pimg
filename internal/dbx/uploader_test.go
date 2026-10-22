package dbx

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestUploaderKeyReservationsSurviveReopenAndRejectRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := UploaderKey{InstanceID: "instance", KeysetID: "keyset", UploadID: "upload", FileID: "file", OwnerID: "owner", ChatID: 77, TopicID: 10, CryptoVersion: 1, Wrapped: []byte("wrapped"), Nonce: []byte("nonce")}
	if _, err = db.ReserveUploaderKey(key); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key.Wrapped = []byte("new value must not replace original")
	got, err := db.ReserveUploaderKey(key)
	if err != nil || string(got.Wrapped) != "wrapped" {
		t.Fatal("persistent retry changed key", err)
	}
	_, err = db.Exec(`INSERT INTO uploader_files(instance_id,keyset_id,tool_file_id,upload_id,album_id,account_id,chat_id,topic_id,mode,state,created_at,updated_at) VALUES('instance','keyset','file','upload',1,11,77,10,'tg','retired',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ReserveUploaderKey(key); !errors.Is(err, ErrUploaderRetired) {
		t.Fatal("retired key identity reactivated", err)
	}
}

func TestImageContentDedupSerializesAcrossDatabaseHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	first := telegramTestFile("canonical", 0)
	first.ArchiveState = "done"
	if err = a.InsertUploadBatch([]*File{first}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := a
			if i%2 == 1 {
				db = b
			}
			f := telegramTestFile(fmt.Sprintf("incoming-%d", i), 0)
			fs := []*File{f}
			if err := db.InsertUploadBatch(fs); err != nil {
				failures <- err
				return
			}
			if fs[0].ID != "canonical" {
				failures <- fmt.Errorf("duplicate record: %s", fs[0].ID)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var files, tasks int
	_ = a.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files)
	_ = a.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks)
	if files != 1 || tasks != 1 {
		t.Fatal("duplicate intake inserted rows/tasks", files, tasks)
	}
}

func TestImagePendingRetryAndFailedCandidateRelease(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := telegramTestFile("first", 0)
	first.ArchiveState = "pending"
	if err = db.InsertUploadBatch([]*File{first}); err != nil {
		t.Fatal(err)
	}
	second := telegramTestFile("second", 0)
	if err = db.InsertUploadBatch([]*File{second}); !errors.Is(err, ErrImagePending) {
		t.Fatal("pending duplicate created competing archive", err)
	}
	if err = db.SetArchiveState(first.ID, "failed", "cannot recover"); err != nil {
		t.Fatal(err)
	}
	if err = db.InsertUploadBatch([]*File{second}); err != nil {
		t.Fatal("failed candidate blocked new intake", err)
	}
}
