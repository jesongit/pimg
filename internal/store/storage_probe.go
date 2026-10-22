package store

import (
	"context"
	"time"
)

// A single bounded metadata check every five minutes supplies outage history.
// Throughput ordering comes from successful real transfers, never speed-test downloads.
func (s *Service) ProbeStorageIfDue(ctx context.Context) error {
	s.storages.mu.Lock()
	due := time.Since(s.storages.lastProbe) > 5*time.Minute
	if due {
		s.storages.lastProbe = time.Now()
	}
	s.storages.mu.Unlock()
	if !due {
		return nil
	}
	return s.RunStorageProbe(ctx)
}
func (s *Service) RunStorageProbe(ctx context.Context) error {
	for _, instance := range s.StorageInstances() {
		if !instance.Enabled {
			continue
		}
		id := instance.ID
		if ctx.Err() != nil {
			return ctx.Err()
		}
		child, cancel := context.WithTimeout(ctx, 5*time.Second)
		started := time.Now()
		e := s.checkStorage(child, id)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.recordStorageResult(id, 0, started, e)
	}
	return nil
}
