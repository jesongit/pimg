package store

import (
	"context"
	"errors"
	"fmt"
	"pimg/internal/storage"
	"testing"
	"time"
)

func storageObservation(t *testing.T, s *Service, id string) string {
	t.Helper()
	var available bool
	var latency, speed float64
	var at int64
	var message string
	if err := s.DB.QueryRow(`SELECT available,latency_ms,bytes_per_second,checked_at,error FROM storage_health WHERE instance_id=?`, id).Scan(&available, &latency, &speed, &at, &message); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(available, latency, speed, at, message)
}
func TestCanceledStorageTransferPreservesLastCompletedHealth(t *testing.T) {
	s, peer, _ := peerFixture(t)
	s.recordStorageResult(peer, 100, time.Now().Add(-time.Second), nil)
	before := storageObservation(t, s, peer)
	for _, err := range []error{context.Canceled, fmt.Errorf("request interrupted: %w", context.Canceled)} {
		s.recordStorageResult(peer, 0, time.Now(), err)
		if before != storageObservation(t, s, peer) {
			t.Fatal("cancellation overwrote persisted health")
		}
		s.storages.mu.RLock()
		v := s.storages.scores[peer]
		s.storages.mu.RUnlock()
		if v.available == nil || !*v.available || v.lastError != "" {
			t.Fatal(v)
		}
	}
	s.recordStorageResult(peer, 0, time.Now(), errors.New("access denied"))
	if before == storageObservation(t, s, peer) {
		t.Fatal("real failure was suppressed")
	}
}

type canceledProbeCold struct {
	storage.Cold
	cancel context.CancelFunc
}

func (c canceledProbeCold) Check(ctx context.Context) error { c.cancel(); return ctx.Err() }
func TestShutdownDuringStorageProbeDoesNotRecordOutage(t *testing.T) {
	s, peer, _ := peerFixture(t)
	s.recordStorageResult(peer, 0, time.Now(), nil)
	before := storageObservation(t, s, peer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := s.instance(peer)
	if err != nil {
		t.Fatal(err)
	}
	instance.cold = canceledProbeCold{instance.cold, cancel}
	if err = s.RunStorageProbe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if before != storageObservation(t, s, peer) {
		t.Fatal("shutdown marked provider unavailable")
	}
}
func TestLegacyCanceledStorageHealthRestoresAsUnknownWithoutRewritingRows(t *testing.T) {
	s, peer, _ := peerFixture(t)
	for _, message := range []string{"context canceled", "probe request: context canceled", "access denied"} {
		if _, err := s.DB.Exec(`INSERT INTO storage_health(instance_id,available,checked_at,error) VALUES(?,0,123,?) ON CONFLICT(instance_id) DO UPDATE SET available=0,checked_at=123,error=excluded.error`, peer, message); err != nil {
			t.Fatal(err)
		}
		before := storageObservation(t, s, peer)
		if err := s.restoreStorageHealth(); err != nil {
			t.Fatal(err)
		}
		s.storages.mu.RLock()
		v := s.storages.scores[peer]
		s.storages.mu.RUnlock()
		if message == "access denied" {
			if v.available == nil || *v.available || v.lastError != message {
				t.Fatal(v)
			}
		} else if v.available != nil || v.lastError != "" || v.at.Unix() != 0 {
			t.Fatal(v)
		}
		if before != storageObservation(t, s, peer) {
			t.Fatal("startup rewrote a historical row")
		}
	}
}
