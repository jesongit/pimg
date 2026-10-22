package dbx

import (
	"errors"
	"testing"
)

func TestTelegramAlbumFreezeCannotStrandIncomingMove(t *testing.T) {
	d := telegramTestDB(t)
	source, _ := d.InsertAlbum("source")
	target, _ := d.InsertAlbum("target")
	for _, id := range []int64{source, target} {
		if err := d.InsertTopicBinding(&TopicBinding{AlbumID: id, AccountID: 11, ChatID: 77, TopicID: int(id) + 10, Name: "test", State: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	f := telegramTestFile("move-source", source)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	task, err := d.EnqueueTelegramMove(f.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueLinkedAlbumDelete(target, "{}"); err == nil {
		t.Fatal("destination froze with an incoming task")
	}
	if err := d.FinishTask(task, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveTelegramMove(&TelegramMove{FileID: f.ID, AlbumID: target, Phase: "cleanup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueLinkedAlbumDelete(target, "{}"); err == nil {
		t.Fatal("destination froze with incoming cleanup")
	}
	if err := d.DeleteTelegramMove(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueLinkedAlbumDelete(target, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueTelegramMove(f.ID, target); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatalf("enqueue bypassed destination freeze: %v", err)
	}
	if err := d.SaveTelegramMove(&TelegramMove{FileID: f.ID, AlbumID: target, Phase: "copy"}); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatalf("copy intent bypassed freeze: %v", err)
	}
}
