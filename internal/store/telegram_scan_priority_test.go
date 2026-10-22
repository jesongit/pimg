package store

import (
	"context"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

func TestAutomaticTelegramScanOnlyPreemptsIdleForUnseenMedia(t *testing.T) {
	s, transport := workflowEnv(t)
	album, topic := workflowBound(t, s, transport)
	now := time.Now().Unix()
	transport.add(topic, 5, now, "known.png", "image/png", "", workflowPNG(t))
	if err := s.DB.UpsertTelegramMessage(&dbx.TelegramMessage{AccountID: 11, ChatID: 77, TopicID: topic, MessageID: 5, Date: now, Reference: dbx.TaskPayloadJSON(transport.messages[5].Reference), State: "managed", FileID: "known-receipt"}); err != nil {
		t.Fatal(err)
	}
	transport.add(topic, 6, now, "notes.pdf", "application/pdf", "", []byte("attachment"))
	transport.add(topic, 7, now, "text", "", "harmless text", nil)
	transport.add(topic, 8, now, "unregistered.pimg.png", "image/png", `{"app":"pimg-part2"}`, workflowPNG(t))
	probe, err := s.QueueAutomaticTelegramScan(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal("automatic probe prevented idle start", err)
	}
	defer finish()
	for i := 0; i < 3; i++ {
		task, err := s.DB.GetTask(probe)
		if err != nil || !dbx.PassiveTask(task) {
			t.Fatal("automatic flag was not persisted", err)
		}
		if err = s.RunTelegramScan(context.Background(), task.Payload); err != nil {
			t.Fatal(err)
		}
		if idle.Err() != nil {
			t.Fatal("unchanged media or unrelated message canceled an idle download")
		}
		if err = s.DB.FinishTask(probe, ""); err != nil {
			t.Fatal(err)
		}
		probe, err = s.QueueAutomaticTelegramScan(context.Background(), album)
		if err != nil {
			t.Fatal(err)
		}
	}
	if transport.downloadCalls != 0 {
		t.Fatal("unchanged scan downloaded existing or unrelated content")
	}
	transport.add(topic, 9, time.Now().Unix(), "new.png", "image/png", "", workflowPNG(t))
	task, err := s.DB.GetTask(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunTelegramScan(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	if idle.Err() != context.Canceled {
		t.Fatal("new media ingestion did not preempt idle download")
	}
	if transport.downloadCalls != 1 {
		t.Fatal("new media was not downloaded exactly once", transport.downloadCalls)
	}
	if _, err = s.DB.FileByTelegramSource("11:77:9"); err != nil {
		t.Fatal("automatic ingestion did not persist source identity", err)
	}
}

func TestTelegramRuntimeOnlyQueuesBoundNewMedia(t *testing.T) {
	s, runtime := runtimeFixture(t)
	runtime.chatID = 41
	album, err := s.DB.InsertAlbum("bound")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.InsertTopicBinding(&dbx.TopicBinding{AlbumID: album, AccountID: 77, ChatID: 41, TopicID: 9, Name: "bound", CreateName: "bound-create", State: "active", ScanDate: 1}); err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	if _, err = s.DB.Exec("UPDATE album_topics SET scan_date=1 WHERE album_id=?", album); err != nil {
		t.Fatal(err)
	}
	telegramSyncWakes.Store(s, wake)
	t.Cleanup(func() { telegramSyncWakes.Delete(s) })
	base := tguser.Message{Reference: tguser.Reference{AccountID: 77, ChatID: 41, TopicID: 9, MessageID: 50}, Name: "picture.png", MIME: "image/png"}
	for i, tc := range []struct {
		name string
		edit func(*tguser.Message)
		wake bool
	}{
		{"bound image", func(*tguser.Message) {}, true},
		{"different group", func(m *tguser.Message) { m.Reference.ChatID = 42 }, false},
		{"different account", func(m *tguser.Message) { m.Reference.AccountID = 78 }, false},
		{"General", func(m *tguser.Message) { m.Reference.TopicID = 1 }, false},
		{"unbound topic", func(m *tguser.Message) { m.Reference.TopicID = 10 }, false},
		{"text", func(m *tguser.Message) { m.Name, m.MIME = "", "" }, false},
		{"PDF", func(m *tguser.Message) { m.Name, m.MIME = "notes.pdf", "application/pdf" }, false},
		{"video", func(m *tguser.Message) { m.Name, m.MIME = "video.mp4", "video/mp4" }, true},
		{"topic rename", func(m *tguser.Message) { m.IsService, m.Caption = true, "topic_edited" }, false},
		{"irrelevant service", func(m *tguser.Message) { m.IsService, m.Caption = true, "service_message" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := base
			message.Reference.MessageID += i
			tc.edit(&message)
			runtime.onMessage(message)
			var count int
			if err := s.DB.QueryRow("SELECT count(*) FROM tasks WHERE type=? AND json_extract(payload,'$.message_id')=?", dbx.TaskTelegramReceive, message.Reference.MessageID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != tc.wake {
				t.Fatal("unexpected message task", count, tc.wake)
			}
			select {
			case <-wake:
				t.Fatal("message woke a history scan")
			default:
			}

		})
	}
	if _, err = s.DB.Exec(`UPDATE album_topics SET state='deleting' WHERE album_id=?`, album); err != nil {
		t.Fatal(err)
	}
	runtime.onMessage(base)
	select {
	case <-wake:
		t.Fatal("frozen topic woke intake")
	default:
	}
}
