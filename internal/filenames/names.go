// Package filenames defines stable, readable media names independently of IDs.
package filenames

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

// Metadata is captured before any encryption, remuxing or splitting. It travels
// with the archive, rather than depending on a temporary file's timestamps.
type Metadata struct {
	Date         string `json:"date"`
	Sequence     int    `json:"sequence"`
	OriginalTime int64  `json:"original_time"`
	TimeSource   string `json:"time_source"`
	TimeZone     string `json:"time_zone"`
	PartIndex    int    `json:"part_index,omitempty"`
	PartCount    int    `json:"part_count,omitempty"`
	ManualTime   *int64 `json:"manual_time,omitempty"`
}

func Location() *time.Location {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return zone
}

func Capture(t time.Time, source string) Metadata {
	return Metadata{Date: t.In(Location()).Format("20060102"), OriginalTime: t.Unix(), TimeSource: source, TimeZone: t.Format("-07:00")}
}

func (m Metadata) ValidDate() bool {
	_, err := time.Parse("20060102", m.Date)
	return len(m.Date) == 8 && err == nil
}

func (m Metadata) Ready() bool { return m.ValidDate() && m.Sequence >= 1 && m.Sequence <= 9999 }

func Stem(kind string, encrypted bool, m Metadata) string {
	if !m.Ready() {
		return ""
	}
	prefix := "P"
	if kind == "video" {
		prefix = "V"
	}
	if encrypted {
		prefix += "E"
	} else {
		prefix += "R"
	}
	return fmt.Sprintf("%s_%s_%04d", prefix, m.Date, m.Sequence)
}

// Name describes the bytes actually returned: encrypted carriers are PNG;
// restored originals use their real extension and the R (readable) marker.
func Name(kind string, encrypted bool, ext string, m Metadata) string {
	stem := Stem(kind, encrypted, m)
	if stem == "" {
		return ""
	}
	if encrypted {
		ext = "png"
	}
	ext = strings.TrimPrefix(ext, ".")
	return stem + "." + ext
}
