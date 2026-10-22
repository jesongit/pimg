package tguser

import (
	"fmt"
	"time"
)

type durableProvisionContextKey struct{}

type RetryAfterError struct{ Wait time.Duration }

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("TG FLOOD_WAIT: %d 秒后重试", int64(e.Wait/time.Second))
}
