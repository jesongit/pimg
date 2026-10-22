package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
	"sync"
	"testing"
)

type provisionTG struct {
	*workflowTG
	groupMu               sync.Mutex
	groups                map[string]tguser.Group
	calls                 int
	loseReply, failBefore bool
}

func (f *provisionTG) FindManagedGroup(_ context.Context, marker string) (tguser.Group, bool, error) {
	f.groupMu.Lock()
	defer f.groupMu.Unlock()
	g, ok := f.groups[marker]
	return g, ok, nil
}
func (f *provisionTG) CreateManagedGroup(_ context.Context, title, marker string) (tguser.Group, error) {
	f.groupMu.Lock()
	defer f.groupMu.Unlock()
	f.calls++
	if f.failBefore {
		f.failBefore = false
		return tguser.Group{}, errors.New("FLOOD_WAIT")
	}
	g := tguser.Group{ID: int64(100 + f.calls), Name: title}
	f.groups[marker] = g
	if f.loseReply {
		f.loseReply = false
		return tguser.Group{}, errors.New("reply lost after commit")
	}
	return g, nil
}

func TestAccountGroupLostReplyRestartAndConcurrentRetry(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}, loseReply: true}
	s.UserTG = fake
	owner := "user:alpha"
	if err := s.DB.CreateAccount(owner, "alpha", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureTelegramSpace(context.Background(), owner); err == nil {
		t.Fatal("lost response reported ready")
	}
	b, err := s.DB.TelegramSpace(owner)
	if err != nil || b.State != "uncertain" {
		t.Fatal(b, err)
	}
	s.DB.Exec(`UPDATE account_telegram SET retry_after=0 WHERE owner_id=?`, owner)
	// Clear all in-memory service locks, as after a process restart. The persisted
	// create token must be sufficient to adopt the committed remote group.
	s.locks = fileLocks{}
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, e := s.EnsureTelegramSpace(context.Background(), owner)
			if e != nil {
				failures <- e
			} else if g.ChatID <= 0 {
				failures <- errors.New("missing group")
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if fake.calls != 1 {
		t.Fatal("duplicate remote group creation", fake.calls)
	}
	legacy, err := s.EnsureTelegramSpace(context.Background(), dbx.AdminOwner)
	if err != nil || legacy.ChatID != 77 || fake.calls != 1 {
		t.Fatal("legacy group was replaced", legacy, err)
	}
}

func TestAccountGroupAmbiguousAbsenceNeverBlindlyRecreates(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}, loseReply: true}
	s.UserTG = fake
	s.DB.CreateAccount("user:alpha", "alpha", "hash")
	s.EnsureTelegramSpace(context.Background(), "user:alpha")
	fake.groups = map[string]tguser.Group{}
	s.DB.Exec(`UPDATE account_telegram SET retry_after=0`)
	for range 3 {
		if _, err := s.EnsureTelegramSpace(context.Background(), "user:alpha"); err == nil {
			t.Fatal("unknown commit silently retried")
		}
	}
	if fake.calls != 1 {
		t.Fatal("uncertain create repeated", fake.calls)
	}
}

func TestAccountGroupKnownFailureRetriesAndGroupsRemainSeparate(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}, failBefore: true}
	s.UserTG = fake
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnsureTelegramSpace(context.Background(), "user:alpha"); err == nil {
		t.Fatal("rate limit accepted")
	}
	b, _ := s.DB.TelegramSpace("user:alpha")
	if b.State != "failed" {
		t.Fatal(b)
	}
	if _, err := s.EnsureTelegramSpace(context.Background(), "user:alpha"); err == nil || fake.calls != 1 {
		t.Fatal("rate limit ignored")
	}
	s.DB.Exec(`UPDATE account_telegram SET retry_after=0`)
	a, err := s.EnsureTelegramSpace(context.Background(), "user:alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err = s.EnsureTelegramSpace(context.Background(), "user:bravo")
	if err != nil || a.ChatID == b.ChatID {
		t.Fatal(a, b, err)
	}
	if !s.DB.TelegramChatAllowed(11, a.ChatID) || s.DB.TelegramChatAllowed(12, a.ChatID) {
		t.Fatal("TG connection ownership lost")
	}
}

func TestAccountTGObjectKeepsRemoteIdentity(t *testing.T) {
	s, _ := workflowEnv(t)
	ref := tguser.Reference{AccountID: 11, ChatID: 123, TopicID: 9, MessageID: 7}
	key := encodeTelegramObject(telegramObject{Reference: ref})
	s.Cfg.Telegram.ChatID = 999
	obj, err := s.telegramObject(key)
	if err != nil || obj.ChatID != 123 {
		t.Fatal(obj, err)
	}
}

func TestAccountTGUploadUsesOwnDefaultAlbumAndGroup(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}
	s.UserTG = fake
	owner := "user:alpha"
	if err := s.DB.CreateAccount(owner, "alpha", "hash"); err != nil {
		t.Fatal(err)
	}
	space, err := s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.receiveLegacyUploadFixture(dbx.WithOwner(context.Background(), owner), bytes.NewReader(workflowPNG(t)), "test.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.GetAlbum(file.AlbumID.Int64)
	if err != nil || album.OwnerID != owner {
		t.Fatal("foreign default album", album, err)
	}
	if err = s.RunArchive(context.Background(), file.ID); err != nil {
		t.Fatal(err)
	}
	chunks, err := s.DB.ListChunks(file.ID, "data")
	if err != nil || len(chunks) != 1 {
		t.Fatal(chunks, err)
	}
	object, err := s.telegramObject(chunks[0].ObjectKey)
	if err != nil || object.ChatID != space.ChatID {
		t.Fatal("wrong account group", object, err)
	}
}

