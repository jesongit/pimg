package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

type runtimeFixtureClient struct {
	tguser.Transport
	mu        sync.Mutex
	st        tguser.Status
	groups    []tguser.Group
	run       func(context.Context) error
	started   chan struct{}
	startOnce sync.Once
	authCalls atomic.Int32
	video     tguser.VideoMetadata
}

func newRuntimeFixtureClient() *runtimeFixtureClient {
	return &runtimeFixtureClient{st: tguser.Status{State: "authorized", Configured: true, AccountID: 77}, groups: []tguser.Group{{ID: 41, Name: "Allowed"}}, started: make(chan struct{})}
}
func (f *runtimeFixtureClient) Status() tguser.Status                          { f.mu.Lock(); defer f.mu.Unlock(); return f.st }
func (f *runtimeFixtureClient) Groups(context.Context) ([]tguser.Group, error) { return f.groups, nil }
func (f *runtimeFixtureClient) Run(ctx context.Context) error {
	f.startOnce.Do(func() { close(f.started) })
	if f.run != nil {
		return f.run(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *runtimeFixtureClient) AuthPhone(context.Context, string) error {
	f.authCalls.Add(1)
	return nil
}
func (f *runtimeFixtureClient) AuthCode(context.Context, string) error {
	f.authCalls.Add(1)
	return nil
}
func (f *runtimeFixtureClient) AuthPassword(context.Context, string) error {
	f.authCalls.Add(1)
	return nil
}
func (f *runtimeFixtureClient) RestartAuth(context.Context) error { f.authCalls.Add(1); return nil }
func (f *runtimeFixtureClient) Logout(context.Context) error      { f.authCalls.Add(1); return nil }
func (f *runtimeFixtureClient) UploadVideo(_ context.Context, chat int64, topic int, _, _ string, data, _ []byte, _ string, _ int64, meta tguser.VideoMetadata) (tguser.Reference, error) {
	f.video = meta
	return tguser.Reference{AccountID: 77, ChatID: chat, TopicID: topic, MessageID: 9, Size: int64(len(data))}, nil
}

func runtimeFixture(t *testing.T) (*Service, *telegramRuntime) {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "pimg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	master, err := cryptox.NewMaster(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Paths: config.Paths{DataDir: dir}, Telegram: config.Telegram{APIID: 10, APIHash: strings.Repeat("a", 32), SessionFile: filepath.Join(dir, "session.enc")}}
	s := &Service{Cfg: cfg, DB: db, Master: master, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := &telegramRuntime{service: s, apiID: 10, apiHash: strings.Repeat("a", 32), client: newRuntimeFixtureClient()}
	r.newClient = func(tguser.Options) telegramUserClient { return newRuntimeFixtureClient() }
	s.telegram = r
	s.UserTG = r
	return s, r
}
func waitRuntime(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not finish")
	}
}
func stopRuntime(t *testing.T, r *telegramRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if err := r.stopLocked(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramRuntimePinsIdentityBeforeOperations(t *testing.T) {
	s, r := runtimeFixture(t)
	r.chatID = 41
	if st := r.Status(); st.State != "authorized" {
		t.Fatal(st)
	}
	pinned, _ := s.DB.GetSetting("telegram.account_id")
	if pinned != "77" {
		t.Fatal("authorized identity was not pinned", pinned)
	}
	c := r.current().(*runtimeFixtureClient)
	c.mu.Lock()
	c.st.AccountID = 999
	c.mu.Unlock()
	if st := r.Status(); st.State != "reauth_required" || st.Error == "" {
		t.Fatal("different account still appears authorized", st)
	}
	if err := s.TGReady(); err == nil {
		t.Fatal("wrong global identity is ready")
	}
	if _, err := r.Download(context.Background(), tguser.Reference{AccountID: 999, ChatID: 41, MessageID: 9}); err == nil {
		t.Fatal("wrong global identity reached provider")
	}
}

func TestTelegramFirstGroupSelectionChecksCurrentPermissionsAndStaysFixed(t *testing.T) {
	s, r := runtimeFixture(t)
	invalid := int64(42)
	if err := s.ConfigureTelegramGroup(context.Background(), invalid); err == nil {
		t.Fatal("nonmember/unprivileged group selected")
	}
	stored, _ := s.DB.GetSetting("telegram.chat_id")
	if stored != "" {
		t.Fatal("invalid selection persisted", stored)
	}
	valid := int64(41)
	if err := s.ConfigureTelegramGroup(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	if s.TGChatID() != 41 {
		t.Fatal("group not selected")
	}
	if err := s.ConfigureTelegramGroup(context.Background(), invalid); err == nil {
		t.Fatal("fixed group silently switched")
	}
	zero := int64(0)
	if err := s.ConfigureTelegramGroup(context.Background(), zero); err == nil {
		t.Fatal("fixed group cleared")
	}
	r.chatID = 41
	meta := tguser.VideoMetadata{Duration: 2, Width: 640, Height: 360, SupportsStreaming: true}
	ref, err := r.UploadVideo(context.Background(), 41, 7, "clip.mp4", "video/mp4", []byte("data"), nil, "", 1, meta)
	if err != nil || ref.Size != 4 || r.current().(*runtimeFixtureClient).video != meta {
		t.Fatal("optional video interface not forwarded", ref, err)
	}
}

func TestTelegramClientReplacementWaitsForOldRunAndSessionRelease(t *testing.T) {
	s, r := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.root = ctx
	old := r.current().(*runtimeFixtureClient)
	release := make(chan struct{})
	old.run = func(ctx context.Context) error { <-ctx.Done(); <-release; return ctx.Err() }
	var created atomic.Int32
	next := newRuntimeFixtureClient()
	r.newClient = func(tguser.Options) telegramUserClient { created.Add(1); return next }
	if err := r.launchLocked(); err != nil {
		t.Fatal(err)
	}
	waitRuntime(t, old.started)
	result := make(chan error, 1)
	go func() { result <- s.ReconnectTelegram(context.Background()) }()
	select {
	case err := <-result:
		t.Fatal("replacement did not wait for old Run", err)
	case <-time.After(80 * time.Millisecond):
	}
	if created.Load() != 0 {
		t.Fatal("second client started before final session writes")
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement blocked")
	}
	waitRuntime(t, next.started)
	stopRuntime(t, r)
}

func TestTelegramSameCredentialsCanRestartFailedRun(t *testing.T) {
	s, r := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.root = ctx
	old := r.current().(*runtimeFixtureClient)
	old.run = func(context.Context) error { return errors.New("upstream secret must not escape") }
	if err := r.launchLocked(); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	done := r.done
	r.mu.RUnlock()
	waitRuntime(t, done)
	if st := r.Status(); st.State != "error" || strings.Contains(st.Error, "secret") {
		t.Fatal(st)
	}
	next := newRuntimeFixtureClient()
	r.newClient = func(tguser.Options) telegramUserClient { return next }
	if err := s.ReconnectTelegram(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRuntime(t, next.started)
	if st := r.Status(); st.State != "authorized" {
		t.Fatal("old run error persisted", st)
	}
	stopRuntime(t, r)
}

func TestTelegramMigrationIgnoresObsoleteWebCiphertext(t *testing.T) {
	s, _ := runtimeFixture(t)
	if _, err := s.DB.Exec("INSERT INTO settings(k,v) VALUES('telegram.api_id','999'),('telegram.api_hash','obsolete-not-valid-ciphertext')"); err != nil {
		t.Fatal(err)
	}
	if err := s.initStorageConnection(); err != nil {
		t.Fatal(err)
	}
	if err := s.initTelegramRuntime(); err != nil {
		t.Fatal("obsolete web credentials should not be read or decrypted", err)
	}
	if s.telegram.apiID != s.Cfg.Telegram.APIID || s.telegram.apiHash != s.Cfg.Telegram.APIHash {
		t.Fatal("web credentials still override configuration")
	}
}

func TestTelegramCLISessionExclusiveAndExistingOnly(t *testing.T) {
	s, r := runtimeFixture(t)
	if _, err := s.ConnectTelegramSession(context.Background()); err == nil {
		t.Fatal("missing session triggered login")
	}
	if err := os.WriteFile(r.sessionFile(), []byte("fixture fake session; not used by real client"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(r.sessionFile() + ".lock")
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := s.ConnectTelegramSession(context.Background()); err == nil || !strings.Contains(err.Error(), "另一个 PIMG") {
		t.Fatal("CLI competed with active server session", err)
	}
	_ = lock.Close()
	r.chatID = 41
	closeSession, err := s.ConnectTelegramSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := r.current().(*runtimeFixtureClient)
	waitRuntime(t, c.started)
	if c.authCalls.Load() != 0 {
		t.Fatal("CLI initiated interactive authentication")
	}
	closeSession()
	lock = flock.New(r.sessionFile() + ".lock")
	ok, err = lock.TryLock()
	if err != nil || !ok {
		t.Fatal("CLI did not release OS session lock", err)
	}
	_ = lock.Close()
}

func TestTelegramCLIRejectsUnauthorizedSessionWithoutSendingCode(t *testing.T) {
	s, r := runtimeFixture(t)
	r.chatID = 41
	if err := os.WriteFile(r.sessionFile(), []byte("fake session"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newRuntimeFixtureClient()
	c.st.State = "phone"
	r.newClient = func(tguser.Options) telegramUserClient { return c }
	if _, err := s.ConnectTelegramSession(context.Background()); err == nil || !strings.Contains(err.Error(), "网页") {
		t.Fatal(err)
	}
	if c.authCalls.Load() != 0 {
		t.Fatal("CLI requested login code")
	}
}
