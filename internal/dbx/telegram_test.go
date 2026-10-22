package dbx

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func telegramTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "telegram.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func telegramTestFile(id string, album int64) *File {
	return &File{ID: id, Name: id, OrigName: id + ".png", Ext: "png", Mime: "image/png", Kind: "image", Size: 1, SHA256: "hash", KeyWrapped: []byte{1}, KeyNonce: []byte{2}, AlbumID: sql.NullInt64{Int64: album, Valid: album > 0}, ArchiveState: "done"}
}

func TestTelegramDefaultAndFrozenAlbum(t *testing.T) {
	d := telegramTestDB(t)
	def, err := d.EnsureDefaultAlbum()
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.EnsureDefaultAlbum()
	if err != nil || again != def {
		t.Fatal("default album is not stable", err)
	}
	a, err := d.GetAlbum(def)
	if err != nil || !a.IsDefault || a.IsHome {
		t.Fatal("default shares home semantics", err)
	}
	id, _ := d.InsertAlbum("photos")
	b := &TopicBinding{AlbumID: id, AccountID: 1, ChatID: 2, TopicID: 4, Name: "photos", CreateName: "create", State: "active"}
	if err = d.InsertTopicBinding(b); err != nil {
		t.Fatal(err)
	}
	task, err := d.QueueLinkedAlbumScan(id, `{"album_id":2}`)
	if err != nil {
		t.Fatal(err)
	}
	againTask, err := d.QueueLinkedAlbumScan(id, `{"since":999}`)
	if err != nil || againTask != task {
		t.Fatal("duplicate scan", err)
	}
	if _, err = d.QueueLinkedAlbumDelete(id, `{}`); err != nil {
		t.Fatal(err)
	}
	old, _ := d.GetTask(task)
	if old.State != "canceled" {
		t.Fatal("pending scan survived deletion intent")
	}
	if err = d.AlbumWritable(id); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatal("deleting album writable", err)
	}
	if err = d.InsertFile(telegramTestFile("late-upload", id)); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatal("late upload bypassed SQL freeze", err)
	}
	f := telegramTestFile("move", def)
	if err = d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: id, Valid: true}, nil); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatal("move bypassed SQL freeze", err)
	}
	if err = d.FinishLinkedAlbumDelete(id, def); err != nil {
		t.Fatal(err)
	}
	b, err = d.GetTopicBinding(id)
	if err != nil || b.State != "deleted" {
		t.Fatal("lost retired binding", err)
	}
}

