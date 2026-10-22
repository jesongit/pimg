package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewCacheCoalescesReadsBoundsMemoryAndExpires(t *testing.T) {
	c := newPreviewCache(8, time.Minute)
	var calls atomic.Int32
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.get(context.Background(), "same", func(context.Context) ([]byte, error) { calls.Add(1); <-gate; return []byte("1234"), nil })
			if err != nil || !bytes.Equal(data, []byte("1234")) {
				t.Errorf("shared read: %q %v", data, err)
			}
		}()
	}
	close(gate)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate cold reads", calls.Load())
	}
	load := func(context.Context) ([]byte, error) { return []byte("5678"), nil }
	c.get(context.Background(), "other", load)
	c.get(context.Background(), "third", load)
	c.mu.Lock()
	if c.bytes > 8 || len(c.entries) != 2 || c.entries["same"] != nil {
		t.Fatal("LRU limit failed")
	}
	c.entries["other"].Value.(*previewEntry).expires = time.Now().Add(-time.Second)
	c.mu.Unlock()
	data, err := c.get(context.Background(), "other", func(context.Context) ([]byte, error) { return []byte("fresh"), nil })
	if err != nil || string(data) != "fresh" {
		t.Fatal("expired entry reused")
	}
}

func TestPreviewCacheFailureAndCancellationAreNotCached(t *testing.T) {
	c := newPreviewCache(32, time.Minute)
	bad := errors.New("bad ciphertext")
	if _, err := c.get(context.Background(), "broken", func(context.Context) ([]byte, error) { return nil, bad }); !errors.Is(err, bad) {
		t.Fatal(err)
	}
	data, err := c.get(context.Background(), "broken", func(context.Context) ([]byte, error) { return []byte("recovered"), nil })
	if err != nil || string(data) != "recovered" {
		t.Fatal("failed value was cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "cancelled", func(ctx context.Context) ([]byte, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("orphaned preview read")
	}
}
