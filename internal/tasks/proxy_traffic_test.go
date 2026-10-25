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
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/proxytraffic"
	"pimg/internal/storage"
	"pimg/internal/store"
	"pimg/internal/tgx"
)

type quotaReplica struct {
	storage.Replica
	paused bool
	writes map[string]int
}

func (q *quotaReplica) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
	if q.paused && len(q.writes) > 0 {
		return nil, proxytraffic.ErrLimit
	}
	return q.Replica.Open(ctx, ref)
}

func (q *quotaReplica) Put(ctx context.Context, album, file string, o storage.ReplicaObject, input io.Reader) (string, error) {
	if q.paused && len(q.writes) > 0 {
		return "", proxytraffic.ErrLimit
	}
	ref, err := q.Replica.Put(ctx, album, file, o, input)
	if err == nil {
		q.writes[ref]++
	}
	return ref, err
}

func TestProxyQuotaTaskResumesCompletedObjectsWithoutRetryPenalty(t *testing.T) {
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}}}
	svc, err := store.New(cfg, db, master, tgx.New("", "", ""), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(db, svc, 1, log)
	defer engine.Stop()
	var data bytes.Buffer
	_ = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 24, 24)))
	f, err := legacyUploadFixture(svc, context.Background(), &data, "quota.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimTask(time.Now().Unix(), 60)
	if err != nil {
		t.Fatal(err)
	}
	engine.run(job)
	q := &quotaReplica{Replica: storage.LocalReplica{DataDir: dir, Directory: "replicas/quota"}, paused: true, writes: map[string]int{}}
	if err := svc.RegisterReplicaDriver(store.ReplicaDriver{ID: "quota", Name: "synthetic quota", Validate: func(json.RawMessage) (string, error) { return "{}", nil }, Open: func(context.Context, string) (storage.Replica, error) { return q, nil }}); err != nil {
		t.Fatal(err)
	}
	repo, err := svc.AddColdRepository("quota", "quota", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetColdRepositoryEnabled(repo, true)
	if err := db.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	job, err = db.ClaimIdleTask(time.Now().Unix(), 60)
	if err != nil {
		t.Fatal(err)
	}
	v := svc.ProxyTrafficView()
	used := v.DailyLimit
	if err := svc.SaveProxyTraffic(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	engine.run(job)
	waiting, _ := db.GetTask(job.ID)
	if waiting.State != "pending" || waiting.Attempts != 0 || waiting.RunAfter <= time.Now().Unix() {
		t.Fatal("quota was treated as failure", waiting)
	}
	if len(q.writes) != 1 {
		t.Fatal("fixture did not pause midway", q.writes)
	}
	q.paused = false
	limit := v.DailyLimit + 1
	if err := svc.SaveProxyTraffic(proxytraffic.Patch{DailyLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	job, err = db.ClaimIdleTask(time.Now().Unix(), 60)
	if err != nil {
		t.Fatal("not eligible after correction", err)
	}
	engine.run(job)
	finished, _ := db.GetTask(job.ID)
	if finished.State != "done" {
		t.Fatal("resume failed", finished)
	}
	for ref, n := range q.writes {
		if n != 1 {
			t.Fatal("completed object retransferred", ref, n)
		}
	}
	receipt, err := db.GetColdSyncJob(repo, f.ID)
	var complete bool
	if err == nil {
		err = db.QueryRow(`SELECT done_revision=wanted_revision AND done_revision>0 FROM cold_sync_files WHERE repository_id=? AND file_id=?`, repo, f.ID).Scan(&complete)
	}
	if err != nil || !complete {
		t.Fatal("replica not completed", receipt, err)
	}
}
