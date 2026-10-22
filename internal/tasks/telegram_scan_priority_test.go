package tasks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"pimg/internal/store"
	"pimg/internal/tguser"
	"pimg/internal/tgx"
)

type scanPriorityTG struct {
	tguser.Transport
	mu           sync.Mutex
	topics       map[int]tguser.Topic
	media        []tguser.Message
	data         []byte
	closeTopic   int
	closeStarted chan context.Context
	release      chan struct{}
}

func (f *scanPriorityTG) Status() tguser.Status {
	return tguser.Status{State: "authorized", AccountID: 11, Configured: true}
}
func (f *scanPriorityTG) Topics(context.Context, int64) ([]tguser.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tguser.Topic
	for _, topic := range f.topics {
		out = append(out, topic)
	}
	return out, nil
}
func (f *scanPriorityTG) History(_ context.Context, _ int64, topic int, _, _ time.Time, _, _ int) (tguser.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	page := tguser.Page{Done: true}
	for _, m := range f.media {
		if m.Reference.TopicID == topic {
			page.Messages = append(page.Messages, m)
		}
	}
	return page, nil
}
func (f *scanPriorityTG) Download(context.Context, tguser.Reference) ([]byte, error) {
	return bytes.Clone(f.data), nil
}
func (f *scanPriorityTG) CloseTopic(ctx context.Context, _ int64, topic int, _ bool) error {
	if topic != f.closeTopic {
		return nil
	}
	select {
	case f.closeStarted <- ctx:
	default:
	}
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *scanPriorityTG) DeleteTopic(_ context.Context, _ int64, topic int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.topics, topic)
	return nil
}

type scanHeldReplica struct {
	storage.Replica
	started chan context.Context
	release chan struct{}
}

func (r *scanHeldReplica) Put(ctx context.Context, album, file string, object storage.ReplicaObject, input io.Reader) (string, error) {
	select {
	case r.started <- ctx:
	default:
	}
	select {
	case <-r.release:
		return r.Replica.Put(ctx, album, file, object, input)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func scanPriorityEngine(t *testing.T) (*Engine, *scanPriorityTG, int64) {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Telegram: config.Telegram{ChatID: 77}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	// The injected unused legacy transport disables real account startup. All
	// media and replica operations remain local/in-memory in this fixture.
	svc, err := store.New(cfg, db, master, tgx.New("", "", ""), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	remote := &scanPriorityTG{topics: map[int]tguser.Topic{}, closeStarted: make(chan context.Context, 1), release: make(chan struct{})}
	svc.UserTG = remote
	engine := New(db, svc, 1, log)
	t.Cleanup(engine.Stop)
	bind := func(name string, topic int, isDefault bool) int64 {
		t.Helper()
		var id int64
		if isDefault {
			id, err = db.EnsureDefaultAlbum()
		} else {
			id, err = db.InsertAlbum(name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = db.InsertTopicBinding(&dbx.TopicBinding{AlbumID: id, AccountID: 11, ChatID: 77, TopicID: topic, Name: name, CreateName: name, IsDefault: isDefault, State: "active"}); err != nil {
			t.Fatal(err)
		}
		remote.topics[topic] = tguser.Topic{ID: topic, Name: name}
		return id
	}
	album := bind("probe", 101, false)
	bind("Uncategorized", 102, true)
	return engine, remote, album
}

func startSlowIdle(t *testing.T, engine *Engine, remote *scanPriorityTG, typ string) (context.Context, <-chan struct{}, int64, func()) {
	t.Helper()
	var started <-chan context.Context
	var release chan struct{}
	if typ == dbx.TaskColdSync {
		var encoded bytes.Buffer
		_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 24, 24)))
		if _, err := legacyUploadFixture(engine.svc,context.Background(), &encoded, "held.png", "png", sql.NullInt64{}, nil, 0, false); err != nil {
			t.Fatal(err)
		}
		archive, err := engine.db.ClaimTask(time.Now().Unix(), 100)
		if err != nil {
			t.Fatal(err)
		}
		engine.run(archive)
		held := &scanHeldReplica{Replica: storage.LocalReplica{DataDir: engine.svc.Cfg.Paths.DataDir, Directory: "replicas/held-scan"}, started: make(chan context.Context, 1), release: make(chan struct{})}
		if err = engine.svc.RegisterReplicaDriver(store.ReplicaDriver{ID: "scan-held", Name: "synthetic adapter", Validate: func(json.RawMessage) (string, error) { return "{}", nil }, Open: func(context.Context, string) (storage.Replica, error) { return held, nil }}); err != nil {
			t.Fatal(err)
		}
		repo, err := engine.svc.AddColdRepository("held", "scan-held", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.db.SetColdRepositoryEnabled(repo, true); err != nil {
			t.Fatal(err)
		}
		if err = engine.db.ReconcileColdSync(); err != nil {
			t.Fatal(err)
		}
		started, release = held.started, held.release
	} else {
		album, err := engine.db.InsertAlbum("deleting")
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.db.InsertTopicBinding(&dbx.TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 103, Name: "deleting", CreateName: "deleting", State: "active"}); err != nil {
			t.Fatal(err)
		}
		remote.topics[103] = tguser.Topic{ID: 103, Name: "deleting"}
		remote.closeTopic = 103
		if _, err = engine.svc.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
			t.Fatal(err)
		}
		started, release = remote.closeStarted, remote.release
	}
	job, err := engine.db.ClaimIdleTask(time.Now().Unix(), 100)
	if err != nil || job.Type != typ {
		t.Fatal("idle task was not claimable", job, err)
	}
	done := make(chan struct{})
	go func() { defer close(done); engine.run(job) }()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("held idle task did not stop")
		}
	})
	select {
	case ctx := <-started:
		return ctx, done, job.ID, stop
	case <-time.After(3 * time.Second):
		t.Fatal("idle task did not reach slow operation")
		return nil, nil, 0, stop
	}
}