func TestTelegramSourceAndMessageTombstones(t *testing.T) {
	d := telegramTestDB(t)
	for _, id := range []string{"normal-1", "normal-2"} {
		if err := d.InsertFile(telegramTestFile(id, 0)); err != nil {
			t.Fatal("empty source should remain nullable", err)
		}
	}
	f := telegramTestFile("import", 0)
	f.TelegramSource = "1:2:42"
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	duplicate := telegramTestFile("duplicate", 0)
	duplicate.TelegramSource = f.TelegramSource
	if err := d.InsertFile(duplicate); err == nil {
		t.Fatal("duplicate source accepted")
	}
	if _, err := d.Exec(`UPDATE files SET hidden=1 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	got, err := d.FileByTelegramSource(f.TelegramSource)
	if err != nil || !got.Hidden {
		t.Fatal("hidden source lost dedup identity", err)
	}
	m := &TelegramMessage{AccountID: 1, ChatID: 2, MessageID: 42, TopicID: 3, State: "managed", FileID: f.ID}
	if err = d.UpsertTelegramMessage(m); err != nil {
		t.Fatal(err)
	}
	if err = d.SetTelegramMessageState(1, 2, 42, "retired", f.ID, ""); err != nil {
		t.Fatal(err)
	}
	m.State = "pending"
	m.FileID = ""
	if err = d.UpsertTelegramMessage(m); err != nil {
		t.Fatal(err)
	}
	m.State = "managed"
	m.FileID = "replacement"
	if err = d.UpsertTelegramMessage(m); err != nil {
		t.Fatal(err)
	}
	if err = d.SetTelegramMessageState(1, 2, 42, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetTelegramMessage(1, 2, 42)
	if err != nil || stored.State != "retired" || stored.FileID != f.ID {
		t.Fatalf("retired locator resurrected: %+v %v", stored, err)
	}
	crash := telegramTestFile("crash", 0)
	crash.TelegramSource = "1:2:43"
	if err = d.InsertFile(crash); err != nil {
		t.Fatal(err)
	}
	if err = d.UpsertTelegramMessage(&TelegramMessage{AccountID: 1, ChatID: 2, TopicID: 3, MessageID: 43, State: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteFile(crash.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ := d.GetTelegramMessage(1, 2, 43)
	if pending.State != "retired" {
		t.Fatal("delete resurrects source from pre-acknowledgement crash")
	}
}

func TestTelegramBindingAndDurableProgress(t *testing.T) {
	d := telegramTestDB(t)
	a, _ := d.InsertAlbum("local")
	b := &TopicBinding{AlbumID: a, AccountID: 1, ChatID: 2, TopicID: 5, Name: "remote", State: "active"}
	if err := d.BindExistingTelegramTopic(b); err != nil {
		t.Fatal(err)
	}
	other, _ := d.InsertAlbum("other")
	b.AlbumID = other
	if err := d.BindExistingTelegramTopic(b); err == nil {
		t.Fatal("topic double-bound")
	}
	bound, _ := d.GetTopicBinding(a)
	if bound.ScanDate != 0 {
		t.Fatal("binding imported old history without date setup")
	}
	id, err := d.QueueLinkedAlbumScan(a, `{"since":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.TelegramScanProgress(a, id, 123, `{"offset":123}`); err != nil {
		t.Fatal(err)
	}
	task, _ := d.GetTask(id)
	if task.Payload != `{"offset":123}` {
		t.Fatal("page checkpoint not durable")
	}
	if err = d.CompleteTelegramScan(a, 100, 5); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteTelegramScan(a, 100, 8); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteTelegramScan(a, 90, 20); err != nil {
		t.Fatal(err)
	}
	bound, _ = d.GetTopicBinding(a)
	if bound.ScanDate != 100 || bound.ScanMessageID != 8 {
		t.Fatalf("cursor moved backwards: %+v", bound)
	}
	if err = d.SaveTelegramMove(&TelegramMove{FileID: "file", AlbumID: other, Phase: "copy", References: `[{"old":42}]`}); err != nil {
		t.Fatal(err)
	}
	move, err := d.GetTelegramMove("file")
	if err != nil || move.Phase != "copy" || move.References != `[{"old":42}]` {
		t.Fatal("move progress not durable", err)
	}
}

func TestTelegramRenameIntentIsAtomicWithTask(t *testing.T) {
	d := telegramTestDB(t)
	id, _ := d.InsertAlbum("before")
	if err := d.InsertTopicBinding(&TopicBinding{AlbumID: id, AccountID: 1, ChatID: 2, TopicID: 3, Name: "before", CreateName: "create", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER reject_rename_task BEFORE INSERT ON tasks WHEN NEW.type='tg_rename_album' BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameLinkedAlbum(id, "after"); err == nil {
		t.Fatal("expected enqueue failure")
	}
	a, _ := d.GetAlbum(id)
	b, _ := d.GetTopicBinding(id)
	if a.Name != "before" || b.Name != "before" {
		t.Fatal("rename committed without recoverable task")
	}
}

func TestTelegramAlbumTasksRetryTheirBindingInsteadOfFile(t *testing.T) {
	d := telegramTestDB(t)
	id, _ := d.InsertAlbum("album")
	if err := d.InsertTopicBinding(&TopicBinding{AlbumID: id, AccountID: 1, ChatID: 2, TopicID: 3, Name: "album", CreateName: "create", State: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{TaskTelegramScan, TaskTelegramRenameAlbum} {
		task, err := d.EnqueueTask(typ, TelegramAlbumTaskKey(id), TaskPayloadJSON(map[string]any{"album_id": id}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, task); err != nil {
			t.Fatal(err)
		}
		if err = d.RetryTask(task); err != nil {
			t.Fatalf("%s looked up synthetic ID in files: %v", typ, err)
		}
	}
	task, err := d.QueueLinkedAlbumDelete(id, TaskPayloadJSON(map[string]any{"album_id": id}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, task); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(task); err != nil {
		t.Fatal("delete binding task cannot retry", err)
	}
}

func TestTelegramMoveGuardAlsoProtectsExistingDestructiveTaskRetries(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("source", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveTelegramMove(&TelegramMove{FileID: f.ID, AlbumID: 2, Phase: "cleanup"}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{TaskConvert, TaskPurgeFile} {
		task, err := d.EnqueueTask(typ, f.ID, "{}")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, task); err != nil {
			t.Fatal(err)
		}
		if err = d.RetryTask(task); !errors.Is(err, ErrTelegramMoving) {
			t.Fatalf("%s retry bypassed pending move: %v", typ, err)
		}
	}
	f, err := d.GetFile(f.ID)
	if err != nil || f.PurgeState.String != "" {
		t.Fatal("rejected purge mutated source", err)
	}
}
