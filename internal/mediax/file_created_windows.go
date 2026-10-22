package mediax

import (
	"os"
	"syscall"
	"time"
)

// FileCreatedTime reads the opened source, never the upload workspace copy.
func FileCreatedTime(f *os.File) (time.Time, bool) {
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	if stat, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		t := time.Unix(0, stat.CreationTime.Nanoseconds())
		return t, t.Year() >= 1900 && t.Year() <= 9999
	}
	return time.Time{}, false
}