func TestAutomaticTelegramProbeDoesNotInterruptSlowIdleTasks(t *testing.T) {
	for _, typ := range []string{dbx.TaskColdSync, dbx.TaskTelegramDeleteAlbum} {
		t.Run(typ, func(t *testing.T) {
			engine, remote, album := scanPriorityEngine(t)
			slow, done, idleID, release := startSlowIdle(t, engine, remote, typ)
			for i := 0; i < 3; i++ {
				id, err := engine.svc.QueueAutomaticTelegramScan(context.Background(), album)
				if err != nil {
					t.Fatal(err)
				}
				probe, err := engine.db.ClaimTask(time.Now().Unix(), 100)
				if err != nil || probe.ID != id || !dbx.PassiveTask(probe) {
					t.Fatal("automatic scan not claimed", probe, err)
				}
				engine.run(probe)
				state, _ := engine.db.GetTask(id)
				if state.State != "done" || slow.Err() != nil {
					t.Fatal("automatic empty probe preempted idle task", state.State, slow.Err())
				}
			}
			state, _ := engine.db.GetTask(idleID)
			if state.State != "running" || state.Attempts != 1 {
				t.Fatal("idle task was silently postponed or retried", state.State, state.Attempts)
			}
			release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("idle task did not complete after release")
			}
			state, _ = engine.db.GetTask(idleID)
			if state.State != "done" {
				t.Fatal("idle task failed", state.State, state.LastError.String)
			}
		})
	}
}

func TestManualTelegramScanAndAutomaticNewMediaStillPreemptIdle(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual-empty-scan", true: "automatic-new-media"}[automatic], func(t *testing.T) {
			engine, remote, album := scanPriorityEngine(t)
			slow, done, idleID, _ := startSlowIdle(t, engine, remote, dbx.TaskColdSync)
			var id int64
			var err error
			if automatic {
				var data bytes.Buffer
				_ = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 24, 24)))
				remote.data = data.Bytes()
				remote.media = []tguser.Message{{Reference: tguser.Reference{AccountID: 11, ChatID: 77, TopicID: 101, MessageID: 50, DocumentID: 50, Size: int64(data.Len())}, Date: time.Now().Unix(), Name: "new.png", MIME: "image/png"}}
				id, err = engine.svc.QueueAutomaticTelegramScan(context.Background(), album)
			} else {
				id, err = engine.svc.QueueTelegramScan(context.Background(), album, "latest", 0, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			scan, err := engine.db.ClaimTask(time.Now().Unix(), 100)
			if err != nil || scan.ID != id {
				t.Fatal("scan not claimed", scan, err)
			}
			engine.run(scan)
			if slow.Err() != context.Canceled {
				t.Fatal("foreground work failed to cancel idle operation")
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("preempted idle task did not stop")
			}
			state, _ := engine.db.GetTask(idleID)
			if state.State != "pending" || state.Attempts != 0 {
				t.Fatal("preempted task lost retry semantics", state.State, state.Attempts)
			}
			if automatic {
				if _, err = engine.db.FileByTelegramSource("11:77:50"); err != nil {
					t.Fatal("new media ingestion did not retain source identity", err)
				}
			}
		})
	}
}
