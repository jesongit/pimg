package mediax

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func FileCreatedTime(f *os.File) (time.Time, bool) {
	var stat unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stat); err != nil || stat.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	t := time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec))
	return t, t.Year() >= 1900 && t.Year() <= 9999
}
