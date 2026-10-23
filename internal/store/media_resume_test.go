package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type resumeCold struct {
	storage.Cold
	reads, writes map[string]int
	failWrite     string
	failRead      string
}

func (b *resumeCold) Put(ctx context.Context, key string, data []byte, caption string) (storage.Reference, error) {
	b.writes[key]++
	if key == b.failWrite {
		return storage.Reference{}, context.Canceled
	}
	return b.Cold.Put(ctx, key, data, caption)
}

func (b *resumeCold) Open(ctx context.Context, ref storage.Reference) (io.ReadCloser, error) {
	b.reads[ref.Key]++
	if ref.Key == b.failRead {
		return nil, context.Canceled
	}
	return b.Cold.Open(ctx, ref)
}

func TestMediaResumeReusesDurableVerifiedParts(t *testing.T) {
	for _, remoteInput := range []bool{false, true} {
		t.Run(map[bool]string{false: "local-source", true: "remote-source"}[remoteInput], func(t *testing.T) {
			s, _, plain := peerFixture(t)
			ctx := context.Background()
			f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "resume.png", "png", sql.NullInt64{}, nil, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			// Two independent inputs exercise continuation after a completed part.
			// Real video splitting and reconstruction are covered by media tests.
			part := dbx.MediaPart{Ext: f.Ext, MIME: f.Mime, Size: f.Size, SHA256: f.SHA256}
			inputs := []mediaInput{{path: s.plainPath(f.ID), part: part}, {path: s.plainPath(f.ID), part: part}}
			work := t.TempDir()
			if remoteInput {
				if err = s.writeMediaReplica(ctx, f, "local", inputs, work); err != nil {
					t.Fatal(err)
				}
				for i := range inputs {
					inputs[i].remote = true
				}
			}
			i, _ := s.instance("r2")
			b := &resumeCold{Cold: i.cold, reads: map[string]int{}, writes: map[string]int{}}
			i.cold = b
			r, _ := s.mediaReplica(f, "r2")
			first := mediaObjectKey(f, r, dbx.MediaPart{Index: 0, Ext: f.Ext})
			second := mediaObjectKey(f, r, dbx.MediaPart{Index: 1, Ext: f.Ext})
			firstRead, secondRead := "archives/"+first, "archives/"+second
			b.failWrite = second
			if err = s.writeMediaReplica(ctx, f, "r2", inputs, work); !errors.Is(err, context.Canceled) {
				t.Fatal("expected interruption on second part", err)
			}
			if b.reads[firstRead] != 1 || b.writes[first] != 1 {
				t.Fatal("first write must be fully verified", b.reads, b.writes)
			}
			if err = s.writeMediaReplica(ctx, f, "r2", inputs[:1], work); err == nil {
				t.Fatal("changed layout reused an incompatible receipt")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err = s.writeMediaReplica(canceled, f, "r2", inputs, work); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled retry continued", err)
			}
			if b.reads[firstRead] != 1 || b.writes[first] != 1 {
				t.Fatal("invalid retries touched completed parts")
			}
			// Reopen SQLite to prove reuse comes from durable progress.
			if err = s.DB.Close(); err != nil {
				t.Fatal(err)
			}
			s.DB, err = dbx.Open(filepath.Join(s.Cfg.Paths.DataDir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.DB.Close() })
			b.failWrite = ""
			if err = s.writeMediaReplica(ctx, f, "r2", inputs, work); err != nil {
				t.Fatal(err)
			}
			if b.reads[firstRead] != 1 || b.writes[first] != 1 || b.reads[secondRead] != 1 || b.writes[second] != 2 {
				t.Fatal("retry repeated completed IO or skipped new verification", b.reads, b.writes)
			}
			r, _ = s.mediaReplica(f, "r2")
			if r.State != "ready" || len(r.Parts) != 2 || !r.Parts[0].Verified || !r.Parts[1].Verified {
				t.Fatal("incomplete replica published", r)
			}
			// An explicit verification still reads every part.
			for _, p := range r.Parts {
				if err = s.verifyMediaPart(ctx, f, r, p); err != nil {
					t.Fatal(err)
				}
			}
			if b.reads[firstRead] != 2 || b.reads[secondRead] != 2 {
				t.Fatal("explicit verification was skipped", b.reads)
			}
		})
	}
}

