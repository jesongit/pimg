package mediax

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"pimg/internal/filenames"
)

// OriginalTime reads content metadata only. In particular it never stats the
// server's temporary copy or treats a modification time as a creation time.
func OriginalTime(ctx context.Context, path, kind string) (time.Time, string) {
	if kind == "image" {
		if data := imageEXIF(path); len(data) > 0 {
			if t, ok := exifOriginalTime(data); ok {
				return t, "exif"
			}
		}
	}
	if kind == "video" {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format_tags=creation_time,date,com.apple.quicktime.creationdate:stream_tags=creation_time", "-of", "json", path).Output()
		if err == nil && len(out) < 1<<20 {
			var probe struct {
				Format  struct{ Tags map[string]string }
				Streams []struct{ Tags map[string]string }
			}
			if json.Unmarshal(out, &probe) == nil {
				sets := []map[string]string{probe.Format.Tags}
				for _, stream := range probe.Streams {
					sets = append(sets, stream.Tags)
				}
				for _, tags := range sets {
					for _, key := range []string{"com.apple.quicktime.creationdate", "creation_time", "date"} {
						if t, ok := parseOriginalTime(tags[key]); ok {
							return t, "video_metadata"
						}
					}
				}
			}
		}
	}
	return time.Time{}, ""
}

func parseOriginalTime(value string) (time.Time, bool) {
	value = strings.Trim(value, "\x00 \t\r\n")
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05-0700", "2006:01:02 15:04:05-07:00", "2006-01-02 15:04:05-07:00"} {
		if t, err := time.Parse(layout, value); err == nil && t.Year() >= 1900 && t.Year() <= 9999 {
			return t, true
		}
	}
	for _, layout := range []string{"2006:01:02 15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, filenames.Location()); err == nil && t.Year() >= 1900 && t.Year() <= 9999 {
			return t, true
		}
	}
	return time.Time{}, false
}

// A bounded metadata reader skips image payloads instead of loading/decompressing
// the image. PNG eXIf and WebP EXIF contain the same TIFF structure as JPEG APP1.
func imageEXIF(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil
	}
	read := func(off, n int64) []byte {
		if off < 0 || n < 0 || n > 2<<20 || off > stat.Size() || n > stat.Size()-off {
			return nil
		}
		b := make([]byte, int(n))
		if _, err := f.ReadAt(b, off); err != nil {
			return nil
		}
		return b
	}
	head := read(0, 12)
	if len(head) != 12 {
		return nil
	}
	if string(head[4:8]) == "ftyp" {
		return heifEXIF(f, stat.Size())
	}
	if string(head[:2]) == "II" || string(head[:2]) == "MM" {
		return read(0, min(stat.Size(), 2<<20))
	}
	if bytes.Equal(head[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		for off, n := int64(8), 0; n < 4096; n++ {
			h := read(off, 8)
			if len(h) != 8 {
				return nil
			}
			size := int64(binary.BigEndian.Uint32(h[:4]))
			if string(h[4:]) == "eXIf" {
				return read(off+8, size)
			}
			if string(h[4:]) == "IEND" {
				return nil
			}
			off += 12 + size
		}
	}
	if string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		for off, n := int64(12), 0; n < 4096; n++ {
			h := read(off, 8)
			if len(h) != 8 {
				return nil
			}
			size := int64(binary.LittleEndian.Uint32(h[4:]))
			if string(h[:4]) == "EXIF" {
				return read(off+8, size)
			}
			off += 8 + size + (size & 1)
		}
	}
	if head[0] == 0xff && head[1] == 0xd8 {
		if _, err := f.Seek(2, io.SeekStart); err != nil {
			return nil
		}
		for n := 0; n < 4096; n++ {
			var marker [2]byte
			if _, err := io.ReadFull(f, marker[:]); err != nil || marker[0] != 0xff {
				return nil
			}
			for marker[1] == 0xff {
				if _, err := io.ReadFull(f, marker[1:]); err != nil {
					return nil
				}
			}
			if marker[1] == 0xda || marker[1] == 0xd9 {
				return nil
			}
			if marker[1] == 0x01 || marker[1] >= 0xd0 && marker[1] <= 0xd7 {
				continue
			}
			var length [2]byte
			if _, err := io.ReadFull(f, length[:]); err != nil {
				return nil
			}
			size := int64(binary.BigEndian.Uint16(length[:])) - 2
			if size < 0 {
				return nil
			}
			off, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil
			}
			if marker[1] == 0xe1 {
				data := read(off, size)
				if bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
					return data
				}
			}
			if _, err := f.Seek(size, io.SeekCurrent); err != nil {
				return nil
			}
		}
	}
	return nil
}

func exifOriginalTime(data []byte) (time.Time, bool) {
	data = bytes.TrimPrefix(data, []byte("Exif\x00\x00"))
	if len(data) < 8 {
		return time.Time{}, false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return time.Time{}, false
	}
	if order.Uint16(data[2:4]) != 42 {
		return time.Time{}, false
	}
	tags := map[uint16]string{}
	seen := map[uint32]bool{}
	var walk func(uint32, int)
	walk = func(off uint32, depth int) {
		if depth > 4 || seen[off] || uint64(off)+2 > uint64(len(data)) {
			return
		}
		seen[off] = true
		n := uint64(order.Uint16(data[off : off+2]))
		start := uint64(off) + 2
		if n > 4096 || start+12*n > uint64(len(data)) {
			return
		}
		for i := uint64(0); i < n; i++ {
			e := data[start+i*12 : start+(i+1)*12]
			tag, typ := order.Uint16(e[:2]), order.Uint16(e[2:4])
			count := uint64(order.Uint32(e[4:8]))
			if tag == 0x8769 && typ == 4 && count == 1 {
				walk(order.Uint32(e[8:12]), depth+1)
			}
			if typ != 2 || count == 0 || count > 128 {
				continue
			}
			value := e[8:12]
			if count > 4 {
				pos := uint64(order.Uint32(e[8:12]))
				if pos+count > uint64(len(data)) {
					continue
				}
				value = data[pos : pos+count]
			} else {
				value = value[:count]
			}
			tags[tag] = strings.Trim(string(value), "\x00 ")
		}
	}
	walk(order.Uint32(data[4:8]), 0)
	for _, pair := range [][2]uint16{{0x9003, 0x9011}, {0x9004, 0x9012}} {
		value := tags[pair[0]]
		if value == "" {
			continue
		}
		if offset := tags[pair[1]]; offset != "" {
			value += offset
		}
		if t, ok := parseOriginalTime(value); ok {
			return t, true
		}
	}
	return time.Time{}, false
}
