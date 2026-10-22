//go:build !windows && !linux

package mediax

import (
	"os"
	"time"
)

func FileCreatedTime(f *os.File) (time.Time, bool) { return time.Time{}, false }
