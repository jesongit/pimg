package dbx

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRetryNewAlbumTaskBeforeItsTopicExists(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.CreateAccount("user:alpha", "alpha", "hash")
	id, err := d.InsertOwnerAlbum("user:alpha", "new album")
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.EnqueueUniqueTask(TaskTelegramRenameAlbum, TelegramAlbumTaskKey(id), TaskPayloadJSON(map[string]any{"album_id": id}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET attempts=19,run_after=?,last_error='record not found' WHERE id=?`, now()+3600, task); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryTask(task); err != nil {
		t.Fatal("manual recovery requires a nonexistent binding", err)
	}
	after, _ := d.GetTask(task)
	if after.State != "pending" || after.RunAfter > now() || after.Attempts != 0 {
		t.Fatal("task did not become runnable", after)
	}
	if err := d.DeleteEmptyOwnerAlbum("user:alpha", id); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryTask(task); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed album can be retried", err)
	}
}

func TestEmptyAccountAlbumDeletionPreservesInFlightInitialization(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	owner := "user:alpha"
	d.CreateAccount(owner, "alpha", "hash")
	id, _ := d.InsertOwnerAlbum(owner, "pending")
	task, _ := d.EnqueueTask(TaskTelegramRenameAlbum, TelegramAlbumTaskKey(id), TaskPayloadJSON(map[string]any{"album_id": id}))
	d.Exec(`UPDATE tasks SET state='running' WHERE id=?`, task)
	if err = d.DeleteEmptyOwnerAlbum(owner, id); err == nil {
		t.Fatal("running creator lost its album")
	}
	if _, err = d.GetAlbum(id); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE tasks SET state='pending' WHERE id=?`, task)
	if err = d.DeleteEmptyOwnerAlbum(owner, id); err != nil {
		t.Fatal(err)
	}
	taskRow, _ := d.GetTask(task)
	if taskRow.State != "canceled" {
		t.Fatal("pending creation can recreate an orphan topic", taskRow.State)
	}
	id, _ = d.InsertOwnerAlbum(owner, "creating")
	err = d.InsertTopicBinding(&TopicBinding{AlbumID: id, AccountID: 11, ChatID: 77, Name: "creating", CreateName: "unique", State: "creating"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueLinkedAlbumDelete(id, "{}", owner); err == nil {
		t.Fatal("incomplete topic was frozen for deletion")
	}
	b, _ := d.GetTopicBinding(id)
	if b.State != "creating" {
		t.Fatal("initialization corrupted", b)
	}
	if err = d.ActivateTopicBinding(id, 12); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueLinkedAlbumDelete(id, "{}", "user:bravo"); err == nil {
		t.Fatal("foreign owner deleted the album")
	}
	if _, err = d.QueueLinkedAlbumDelete(id, "{}", owner); err != nil {
		t.Fatal(err)
	}
	b, _ = d.GetTopicBinding(id)
	if b.State != "deleting" {
		t.Fatal("ready empty topic was not queued", b)
	}
}
