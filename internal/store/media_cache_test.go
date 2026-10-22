package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type countedCold struct {
	storage.Cold
	reads  atomic.Int32
	gate   <-chan struct{}
	broken bool
}

func (c *countedCold) Open(ctx context.Context, ref storage.Reference) (io.ReadCloser, error) {
	c.reads.Add(1)
	if c.broken {
		return nil, errors.New("primary offline")
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.Cold.Open(ctx, ref)
}

func cachedFixture(t *testing.T, direct bool) (*Service, *dbx.File, []byte) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	t.Cleanup(s.StopMediaLoads)
	if err := s.SetStorage("local", "r2"); err != nil {
		t.Fatal(err)
	}
	data := bigPNG(t)
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(data), "preview.png", "png", sql.NullInt64{}, nil, 0, direct)
	if err != nil {
		t.Fatal(err)
	}
	if direct {
		err = s.RunDirectArchive(context.Background(), f.ID)
	} else {
		err = s.RunArchive(context.Background(), f.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, data
}

func TestOriginalCacheCoalescesVerifiedReadsAndExpiresWithoutPreviewLoss(t *testing.T) {
	s, f, data := cachedFixture(t, false)
	preview, err := os.ReadFile(s.previewPath(f))
	if err != nil || len(preview) == 0 {
		t.Fatal("compressed local preview missing", err)
	}
	file, err := s.CachedOriginal(f)
	if err != nil {
		t.Fatal("verified upload was not reused", err)
	}
	file.Close()
	if s.OriginalCacheHours() != 24 {
		t.Fatal("cache default must be one day")
	}
	os.Remove(s.originalCachePath(f))
	gate := make(chan struct{})
	cold := &countedCold{Cold: s.coldBackends["local"], gate: gate}
	s.coldBackends["local"] = cold
	for i := 0; i < 20; i++ {
		if v, err := s.PrepareOriginal(context.Background(), f, false); err != nil || v.Status != "loading" || v.Percent == 100 {
			t.Fatal("unverified original exposed", v, err)
		}
	}
	close(gate)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.ViewOriginal(context.Background(), f)
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Error("cached original differs", err)
			}
		}()
	}
	wg.Wait()
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	if int(cold.reads.Load()) != len(chunks) {
		t.Fatal("duplicate original loads", cold.reads.Load(), len(chunks))
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(s.originalCachePath(f), past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CachedOriginal(f); err == nil {
		t.Fatal("expired cache reused")
	}
	if err := s.CleanupMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.originalCachePath(f)); !os.IsNotExist(err) {
		t.Fatal("expired original remains", err)
	}
	if got, err := s.PreviewThumbnail(context.Background(), f); err != nil || !bytes.Equal(got, preview) {
		t.Fatal("original TTL removed preview", err)
	}
	// A completed in-memory receipt must not revive the expired disk entry.
	r, err := s.ViewOriginal(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if int(cold.reads.Load()) != 2*len(chunks) {
		t.Fatal("expired original was not reloaded")
	}
}

func TestOriginalCacheFallsBackAcrossLocalOutageAndR2Corruption(t *testing.T) {
	s, f, data := cachedFixture(t, true)
	s.ForgetMediaViews(f)
	cold := &countedCold{Cold: s.coldBackends["local"], broken: true}
	s.coldBackends["local"] = cold
	r, err := s.ViewOriginal(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) || cold.reads.Load() > 1 {
		t.Fatal("healthy R2 copy did not replace unavailable local archive", err)
	}
	previousReads := cold.reads.Load()
	s.ForgetMediaViews(f)
	cold.broken = false
	if err := s.R2.PutObject(context.Background(), s.R2.PermBucket, f.PublishedPath.String, bytes.NewReader([]byte("corrupt")), 7, f.Mime, ""); err != nil {
		t.Fatal(err)
	}
	r, err = s.ViewOriginal(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) || cold.reads.Load() <= previousReads {
		t.Fatal("bad R2 copy was exposed or primary fallback failed", err)
	}
}

func TestConversionCancelsLoadAndRevokesOldCachedIdentity(t *testing.T) {
	s, f, _ := cachedFixture(t, false)
	s.ForgetMediaViews(f)
	gate := make(chan struct{})
	cold := &countedCold{Cold: s.coldBackends["local"], gate: gate}
	s.coldBackends["local"] = cold
	if _, err := s.PrepareOriginal(context.Background(), f, false); err != nil {
		t.Fatal(err)
	}
	s.media.mu.Lock()
	job := s.media.jobs[mediaKey(f)]
	s.media.mu.Unlock()
	if _, err := s.QueueConversion(context.Background(), f.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatal("conversion did not cancel old load")
	}
	if _, err := s.CachedOriginal(f); err == nil {
		t.Fatal("old identity still readable")
	}
	if _, err := os.Stat(s.originalCachePath(f)); !os.IsNotExist(err) {
		t.Fatal("old cache reappeared after cancellation")
	}
}