func TestMediaResumeLocalStampAndCorruption(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plaintext", true: "encrypted"}[encrypted], func(t *testing.T) {
			s, peer, plain := peerFixture(t)
			i, _ := s.instance(peer)
			cfg := i.config
			cfg.Encrypt = encrypted
			if _, err := s.SaveStorageInstance(peer, i.row.Name, "local", cfg, true); err != nil {
				t.Fatal(err)
			}
			i, _ = s.instance(peer)
			root := i.cold.(storage.LocalCold).Disk.Dir
			counted := &countedCold{Cold: i.cold}
			i.cold = counted
			f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(plain), "local.png", "png", sql.NullInt64{}, nil, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := s.mediaInputs(context.Background(), f, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err = s.writeMediaReplica(context.Background(), f, peer, inputs, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			r, _ := s.mediaReplica(f, peer)
			p := r.Parts[0]
			if p.LocalModNS == 0 || !p.Verified {
				t.Fatal("new local receipt lacks verification stamp", p)
			}
			// A cache hit opens/stats the file but does not consume its contents.
			before := counted.reads.Load()
			if err = s.reuseVerifiedMediaPart(context.Background(), f, r, p); err != nil {
				t.Fatal(err)
			}
			if counted.reads.Load()-before != 1 {
				t.Fatal("unchanged local part was fully reread")
			}
			// Upgrade receipt without a stamp: validate once, then persist it.
			p.LocalModNS = 0
			before = counted.reads.Load()
			if err = s.reuseVerifiedMediaPart(context.Background(), f, r, p); err != nil {
				t.Fatal(err)
			}
			if counted.reads.Load()-before != 4 {
				t.Fatal("legacy receipt did not receive full verification")
			}
			r, _ = s.mediaReplica(f, peer)
			p = r.Parts[0]
			if p.LocalModNS == 0 {
				t.Fatal("legacy stamp not persisted")
			}
			path := filepath.Join(root, filepath.FromSlash(mediaObjectKey(f, r, p)))
			wire, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wire[len(wire)-1] ^= 1
			if err = os.WriteFile(path, wire, 0600); err != nil {
				t.Fatal(err)
			}
			changed := time.Unix(0, p.LocalModNS).Add(time.Second)
			if err = os.Chtimes(path, changed, changed); err != nil {
				t.Fatal(err)
			}
			if err = s.reuseVerifiedMediaPart(context.Background(), f, r, p); err == nil {
				t.Fatal("changed local bytes bypassed verification")
			}
		})
	}
}

func TestMediaResumeNeverTrustsInterruptedVerification(t *testing.T) {
	s, _, plain := peerFixture(t)
	ctx := context.Background()
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "unverified.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := s.mediaInputs(ctx, f, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance("r2")
	b := &resumeCold{Cold: i.cold, reads: map[string]int{}, writes: map[string]int{}}
	i.cold = b
	r, _ := s.mediaReplica(f, "r2")
	key := mediaObjectKey(f, r, dbx.MediaPart{Ext: f.Ext})
	b.failRead = "archives/" + key
	if err = s.writeMediaReplica(ctx, f, "r2", inputs, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal("expected interrupted verification", err)
	}
	r, _ = s.mediaReplica(f, "r2")
	if r.State == "ready" || len(r.Parts) != 1 || r.Parts[0].Verified {
		t.Fatal("unverified bytes became a receipt", r)
	}
	b.failRead = ""
	if err = s.writeMediaReplica(ctx, f, "r2", inputs, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if b.reads["archives/"+key] != 2 {
		t.Fatal("retry skipped incomplete verification", b.reads)
	}
}
