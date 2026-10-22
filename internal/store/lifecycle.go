package store

import (
	"context"
	"sync"

	"pimg/internal/dbx"
)

var ErrDeleting = dbx.ErrDeleting

type fileLock struct {
	gate chan struct{}
	refs int
}

type fileLocks struct {
	mu    sync.Mutex
	items map[string]*fileLock
}

// lockFile serializes remote writes and deletion for one file, including direct
// service calls. Waiting for a long transfer remains cancellable.
func (s *Service) lockFile(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.locks.mu.Lock()
	if s.locks.items == nil {
		s.locks.items = make(map[string]*fileLock)
	}
	l := s.locks.items[id]
	if l == nil {
		l = &fileLock{gate: make(chan struct{}, 1)}
		s.locks.items[id] = l
	}
	l.refs++
	s.locks.mu.Unlock()
	releaseRef := func() {
		s.locks.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.locks.items, id)
		}
		s.locks.mu.Unlock()
	}
	select {
	case l.gate <- struct{}{}:
		return func() { <-l.gate; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

func (s *Service) mutableFile(id string) (*dbx.File, error) {
	f, err := s.DB.MutationFile(id)
	if err != nil {
		return nil, err
	}
	if f.PurgeState.String != "" {
		return nil, ErrDeleting
	}
	return f, nil
}
