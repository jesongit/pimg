package dbx

import (
	"testing"
)

func TestTelegramAlbumCreateIntentAndTaskCommitTogether(t *testing.T) {
	d := telegramTestDB(t)
	if _, err := d.Exec(`CREATE TRIGGER fail_album_sync_task BEFORE INSERT ON tasks WHEN NEW.type='tg_rename_album' BEGIN SELECT RAISE(ABORT,'simulated task failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.CreateLinkedTelegramAlbum("photos", 11, 77, "Pimg-reserved"); err == nil {
		t.Fatal("expected task failure")
	}
	for _, table := range []string{"albums", "album_topics", "tasks"} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s intent committed: %d %v", table, n, err)
		}
	}
	if _, err := d.Exec(`DROP TRIGGER fail_album_sync_task`); err != nil {
		t.Fatal(err)
	}
	id, taskID, err := d.CreateLinkedTelegramAlbum("photos", 11, 77, "Pimg-reserved")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := d.GetTopicBinding(id)
	task, err := d.LatestTelegramAlbumTask(id, TaskTelegramRenameAlbum)
	if err != nil || task.ID != taskID || b.State != "creating" || b.CreateName != "Pimg-reserved" {
		t.Fatal("creation receipt missing", b, task, err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(taskID); err != nil {
		t.Fatal("creating topic cannot retry its task", err)
	}
}

func TestTelegramAlbumFromTopicHasNoEmptyAlbumOnConflictOrFailure(t *testing.T) {
	d := telegramTestDB(t)
	b := &TopicBinding{AccountID: 11, ChatID: 77, TopicID: 42, Name: "existing"}
	if _, err := d.Exec(`CREATE TRIGGER fail_album_binding BEFORE INSERT ON album_topics BEGIN SELECT RAISE(ABORT,'simulated binding failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAlbumFromTelegramTopic(b); err == nil {
		t.Fatal("expected binding failure")
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&n)
	if n != 0 {
		t.Fatal("failed binding left an empty album")
	}
	if _, err := d.Exec(`DROP TRIGGER fail_album_binding`); err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateAlbumFromTelegramTopic(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.CreateAlbumFromTelegramTopic(b); err == nil {
		t.Fatal("same topic bound twice")
	}
	d.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate binding left an empty album", n)
	}
	bound, _ := d.GetTopicBinding(id)
	if bound.ScanDate != 0 || bound.ScanTaskID != 0 {
		t.Fatal("existing history auto-enabled without date setup")
	}
	if _, _, err = d.CreateLinkedTelegramAlbum("creating", 11, 77, "Pimg-in-flight"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CreateAlbumFromTelegramTopic(&TopicBinding{AccountID: 11, ChatID: 77, TopicID: 43, Name: "Pimg-in-flight"}); err == nil {
		t.Fatal("a second album stole an in-flight creation topic")
	}
	d.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&n)
	if n != 2 {
		t.Fatal("reserved topic conflict left an empty album", n)
	}
}
