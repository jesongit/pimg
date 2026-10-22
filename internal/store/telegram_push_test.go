package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
	"testing"
	"time"
)

func TestTelegramPushDurableDuplicateInboxDoesNotReadHistoryOrAdvanceCursor(t *testing.T) {
	s, tr := workflowEnv(t)
	album, topic := workflowBound(t, s, tr)
	b, _ := s.DB.GetTopicBinding(album)
	before := b.ScanDate
	tr.add(topic, 501, time.Now().Unix(), "pushed.png", "image/png", "", workflowPNG(t))
	msg := tr.messages[501]
	ref, _ := json.Marshal(msg.Reference)
	m := &dbx.TelegramMessage{AccountID: 11, ChatID: 77, TopicID: topic, MessageID: 501, Date: msg.Date, Name: msg.Name, MIME: msg.MIME, Reference: string(ref)}
	one, err := s.DB.QueueTelegramMessage(album, m)
	if err != nil || one == 0 {
		t.Fatal(one, err)
	}
	two, err := s.DB.QueueTelegramMessage(album, m)
	if err != nil || one != two {
		t.Fatal("duplicate inbox task", one, two, err)
	}
	task, _ := s.DB.GetTask(one)
	if !dbx.PassiveTask(task) {
		t.Fatal("push metadata preempted background work")
	}
	// Reopen SQLite before consuming the inbox, as a process restart would.
	var filename string
	var seq int
	var dbName string
	if err = s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &dbName, &filename); err != nil {
		t.Fatal(err)
	}
	if filename == "" || !filepath.IsAbs(filename) {
		t.Fatal(filename)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s.DB, err = dbx.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	task, err = s.DB.GetTask(one)
	if err != nil {
		t.Fatal("inbox work lost after restart", err)
	}
	if err = s.RunTelegramReceive(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	if err = s.RunTelegramReceive(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	if tr.historyCalls != 0 || tr.downloadCalls != 1 {
		t.Fatal("push fetched history or redownloaded", tr.historyCalls, tr.downloadCalls)
	}
	b, _ = s.DB.GetTopicBinding(album)
	if b.ScanDate != before {
		t.Fatal("out-of-order push advanced catch-up cursor")
	}
	if err = s.DB.FinishTask(one, ""); err != nil {
		t.Fatal(err)
	}
	id, err := s.DB.QueueTelegramMessage(album, m)
	if err != nil || id != 0 {
		t.Fatal("completed source requeued", id, err)
	}
}

func TestTelegramPushFiltersOwnArchivesAndUpdatesTopicNameWithoutTask(t *testing.T) {
	s, r := runtimeFixture(t)
	r.chatID = 41
	album, _ := s.DB.InsertAlbum("bound")
	if err := s.DB.InsertTopicBinding(&dbx.TopicBinding{AlbumID: album, AccountID: 77, ChatID: 41, TopicID: 9, Name: "bound", State: "active", ScanDate: 1}); err != nil {
		t.Fatal(err)
	}
	base := tguser.Message{Reference: tguser.Reference{AccountID: 77, ChatID: 41, TopicID: 9, MessageID: 50}, Date: time.Now().Unix(), Name: "PE_20261018_0001.png", MIME: "image/png", Caption: `{"app":"pimg"}`}
	if _, err := s.DB.Exec("UPDATE album_topics SET scan_date=1 WHERE album_id=?", album); err != nil {
		t.Fatal(err)
	}
	r.onMessage(base)
	base.Caption = `{"app":"pimg-part2"}`
	r.onMessage(base)
	base.IsService = true
	base.Name = "renamed"
	base.Caption = "topic_edited"
	r.onMessage(base)
	b, _ := s.DB.GetTopicBinding(album)
	if b.Name != "renamed" {
		t.Fatal(b)
	}
	var count int
	_ = s.DB.QueryRow("SELECT count(*) FROM tasks").Scan(&count)
	if count != 0 {
		t.Fatal("own archive or rename scheduled work", count)
	}
	base.IsService = false
	base.Caption = ""
	base.Name = "new.png"
	r.onMessage(base)
	r.onMessage(base)
	_ = s.DB.QueryRow("SELECT count(*) FROM tasks WHERE type=?", dbx.TaskTelegramReceive).Scan(&count)
	if count != 1 {
		t.Fatal("new media not deduplicated", count)
	}
	if _, err := s.DB.Exec("UPDATE album_topics SET scan_date=0,scan_task_id=0 WHERE album_id=?", album); err != nil {
		t.Fatal(err)
	}
	base.Reference.MessageID++
	r.onMessage(base)
	_ = s.DB.QueryRow("SELECT count(*) FROM tasks WHERE type=?", dbx.TaskTelegramReceive).Scan(&count)
	if count != 1 {
		t.Fatal("uninitialized topic imported without chosen starting point")
	}
}

func TestTelegramSyncStopsPollingAndReconnectionCatchesUp(t *testing.T) {
	s, tr := workflowEnv(t)
	album, topic := workflowBound(t, s, tr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.StartTelegramSync(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("sync did not stop")
		}
	}()
	var first *dbx.Task
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.DB.LatestTelegramAlbumTask(album, dbx.TaskTelegramScan)
		if err == nil {
			first = task
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first == nil {
		t.Fatal("startup catch-up missing")
	}
	if err := s.RunTelegramScan(ctx, first.Payload); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.FinishTask(first.ID, "")
	before := tr.historyCalls
	// Longer idle windows are also checked against the real Docker service.
	time.Sleep(100 * time.Millisecond)
	last, _ := s.DB.LatestTelegramAlbumTask(album, dbx.TaskTelegramScan)
	if last.ID != first.ID || tr.historyCalls != before {
		t.Fatal("idle sync polled")
	}
	tr.add(topic, 701, time.Now().Unix(), "offline.png", "image/png", "", workflowPNG(t))
	s.WakeTelegramSync()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		last, _ = s.DB.LatestTelegramAlbumTask(album, dbx.TaskTelegramScan)
		if last.ID != first.ID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last.ID == first.ID {
		t.Fatal("reconnect catch-up missing")
	}
	if err := s.RunTelegramScan(ctx, last.Payload); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.FileByTelegramSource("11:77:701"); err != nil {
		t.Fatal("offline message missed", err)
	}
}

func TestReconnectWaitsForOldRangeWithoutRepeatedlyScanningOtherAlbums(t *testing.T) {
	s, tr := workflowEnv(t)
	oldAlbum, _ := workflowBound(t, s, tr)
	otherAlbum, _ := workflowBound(t, s, tr)
	goal := time.Now().Unix()
	b, _ := s.DB.GetTopicBinding(oldAlbum)
	old, err := s.DB.QueueLinkedAlbumScan(oldAlbum, dbx.TaskPayloadJSON(TelegramScanPayload{AlbumID: oldAlbum, AccountID: b.AccountID, ChatID: b.ChatID, TopicID: b.TopicID, Mode: "date", Since: goal - 60, Until: goal - 10}))
	if err != nil {
		t.Fatal(err)
	}
	scheduled := map[int64]bool{}
	if err = s.reconcileTelegramTopics(context.Background(), goal, scheduled); err == nil {
		t.Fatal("unfinished old range claimed full reconnect coverage")
	}
	other, err := s.DB.LatestTelegramAlbumTask(otherAlbum, dbx.TaskTelegramScan)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunTelegramScan(context.Background(), other.Payload); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.FinishTask(other.ID, "")
	if err = s.reconcileTelegramTopics(context.Background(), goal, scheduled); err == nil {
		t.Fatal("old range did not remain pending")
	}
	again, _ := s.DB.LatestTelegramAlbumTask(otherAlbum, dbx.TaskTelegramScan)
	if again.ID != other.ID {
		t.Fatal("unrelated album was rescanned while waiting")
	}
	oldTask, _ := s.DB.GetTask(old)
	if err = s.RunTelegramScan(context.Background(), oldTask.Payload); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.FinishTask(old, "")
	if err = s.reconcileTelegramTopics(context.Background(), goal, scheduled); err != nil {
		t.Fatal(err)
	}
	next, _ := s.DB.LatestTelegramAlbumTask(oldAlbum, dbx.TaskTelegramScan)
	if next.ID == old {
		t.Fatal("remaining offline interval lost after old scan finished")
	}
	var p TelegramScanPayload
	if err = json.Unmarshal([]byte(next.Payload), &p); err != nil || p.Until < goal {
		t.Fatal("catch-up did not cover the reconnection", p, err)
	}
}

func TestUnrelatedChannelGapDoesNotWakeArchiveCatchup(t *testing.T) {
	s, r := runtimeFixture(t)
	r.chatID = 41
	wake := make(chan struct{}, 1)
	telegramSyncWakes.Store(s, wake)
	defer telegramSyncWakes.Delete(s)
	r.onGap(42)
	select {
	case <-wake:
		t.Fatal("unrelated channel triggered a scan")
	default:
	}
	for _, chat := range []int64{41, 0} {
		r.onGap(chat)
		select {
		case <-wake:
		default:
			t.Fatal("relevant gap did not request catch-up", chat)
		}
	}
}
