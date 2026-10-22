package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

type albumCreateFaultTG struct {
	*workflowTG
	failTopics bool
}

type accountAlbumCreateTG struct {
	*workflowTG
	createdChats []int64
}

func (f *accountAlbumCreateTG) CreateTopic(ctx context.Context, chat int64, name string) (tguser.Topic, error) {
	f.createdChats = append(f.createdChats, chat)
	return f.workflowTG.CreateTopic(ctx, chat, name)
}

func TestTelegramAlbumTaskInitializesMissingBindingInOwnersGroup(t *testing.T) {
	s, base := workflowEnv(t)
	remote := &accountAlbumCreateTG{workflowTG: base}
	s.UserTG = remote
	for i, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		chat := int64(101 + i)
		if _, err := s.DB.Exec(`INSERT INTO account_telegram(owner_id,account_id,chat_id,create_token,state) VALUES(?,11,?,?,'ready')`, owner, chat, owner); err != nil {
			t.Fatal(err)
		}
		id, err := s.DB.InsertOwnerAlbum(owner, "same-name")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = s.RunRenameLinkedAlbum(ctx, id, "stale-task-name")
		cancel()
		if err != nil {
			t.Fatal("unbound album did not initialize", err)
		}
		b, err := s.DB.GetTopicBinding(id)
		if err != nil || b.State != "active" || b.ChatID != chat || remote.topics[b.TopicID].Name != "same-name" {
			t.Fatal("wrong owner, topic or desired name", b, err)
		}
		if err := s.RunRenameLinkedAlbum(context.Background(), id, "stale-task-name"); err != nil {
			t.Fatal(err)
		}
	}
	if base.createCalls != 2 || len(remote.createdChats) != 2 || remote.createdChats[0] != 101 || remote.createdChats[1] != 102 {
		t.Fatal("replay duplicated topics or crossed accounts", remote.createdChats)
	}
}

func TestTelegramMissingBindingTaskRecoversLostCreateAcknowledgement(t *testing.T) {
	s, remote := workflowEnv(t)
	id, err := s.DB.InsertAlbum("new album")
	if err != nil {
		t.Fatal(err)
	}
	remote.failAfterCreate = true
	if err := s.RunRenameLinkedAlbum(context.Background(), id, "old name"); err == nil {
		t.Fatal("fixture did not lose acknowledgement")
	}
	if err := s.RunRenameLinkedAlbum(context.Background(), id, "old name"); err != nil {
		t.Fatal(err)
	}
	b, err := s.DB.GetTopicBinding(id)
	if err != nil || b.State != "active" || remote.createCalls != 1 || remote.topics[b.TopicID].Name != "new album" {
		t.Fatal("recovery duplicated topic", b, err)
	}
}

func TestTelegramNameSyncFinishesForRemovedAlbums(t *testing.T) {
	s, remote := workflowEnv(t)
	if err := s.RunRenameLinkedAlbum(context.Background(), 999999, "gone"); !errors.Is(err, ErrTelegramAlbumSyncObsolete) {
		t.Fatal("removed album still retryable", err)
	}
	id, _ := workflowBound(t, s, remote)
	if _, err := s.DB.Exec(`UPDATE album_topics SET state='deleting' WHERE album_id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.UserTG = nil
	if err := s.RunRenameLinkedAlbum(context.Background(), id, "gone"); !errors.Is(err, ErrTelegramAlbumSyncObsolete) {
		t.Fatal("deleting album still retryable", err)
	}
}

func (f *albumCreateFaultTG) Topics(ctx context.Context, chat int64) ([]tguser.Topic, error) {
	if f.failTopics {
		return nil, errors.New("Telegram temporarily unavailable")
	}
	return f.workflowTG.Topics(ctx, chat)
}

func TestTelegramAlbumRemoteFailureReturnsDurableAlbumAndRetriesSameIntent(t *testing.T) {
	s, base := workflowEnv(t)
	remote := &albumCreateFaultTG{workflowTG: base, failTopics: true}
	s.UserTG = remote
	id, err := s.CreateLinkedAlbum(context.Background(), "photos")
	if err != nil || id <= 0 {
		t.Fatal("remote failure hid successful local creation", id, err)
	}
	b, err := s.DB.GetTopicBinding(id)
	if err != nil || b.State != "creating" || b.LastError == "" {
		t.Fatal("pending creation has no status", b, err)
	}
	task, err := s.DB.LatestTelegramAlbumTask(id, dbx.TaskTelegramRenameAlbum)
	if err != nil || task.State != "pending" {
		t.Fatal("no durable creation task", task, err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.RetryTask(task.ID); err != nil {
		t.Fatal(err)
	}
	remote.failTopics = false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = s.RunRenameLinkedAlbum(ctx, id, "ignored-stale-name"); err != nil {
		t.Fatal("creating sync failed or recursively locked", err)
	}
	current, _ := s.DB.GetTopicBinding(id)
	if current.State != "active" || current.TopicID <= 1 || base.createCalls != 1 || base.topics[current.TopicID].Name != "photos" || current.CreateName != b.CreateName {
		t.Fatal("creation retry duplicated or changed intent", current)
	}
	if current.ScanDate == 0 || current.ScanTaskID != 0 {
		t.Fatal("new empty topic did not preserve creation-time incremental start")
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM albums WHERE is_default=0 AND is_home=0`).Scan(&n)
	if n != 1 {
		t.Fatal("retry created a second local album", n)
	}
}

