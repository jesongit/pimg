package filenames

import (
	"testing"
	"time"
)

func TestRequestedNamesAndSourceCalendar(t *testing.T) {
	for _, tc := range []struct {
		kind            string
		encrypted       bool
		date, ext, want string
	}{
		{"image", true, "20260601", "jpg", "PE_20260601_0001.png"},
		{"image", false, "20250101", "png", "PR_20250101_0001.png"},
		{"video", true, "20000101", "mp4", "VE_20000101_0001.png"},
		{"video", false, "20000101", "mp4", "VR_20000101_0001.mp4"},
	} {
		if got := Name(tc.kind, tc.encrypted, tc.ext, Metadata{Date: tc.date, Sequence: 1}); got != tc.want {
			t.Fatalf("got %s want %s", got, tc.want)
		}
	}
	m := Capture(time.Date(1999, 12, 31, 23, 30, 0, 0, time.FixedZone("-03", -3*3600)), "exif")
	if m.Date != "20000101" || m.TimeZone != "-03:00" {
		t.Fatal("date timezone or original offset lost", m)
	}
}
