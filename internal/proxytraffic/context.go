package proxytraffic

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type trackingKey struct{}
type tracking struct {
	once   sync.Once
	cancel context.CancelCauseFunc
	meter  atomic.Pointer[Meter]
	done   <-chan struct{}
}

// Track cancels an operation only after it actually selects a proxy. Local and
// NO_PROXY work therefore remains runnable while the shared budget is exhausted.
func Track(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ctx = context.WithValue(ctx, trackingKey{}, &tracking{cancel: cancel, done: ctx.Done()})
	return ctx, func() { cancel(context.Canceled) }
}

func Mark(ctx context.Context, m *Meter) error {
	if m == nil {
		return nil
	}
	if t, ok := ctx.Value(trackingKey{}).(*tracking); ok {
		t.meter.Store(m)
		if err := m.Check(); err != nil {
			t.cancel(err)
			return err
		}
		t.once.Do(func() {
			go func() {
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-t.done:
						return
					case <-ticker.C:
						if err := m.Check(); err != nil {
							t.cancel(err)
							return
						}
					}
				}
			}()
		})
	}
	return m.Check()
}

func PauseError(ctx context.Context) error {
	if t, ok := ctx.Value(trackingKey{}).(*tracking); ok {
		if m := t.meter.Load(); m != nil {
			return m.Check()
		}
	}
	return nil
}
