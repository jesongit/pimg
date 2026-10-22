package dbx

import (
	"path/filepath"
	"testing"
)

func TestTelegramInboxTransactionDeduplicatesAndRejectsFrozenOrUninitializedTopic(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	album, err := d.EnsureDefaultAlbum()
	if err != nil {
		t.Fatal(err)
	}
	if err = d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 9, State: "active"}); err != nil {
		t.Fatal(err)
	}
	m := &TelegramMessage{AccountID: 11, ChatID: 77, TopicID: 9, MessageID: 50, Date: 100, Reference: `{"message_id":50}`}
	if id, err := d.QueueTelegramMessage(album, m); id != 0 || err != nil {
		t.Fatal("uninitialized topic queued", id, err)
	}
	if _, err = d.Exec("UPDATE album_topics SET scan_date=1 WHERE album_id=?", album); err != nil {
		t.Fatal(err)
	}
	id, err := d.QueueTelegramMessage(album, m)
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if again, err := d.QueueTelegramMessage(album, m); err != nil || again != id {
		t.Fatal("duplicate", again, err)
	}
	if _, err = d.Exec("UPDATE album_topics SET state='deleting' WHERE album_id=?", album); err != nil {
		t.Fatal(err)
	}
	m.MessageID++
	if id, err := d.QueueTelegramMessage(album, m); id != 0 || err != nil {
		t.Fatal("frozen topic queued", id, err)
	}
}

func TestTelegramInboxRollsBackMessageWhenTaskCannotBeCommitted(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	album, _ := d.EnsureDefaultAlbum()
	_ = d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 9, State: "active"})
	_, err = d.Exec("UPDATE album_topics SET scan_date=1 WHERE album_id=?", album)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`CREATE TRIGGER reject_push_task BEFORE INSERT ON tasks WHEN NEW.type='tg_receive' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTelegramMessage(album, &TelegramMessage{AccountID: 11, ChatID: 77, TopicID: 9, MessageID: 50, Date: 100, Reference: `{}`}); err == nil {
		t.Fatal("task failure not reported")
	}
	var count int
	if err = d.QueryRow("SELECT count(*) FROM tg_messages").Scan(&count); err != nil || count != 0 {
		t.Fatal("orphaned inbox without work", count, err)
	}
}