func TestTelegramAlbumCreationTaskRecoversCommitLostAcknowledgement(t *testing.T) {
	s, remote := workflowEnv(t)
	remote.failAfterCreate = true
	id, err := s.CreateLinkedAlbum(context.Background(), "photos")
	if err != nil || id <= 0 {
		t.Fatal("committed local album not returned", id, err)
	}
	b, _ := s.DB.GetTopicBinding(id)
	if b.State != "creating" {
		t.Fatal("fixture did not preserve remote-commit window")
	}
	if err = s.RunRenameLinkedAlbum(context.Background(), id, "photos"); err != nil {
		t.Fatal(err)
	}
	if remote.createCalls != 1 {
		t.Fatal("retry created duplicate remote topics", remote.createCalls)
	}
	b, _ = s.DB.GetTopicBinding(id)
	if b.State != "active" || remote.topics[b.TopicID].Name != "photos" {
		t.Fatal("recovered topic not linked", b)
	}
}

func TestTelegramCreateAlbumFromExistingTopicValidatesBeforeWriting(t *testing.T) {
	s, base := workflowEnv(t)
	base.topics[42] = tguser.Topic{ID: 42, Name: "existing topic"}
	remote := &albumCreateFaultTG{workflowTG: base}
	s.UserTG = remote
	var before int
	s.DB.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&before)
	for _, topic := range []int{1, 999} {
		if _, err := s.CreateLinkedAlbumFromTopic(context.Background(), "input name", topic); err == nil {
			t.Fatal("unselectable topic accepted", topic)
		}
	}
	remote.failTopics = true
	if _, err := s.CreateLinkedAlbumFromTopic(context.Background(), "input name", 42); err == nil {
		t.Fatal("failed remote validation left a local album")
	}
	remote.failTopics = false
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&n)
	if n != before {
		t.Fatal("failed selection wrote empty albums", n, before)
	}
	id, err := s.CreateLinkedAlbumFromTopic(context.Background(), "input name", 42)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.DB.GetAlbum(id)
	b, _ := s.DB.GetTopicBinding(id)
	if a.Name != "existing topic" || b.TopicID != 42 || b.ScanDate != 0 || b.ScanTaskID != 0 {
		t.Fatal("bound topic identity or initial scan policy changed", a, b)
	}
	if _, err = s.CreateLinkedAlbumFromTopic(context.Background(), "duplicate", 42); err == nil {
		t.Fatal("existing topic reused")
	}
	s.DB.QueryRow(`SELECT COUNT(*) FROM albums`).Scan(&n)
	if n != before+1 {
		t.Fatal("duplicate left an empty album")
	}
	if base.createCalls != 0 {
		t.Fatal("binding an existing topic created another remote topic")
	}
}
