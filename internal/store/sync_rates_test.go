package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestSyncCopyLimitsAreIndependentOfNetworkSchedule(t *testing.T) {
	s, peer, _ := peerFixture(t)
	p := s.StoragePolicy()
	p.NormalMB, p.AcceleratedMB, p.LocalMB = 1, 8, 64
	p.Start, p.End = "23:00", "06:00"
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	for _, hour := range []int{0, 6, 12, 23} {
		now := time.Date(2026, 10, 23, hour, 0, 0, 0, zone)
		for _, instance := range []string{"local", peer, "r2", "telegram", "unknown"} {
			r := s.syncCopyReader(context.Background(), bytes.NewReader(nil), instance).(*pacedReader)
			want := 1
			if hour < 6 || hour >= 23 {
				want = 8
			}
			if instance == "local" || instance == peer {
				want = 64
			}
			if got := r.rateMiB(now); got != want {
				t.Fatalf("%s at %d: rate %d want %d", instance, hour, got, want)
			}
		}
	}
	// Local validation reads/hashes and must not inherit either copy/network cap.
	ctx, cancel := context.WithCancel(context.Background())
	r := s.syncVerifyReader(ctx, bytes.NewReader([]byte{1}), peer)
	if _, ok := r.(*pacedReader); ok {
		t.Fatal("local validation was treated as copying")
	}
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("local validation lost cancellation", err)
	}
	if _, ok := s.syncVerifyReader(context.Background(), bytes.NewReader(nil), "r2").(*pacedReader); !ok {
		t.Fatal("remote validation bypassed network limit")
	}
}

func TestLocalCopyLimitCancelsAndResumesVerifiedMedia(t *testing.T) {
	s, peer, plain := peerFixture(t)
	p := s.StoragePolicy()
	p.Archives = []string{"local", peer}
	p.NormalMB, p.AcceleratedMB, p.LocalMB = 1024, 1024, 1
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	plain = append(plain, bytes.Repeat([]byte{0x4f}, 512<<10)...)
	f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(plain), "copy.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err = s.RunArchiveSync(ctx, f.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("local copy ignored its independent 1 MiB/s limit", err)
	}
	p.LocalMB = 64
	if err = s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal("retry did not complete", err)
	}
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil || len(replicas) != 2 {
		t.Fatal(len(replicas), err)
	}
	for _, replica := range replicas {
		if replica.State != "ready" || len(replica.Parts) != 1 || !replica.Parts[0].Verified {
			t.Fatal("unverified copy", replica)
		}
		r, err := s.openMediaPart(context.Background(), f, replica, replica.Parts[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatal("resumed copy changed original bytes", err)
		}
	}
}

func TestRemoteSourceToLocalCopyKeepsNetworkLimit(t *testing.T) {
	s, peer, _ := peerFixture(t)
	p := s.StoragePolicy()
	p.NormalMB, p.AcceleratedMB, p.LocalMB = 1, 1, 1024
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	data := bytes.Repeat([]byte{7}, 512<<10)
	// The large streaming path wraps both endpoints. A local destination may
	// be fast without removing the remote source's independently paced read.
	remote := s.syncCopyReader(ctx, bytes.NewReader(data), "r2")
	copy := s.syncCopyReader(ctx, remote, peer)
	n, err := io.Copy(io.Discard, copy)
	if !errors.Is(err, context.DeadlineExceeded) || n >= int64(len(data)) {
		t.Fatal("local destination bypassed network source limit", n, err)
	}
}

func TestLocalCopyPolicyBackwardCompatibilityAndLiveUpdate(t *testing.T) {
	s, _, _ := peerFixture(t)
	p := s.StoragePolicy()
	if p.LocalMB != 64 {
		t.Fatal("unexpected default local copy limit", p.LocalMB)
	}
	p.LocalMB = 37
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	reader := s.localCopyReader(context.Background(), bytes.NewReader(nil)).(*pacedReader)
	// A pre-upgrade client cannot clear a saved value by omitting the new field.
	p.LocalMB = 0
	if err := s.SaveStoragePolicy(p); err != nil || s.StoragePolicy().LocalMB != 37 {
		t.Fatal("old client reset local copy limit", err)
	}
	if _, err := s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	p.LocalMB = 12
	if err = s.SaveStoragePolicy(p); err != nil || idle.Err() != context.Canceled || reader.rateMiB(time.Now()) != 12 {
		t.Fatal("policy update was not live", err, idle.Err())
	}
	raw, err := s.DB.GetSetting("storage.policy")
	var saved StoragePolicy
	if err != nil || json.Unmarshal([]byte(raw), &saved) != nil || saved.LocalMB != 12 || saved.NormalMB != p.NormalMB {
		t.Fatal("local/network policy was not persisted separately", saved, err)
	}
	for _, invalid := range []int{-1, 1025} {
		p.LocalMB = invalid
		if err = s.SaveStoragePolicy(p); err == nil || s.StoragePolicy().LocalMB != 12 {
			t.Fatal("invalid local copy limit changed saved state", invalid, err)
		}
	}
}