func TestAccountPendingTGCanUseAnotherArchive(t *testing.T) {
	s, _ := workflowEnv(t)
	owner := "user:alpha"
	s.DB.CreateAccount(owner, "alpha", "hash")
	policy := s.StoragePolicy()
	policy.Archives = []string{"telegram", "local"}
	policy.Permanent = "local"
	policy.Temporary = "local"
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	file, err := s.receiveLegacyUploadFixture(dbx.WithOwner(context.Background(), owner), bytes.NewReader(workflowPNG(t)), "local.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil || file.ColdBackend != "local" {
		t.Fatal(file, err)
	}
	if err = s.RunArchive(context.Background(), file.ID); err != nil {
		t.Fatal(err)
	}
	policy.Archives = []string{"telegram"}
	if err = s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if _, err = s.receiveLegacyUploadFixture(dbx.WithOwner(context.Background(), owner), bytes.NewReader(workflowPNG(t)), "waiting.png", "png", sql.NullInt64{}, nil, 0, false); err == nil {
		t.Fatal("upload accepted without an available archive")
	}
}

func TestLocalArchiveCompletesBeforeTelegramCatchup(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "encrypted"
		if direct {
			name = "plaintext"
		}
		t.Run(name, func(t *testing.T) {
			s, transport := workflowEnv(t)
			policy := s.StoragePolicy()
			// Local wins even when Telegram appears first in the configured list.
			policy.Archives = []string{"telegram", "local"}
			policy.Permanent, policy.Temporary = "local", "local"
			policy.NormalMB, policy.AcceleratedMB = 1024, 1024
			if err := s.SaveStoragePolicy(policy); err != nil {
				t.Fatal(err)
			}
			plain := workflowPNG(t)
			ctx := WithPrivateUpload(context.Background())
			file, err := s.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "photo.png", "png", sql.NullInt64{}, nil, 0, direct)
			if err != nil || file.ColdBackend != "local" {
				t.Fatal("intake did not select local archive", file, err)
			}
			if direct {
				err = s.RunDirectArchive(ctx, file.ID)
			} else {
				err = s.RunArchive(ctx, file.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			file, err = s.DB.GetFile(file.ID)
			if err != nil || file.ArchiveState != "done" || file.Published || len(transport.data) != 0 {
				t.Fatal("initial success waited for TG or published private content", file, err)
			}
			var queued int
			if err = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=? AND file_id=? AND state='pending'`, dbx.TaskArchiveSync, file.ID).Scan(&queued); err != nil || queued != 1 {
				t.Fatal("missing durable catchup task", queued, err)
			}
			if err = s.RunArchiveSync(ctx, file.ID); err != nil {
				t.Fatal(err)
			}
			if len(transport.data) == 0 {
				t.Fatal("catchup did not upload to TG")
			}
			for _, target := range policy.Archives {
				reader, err := s.SourceStream(context.WithValue(ctx, archiveSourceContext{}, target), file)
				if err != nil {
					t.Fatal(target, err)
				}
				restored, readErr := io.ReadAll(reader)
				reader.Close()
				if readErr != nil || !bytes.Equal(restored, plain) {
					t.Fatal("replica failed independent recovery", target, readErr)
				}
			}
		})
	}
}

type provisionStreamTG struct{ *provisionTG }

func (f *provisionStreamTG) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	data, err := f.workflowTG.Download(ctx, ref)
	return io.NopCloser(bytes.NewReader(data)), err
}
func (f *provisionStreamTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, input io.Reader, size int64, thumb []byte, caption string, random int64, video *tguser.VideoMetadata) (tguser.Reference, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return tguser.Reference{}, err
	}
	return f.workflowTG.Upload(ctx, chat, topic, name, mime, data, thumb, caption, random)
}

func TestAccountStreamingArchiveUsesAllocatedGroup(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &provisionStreamTG{&provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}}
	s.UserTG = fake
	s.DB.CreateAccount("user:alpha", "alpha", "hash")
	space, err := s.EnsureTelegramSpace(context.Background(), "user:alpha")
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.receiveLegacyUploadFixture(dbx.WithOwner(context.Background(), "user:alpha"), bytes.NewReader(workflowPNG(t)), "stream.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("stream payload")
	ref, err := (topicCold{s: s, instance: "telegram"}).PutReader(context.Background(), originalArchiveKey(file), bytes.NewReader(payload), int64(len(payload)), "")
	if err != nil {
		t.Fatal(err)
	}
	object, err := decodeTelegramObject(ref.Key)
	if err != nil || object.ChatID != space.ChatID || object.ChatID == 77 {
		t.Fatal("stream sent to global group", object, err)
	}
	// Selecting TG requires an authorized connection, and never a manually pinned group.
	s.Cfg.Telegram.ChatID = 0
	for _, instance := range s.StorageInstances() {
		if instance.Driver == "telegram" && !instance.Configured {
			t.Fatal("automatic TG storage cannot be selected")
		}
	}
}
