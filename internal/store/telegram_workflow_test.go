package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

// All transport operations stay in memory; these tests cannot contact Telegram.
type workflowTG struct {
	mu                                                         sync.Mutex
	topics                                                     map[int]tguser.Topic
	messages                                                   map[int]tguser.Message
	data                                                       map[int][]byte
	next                                                       int
	createCalls, downloadCalls, deleteTopicCalls, historyCalls int
	pageSize                                                   int
	failAfterCreate                                            bool
	failDownload                                               map[int]int
	historySignal                                              chan struct{}
	failHistoryOffset                                          int
	copyCalls, deleteCalls                                     int
	failNextCopyDownload                                       bool
	failDelete                                                 map[int]int
	corruptCopy                                                bool
}

func (f *workflowTG) Status() tguser.Status {
	return tguser.Status{State: "authorized", AccountID: 11, Configured: true}
}
func (f *workflowTG) Groups(context.Context) ([]tguser.Group, error) {
	return []tguser.Group{{ID: 77, Name: "test"}}, nil
}
func (f *workflowTG) Topics(context.Context, int64) ([]tguser.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []tguser.Topic{}
	for _, t := range f.topics {
		out = append(out, t)
	}
	return out, nil
}
func (f *workflowTG) CreateTopic(_ context.Context, _ int64, name string) (tguser.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.next++
	t := tguser.Topic{ID: f.next, Name: name}
	f.topics[t.ID] = t
	if f.failAfterCreate {
		f.failAfterCreate = false
		return tguser.Topic{}, errors.New("connection lost after remote commit")
	}
	return t, nil
}
func (f *workflowTG) RenameTopic(_ context.Context, _ int64, id int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.topics[id]
	if !ok {
		return errors.New("TOPIC_DELETED")
	}
	t.Name = name
	f.topics[id] = t
	return nil
}
func (f *workflowTG) CloseTopic(_ context.Context, _ int64, id int, closed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.topics[id]
	if !ok {
		return errors.New("TOPIC_DELETED")
	}
	t.Closed = closed
	f.topics[id] = t
	return nil
}
func (f *workflowTG) DeleteTopic(_ context.Context, _ int64, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteTopicCalls++
	delete(f.topics, id)
	for mid, m := range f.messages {
		if m.Reference.TopicID == id {
			delete(f.messages, mid)
			delete(f.data, mid)
		}
	}
	return nil
}
func (f *workflowTG) Upload(_ context.Context, chat int64, topic int, name, mime string, data, thumb []byte, caption string, randomID int64) (tguser.Reference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	r := tguser.Reference{AccountID: 11, ChatID: chat, TopicID: topic, MessageID: f.next, DocumentID: int64(f.next), Size: int64(len(data))}
	f.messages[r.MessageID] = tguser.Message{Reference: r, Date: time.Now().Unix(), Name: name, MIME: mime, Caption: caption}
	f.data[r.MessageID] = bytes.Clone(data)
	return r, nil
}
func (f *workflowTG) Download(_ context.Context, r tguser.Reference) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloadCalls++
	if f.failDownload[r.MessageID] > 0 {
		f.failDownload[r.MessageID]--
		return nil, errors.New("temporary download failure")
	}
	data, ok := f.data[r.MessageID]
	if !ok {
		return nil, errors.New("MESSAGE_ID_INVALID")
	}
	return bytes.Clone(data), nil
}
func (f *workflowTG) Copy(_ context.Context, r tguser.Reference, chat int64, topic int, randomID int64) (tguser.Reference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyCalls++
	old, ok := f.messages[r.MessageID]
	if !ok {
		return tguser.Reference{}, errors.New("MESSAGE_ID_INVALID")
	}
	oldID := r.MessageID
	f.next++
	r.MessageID = f.next
	r.ChatID = chat
	r.TopicID = topic
	old.Reference = r
	f.messages[r.MessageID] = old
	f.data[r.MessageID] = bytes.Clone(f.data[oldID])
	if f.failNextCopyDownload {
		f.failNextCopyDownload = false
		f.failDownload[r.MessageID] = 1
	}
	if f.corruptCopy {
		f.data[r.MessageID] = []byte("corrupted replacement")
	}
	return r, nil
}
func (f *workflowTG) Delete(_ context.Context, r tguser.Reference) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.failDelete[r.MessageID] > 0 {
		f.failDelete[r.MessageID]--
		return errors.New("temporary delete failure")
	}
	delete(f.messages, r.MessageID)
	delete(f.data, r.MessageID)
	return nil
}
func (f *workflowTG) History(_ context.Context, chat int64, topic int, since, until time.Time, offset, limit int) (tguser.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyCalls++
	if f.failHistoryOffset > 0 && offset == f.failHistoryOffset {
		f.failHistoryOffset = 0
		return tguser.Page{}, errors.New("history connection interrupted")
	}
	if f.historySignal != nil {
		select {
		case f.historySignal <- struct{}{}:
		default:
		}
	}
	var all []tguser.Message
	for _, m := range f.messages {
		if m.Reference.ChatID != chat || m.Reference.TopicID != topic {
			continue
		}
		if offset > 0 && m.Reference.MessageID >= offset {
			continue
		}
		if !since.IsZero() && m.Date < since.Unix() {
			continue
		}
		if !until.IsZero() && m.Date >= until.Unix() {
			continue
		}
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Reference.MessageID > all[j].Reference.MessageID })
	if f.pageSize > 0 && f.pageSize < limit {
		limit = f.pageSize
	}
	p := tguser.Page{Done: len(all) <= limit}
	if len(all) > limit {
		all = all[:limit]
	}
	p.Messages = all
	if len(all) > 0 {
		p.NextOffset = all[len(all)-1].Reference.MessageID
	}
	return p, nil
}
func (f *workflowTG) add(topic, id int, date int64, name, mime, caption string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := tguser.Reference{AccountID: 11, ChatID: 77, TopicID: topic, MessageID: id, DocumentID: int64(id), Size: int64(len(data))}
	f.messages[id] = tguser.Message{Reference: r, Date: date, Name: name, MIME: mime, Caption: caption}
	f.data[id] = bytes.Clone(data)
}
func workflowEnv(t *testing.T) (*Service, *workflowTG) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	f := &workflowTG{topics: map[int]tguser.Topic{1: {ID: 1, Name: "General"}}, messages: map[int]tguser.Message{}, data: map[int][]byte{}, next: 1000, failDownload: map[int]int{}, failDelete: map[int]int{}}
	s.UserTG = f
	s.TG = nil
	s.Cfg.Telegram.ChatID = 77
	s.coldBackends["telegram"] = userCold{s}
	return s, f
}
func workflowPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func workflowBound(t *testing.T, s *Service, f *workflowTG) (int64, int) {
	t.Helper()
	id, err := s.CreateLinkedAlbum(context.Background(), "photos")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.DB.GetTopicBinding(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, b.TopicID
}

func TestTelegramTopicCreationRecoversRemoteCommit(t *testing.T) {
	s, f := workflowEnv(t)
	id, _ := s.DB.InsertAlbum("photos")
	f.failAfterCreate = true
	if _, err := s.EnsureTelegramAlbum(context.Background(), id); err == nil {
		t.Fatal("expected simulated transport failure")
	}
	b, err := s.EnsureTelegramAlbum(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if f.createCalls != 1 || b.TopicID <= 1 || b.State != "active" {
		t.Fatalf("creation was duplicated: %+v calls=%d", b, f.createCalls)
	}
	if f.topics[b.TopicID].Name != "photos" {
		t.Fatal("temporary creation name leaked after recovery")
	}
	if _, err = s.QueueDeleteLinkedAlbum(context.Background(), 0); err == nil {
		t.Fatal("default deletion accepted")
	}
}

func TestTelegramScanDateDedupAndFailureRecovery(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	data := workflowPNG(t)
	f.pageSize = 1
	for _, x := range []struct {
		id   int
		date int64
	}{{5, 101}, {4, 100}, {3, 100}, {2, 99}} {
		f.add(topic, x.id, x.date, fmt.Sprintf("%d.png", x.id), "image/png", "", append(bytes.Clone(data), byte(x.id)))
	}
	f.failDownload[4] = 1
	taskID, err := s.QueueTelegramScan(context.Background(), album, "date", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := s.DB.GetTask(taskID)
	if err = s.RunTelegramScan(context.Background(), task.Payload); err == nil {
		t.Fatal("download failure was swallowed")
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE telegram_source IS NOT NULL`).Scan(&count)
	if count != 2 {
		t.Fatal("failed message blocked older discovery", count)
	}
	failed, _ := s.DB.GetTelegramMessage(11, 77, 4)
	if failed.State != "failed" {
		t.Fatal("failure lost durable inbox state")
	}
	task, _ = s.DB.GetTask(taskID)
	if err = s.RunTelegramScan(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	if err = s.RunTelegramScan(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE telegram_source IS NOT NULL`).Scan(&count)
	if count != 3 {
		t.Fatal("inclusive date or dedup failed", count)
	}
	for _, mid := range []int{3, 4, 5} {
		file, err := s.DB.FileByTelegramSource(fmt.Sprintf("11:77:%d", mid))
		if err != nil || file.Published || file.Storage == "direct" || file.AlbumID.Int64 != album {
			t.Fatalf("unsafe import %d: %+v %v", mid, file, err)
		}
	}
	if _, err = s.DB.GetTelegramMessage(11, 77, 2); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("scan included message before specified date", err)
	}
}

func TestTelegramScanReusesCrashWindowSource(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	data := workflowPNG(t)
	f.add(topic, 42, 100, "photo.png", "image/png", "", data)
	created, err := s.receiveLegacyTelegramFixture(context.Background(), data, "photo.png", "image/png", album, "11:77:42")
	if err != nil {
		t.Fatal(err)
	}
	msg := f.messages[42]
	got, err := s.ReceiveTelegramMessage(context.Background(), msg, album)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || f.downloadCalls != 0 {
		t.Fatal("received bytes a second time after committed source", got.ID, created.ID, f.downloadCalls)
	}
}

func TestTelegramManagedRetiredAndUnregisteredCarrier(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	for _, mid := range []int{10, 11, 12} {
		f.add(topic, mid, 100, "chunk.png", "image/png", `{"app":"pimg","file":"owned"}`, []byte("opaque"))
	}
	if err := s.recordManagedTelegram(f.messages[10].Reference, "owned", "chunk.png", "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := s.recordManagedTelegram(f.messages[11].Reference, "retired", "chunk.png", "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRetiredTelegram(f.messages[11].Reference); err != nil {
		t.Fatal(err)
	}
	for _, mid := range []int{10, 11, 12} {
		if _, err := s.ReceiveTelegramMessage(context.Background(), f.messages[mid], album); err != nil && !errors.Is(err, dbx.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if f.downloadCalls != 0 {
		t.Fatal("self chunks were imported again")
	}
	unknown, _ := s.DB.GetTelegramMessage(11, 77, 12)
	if unknown.State != "ignored" || !strings.Contains(unknown.LastError, "待核对") {
		t.Fatal("unknown own data was not retained")
	}
}

func TestTelegramDeleteIgnoresUnmanagedContentAfterMovingFiles(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	file := workflowArchivedFile(t, s, album)
	f.add(topic, 20, 1, "", "", "plain text", nil)
	f.add(topic, 21, 1, "notes.pdf", "application/pdf", "", []byte("unrelated attachment"))
	f.add(topic, 22, 1, "orphan.pimg.png", "image/png", `{"app":"pimg","file":"unregistered"}`, []byte("unregistered carrier"))
	f.add(1, 23, 1, "other-topic.zip", "application/zip", "", []byte("keep another topic"))
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal("unrelated content blocked topic deletion", err)
	}
	if f.deleteTopicCalls != 1 {
		t.Fatal("topic was not deleted")
	}
	if _, err := s.DB.GetAlbum(album); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("deleted album remains", err)
	}
	current, err := s.DB.GetFile(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := s.DB.GetAlbum(current.AlbumID.Int64)
	if err != nil || !destination.IsDefault {
		t.Fatal("managed file was not moved to Uncategorized", err)
	}
	if _, err := s.Verify(context.Background(), file.ID); err != nil {
		t.Fatal("managed file lost its archive", err)
	}
	for _, mid := range []int{20, 21, 22} {
		if _, exists := f.messages[mid]; exists {
			t.Fatal("unrelated content remained in deleted topic", mid)
		}
	}
	if _, exists := f.messages[23]; !exists {
		t.Fatal("another topic's attachment was deleted")
	}
}

type deleteSweepTG struct {
	*workflowTG
	sweeps        int
	onSecondSweep func()
}

func (f *deleteSweepTG) History(ctx context.Context, chat int64, topic int, since, until time.Time, offset, limit int) (tguser.Page, error) {
	if offset == 0 {
		f.sweeps++
		if f.sweeps == 2 {
			f.onSecondSweep()
		}
	}
	return f.workflowTG.History(ctx, chat, topic, since, until, offset, limit)
}

func TestTelegramDeleteNewUnrelatedContentDoesNotBlock(t *testing.T) {
	s, remote := workflowEnv(t)
	album, topic := workflowBound(t, s, remote)
	file := workflowArchivedFile(t, s, album)
	s.UserTG = &deleteSweepTG{workflowTG: remote, onSecondSweep: func() {
		remote.add(topic, 20000, 1, "late.pdf", "application/pdf", "unrelated late arrival", []byte("unrelated"))
	}}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil || remote.deleteTopicCalls != 1 {
		t.Fatal("new unrelated attachment blocked deletion", err)
	}
	if _, err := s.Verify(context.Background(), file.ID); err != nil {
		t.Fatal("managed archive was lost", err)
	}
}

func TestTelegramDeleteNewMediaStillWaitsForArchive(t *testing.T) {
	s, remote := workflowEnv(t)
	album, topic := workflowBound(t, s, remote)
	file := workflowArchivedFile(t, s, album)
	s.UserTG = &deleteSweepTG{workflowTG: remote, onSecondSweep: func() {
		remote.add(topic, 20000, 1, "late.png", "image/png", "", workflowPNG(t))
	}}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil || remote.deleteTopicCalls != 0 {
		t.Fatal("new media was deleted before archiving", err)
	}
	late, err := s.DB.FileByTelegramSource("11:77:20000")
	if err != nil {
		t.Fatal("new media was not indexed", err)
	}
	if err := s.RunArchive(context.Background(), late.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil || remote.deleteTopicCalls != 1 {
		t.Fatal("deletion did not resume after media archive", err)
	}
	for _, id := range []string{file.ID, late.ID} {
		if _, err := s.Verify(context.Background(), id); err != nil {
			t.Fatal("archived media cannot be restored after topic deletion", id, err)
		}
	}
}

func TestTelegramDeleteResumesAfterRemoteTopicRemoved(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetTopicDeletePhase(album, "verified"); err != nil {
		t.Fatal(err)
	}
	delete(f.topics, topic) // Crash after successful remote deletion, before DB commit.
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.GetAlbum(album); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("DB cleanup did not resume", err)
	}
	b, _ := s.DB.GetTopicBinding(album)
	if b.State != "deleted" {
		t.Fatal("retired source binding was discarded")
	}
}

func TestTelegramDeleteSweepsOldUnscannedImageBeforeRemoval(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	f.add(topic, 30, 1, "old.png", "image/png", "", workflowPNG(t))
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	err := s.RunDeleteLinkedAlbum(context.Background(), album)
	if err == nil || !strings.Contains(err.Error(), "尚未归档") {
		t.Fatal("unverified intake must block deletion", err)
	}
	file, err := s.DB.FileByTelegramSource("11:77:30")
	if err != nil {
		t.Fatal("old message outside normal date was missed", err)
	}
	destination, _ := s.DB.GetAlbum(file.AlbumID.Int64)
	if !destination.IsDefault {
		t.Fatal("source was not imported into fixed unclassified topic")
	}
	if f.deleteTopicCalls != 0 {
		t.Fatal("source deleted before imported archive finished")
	}
	if err = s.RunArchive(context.Background(), file.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if f.deleteTopicCalls != 1 {
		t.Fatal("verified deletion did not finish")
	}
}

func TestTelegramDeleteDoesNotInvertArchiveBindingLock(t *testing.T) {
	s, f := workflowEnv(t)
	album, _ := workflowBound(t, s, f)
	file, err := s.receiveLegacyTelegramFixture(context.Background(), workflowPNG(t), "pending.png", "image/png", album, "11:77:90")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	// An archive can hold its file lock just before it resolves the topic.
	release, err := s.lockFile(context.Background(), file.ID)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	f.historySignal = make(chan struct{}, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { done <- s.RunDeleteLinkedAlbum(ctx, album) }()
	select {
	case <-f.historySignal:
	case <-ctx.Done():
		t.Fatal("deletion did not reach source sweep")
	}
	if _, err = s.EnsureTelegramAlbum(ctx, album); !errors.Is(err, dbx.ErrAlbumFrozen) {
		t.Fatalf("archive->binding blocked behind deletion->file: %v", err)
	}
	unlock()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("deletion lock did not recover")
	}
}

func TestTelegramExplicitBindingPausesHistoryAndRejectsReuse(t *testing.T) {
	s, f := workflowEnv(t)
	f.topics[42] = tguser.Topic{ID: 42, Name: "remote album"}
	album, _ := s.DB.InsertAlbum("local album")
	if err := s.BindTelegramTopic(context.Background(), album, 42); err != nil {
		t.Fatal(err)
	}
	a, _ := s.DB.GetAlbum(album)
	b, _ := s.DB.GetTopicBinding(album)
	if a.Name != "remote album" || b.ScanDate != 0 || b.ScanTaskID != 0 {
		t.Fatalf("binding changed history policy: %+v %+v", a, b)
	}
	other, _ := s.DB.InsertAlbum("other")
	if err := s.BindTelegramTopic(context.Background(), other, 42); err == nil {
		t.Fatal("same topic bound twice")
	}
	if err := s.BindTelegramTopic(context.Background(), other, 1); err == nil {
		t.Fatal("General accepted")
	}
}

func TestTelegramScanResumeKeepsUnvisitedHistoryCursor(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	data := workflowPNG(t)
	f.pageSize = 1
	f.failHistoryOffset = 5
	for _, m := range []struct {
		id   int
		date int64
	}{{5, 101}, {4, 100}, {3, 100}} {
		f.add(topic, m.id, m.date, "photo.png", "image/png", "", append(bytes.Clone(data), byte(m.id)))
	}
	id, err := s.QueueTelegramScan(context.Background(), album, "date", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := s.DB.GetTask(id)
	if err = s.RunTelegramScan(context.Background(), task.Payload); err == nil {
		t.Fatal("expected interrupted page")
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.QueueTelegramScan(context.Background(), album, "resume", 0, 0)
	if err != nil || resumed != id {
		t.Fatal("resume replaced interrupted task", resumed, id, err)
	}
	task, _ = s.DB.GetTask(id)
	if !strings.Contains(task.Payload, `"offset":5`) || !strings.Contains(task.Payload, `"since":100`) {
		t.Fatal("resume discarded old page/date bounds", task.Payload)
	}
	if err = s.RunTelegramScan(context.Background(), task.Payload); err != nil {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE telegram_source IS NOT NULL`).Scan(&count)
	if count != 3 {
		t.Fatal("unvisited older pages lost after resume", count)
	}
}

func TestTelegramRetiredUnregisteredSourceCannotReturn(t *testing.T) {
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	f.add(topic, 61, 100, "old.png", "image/png", "", workflowPNG(t))
	if err := s.MarkRetiredTelegram(f.messages[61].Reference); err != nil {
		t.Fatal(err)
	}
	if file, err := s.ReceiveTelegramMessage(context.Background(), f.messages[61], album); err != nil || file != nil {
		t.Fatal("retired unregistered source returned", file, err)
	}
	if f.downloadCalls != 0 {
		t.Fatal("retired data was downloaded again")
	}
}

func TestTelegramScanAcceptsVideoAndMIMEFilenamePrivately(t *testing.T) {
	for _, m := range []tguser.Message{{Name: "movie.MKV", MIME: "application/octet-stream"}, {Name: "movie.m4v"}, {Name: "movie.mov"}, {Name: "movie.webm"}, {MIME: "video/mp4"}} {
		if !isTelegramMedia(m) {
			t.Fatal("supported video ignored", m)
		}
	}
	if isTelegramMedia(tguser.Message{Name: "notes.txt", MIME: "text/plain"}) {
		t.Fatal("text treated as video")
	}
	if telegramExtension("video/quicktime") != ".mov" || telegramExtension("video/webm; codecs=vp9") != ".webm" {
		t.Fatal("video MIME extension mismatch")
	}
	s, f := workflowEnv(t)
	album, topic := workflowBound(t, s, f)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("video intake requires ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("video intake requires ffprobe")
	}
	videoPath := filepath.Join(t.TempDir(), "tiny.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=16x16:r=10", "-t", "0.3", "-an", "-c:v", "mpeg4", "-y", videoPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create valid video fixture: %v %s", err, output)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	f.add(topic, 71, 100, "", "video/mp4", "", video)
	file, err := s.ReceiveTelegramMessage(context.Background(), f.messages[71], album)
	if err != nil {
		t.Fatal(err)
	}
	if file == nil || file.Kind != "video" || file.Ext != "mp4" || file.Storage == "direct" || file.Published || file.AlbumID.Int64 != album {
		t.Fatalf("video intake was not private: %+v", file)
	}
}

type workflowStreamTG struct {
	*workflowTG
	opens, closes int
}
type workflowStreamReader struct {
	io.Reader
	close func()
}

func (r workflowStreamReader) Close() error { r.close(); return nil }
func (f *workflowStreamTG) OpenStream(_ context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	f.mu.Lock()
	data, ok := f.data[ref.MessageID]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("MESSAGE_ID_INVALID")
	}
	f.opens++
	return workflowStreamReader{Reader: bytes.NewReader(data), close: func() { f.closes++ }}, nil
}
func (f *workflowStreamTG) Download(context.Context, tguser.Reference) ([]byte, error) {
	return nil, errors.New("buffered Download must not be used by streaming scanner")
}
func (f *workflowStreamTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, input io.Reader, size int64, thumb []byte, caption string, randomID int64, _ *tguser.VideoMetadata) (tguser.Reference, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return tguser.Reference{}, err
	}
	return f.workflowTG.Upload(ctx, chat, topic, name, mime, data, thumb, caption, randomID)
}

func TestTelegramScanUsesOptionalStreamAndClosesIt(t *testing.T) {
	s, tg := workflowEnv(t)
	album, topic := workflowBound(t, s, tg)
	tg.add(topic, 72, 100, "stream.png", "image/png", "", workflowPNG(t))
	stream := &workflowStreamTG{workflowTG: tg}
	s.UserTG = stream
	file, err := s.ReceiveTelegramMessage(context.Background(), tg.messages[72], album)
	if err != nil || file == nil {
		t.Fatal("streamed intake failed", file, err)
	}
	if stream.opens != 1 || stream.closes != 1 {
		t.Fatal("stream lifecycle incorrect", stream.opens, stream.closes)
	}
	if _, err = s.ReceiveTelegramMessage(context.Background(), tg.messages[72], album); err != nil {
		t.Fatal(err)
	}
	if stream.opens != 1 {
		t.Fatal("dedup reopened remote stream")
	}
}
