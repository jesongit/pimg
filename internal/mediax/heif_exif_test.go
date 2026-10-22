package mediax

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func exifBox(kind string, data []byte) []byte {
	out := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	copy(out[4:], kind)
	copy(out[8:], data)
	return out
}
func TestOriginalTimeFromHEIFAndAVIFItemLocations(t *testing.T) {
	tiff := imageEXIF("testdata/taken-20000101.jpg")[6:]
	for _, method := range []byte{0, 1} {
		for _, brand := range []string{"heic", "mif1", "avif"} {
			t.Run(brand+string(rune('0'+method)), func(t *testing.T) {
				ftyp := exifBox("ftyp", []byte(brand+"\x00\x00\x00\x00"+brand))
				exif := append([]byte{0, 0, 0, 0}, tiff...)
				infe := exifBox("infe", append([]byte{2, 0, 0, 0, 0, 1, 0, 0}, []byte("Exif\x00")...))
				iinf := exifBox("iinf", append([]byte{0, 0, 0, 0, 0, 1}, infe...))
				ilocData := []byte{1, 0, 0, 0, 0x44, 0, 0, 1, 0, 1, 0, method, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
				binary.BigEndian.PutUint32(ilocData[20:], uint32(len(exif)))
				if method == 0 {
					binary.BigEndian.PutUint32(ilocData[16:], uint32(len(ftyp)+8))
				}
				children := append(iinf, exifBox("iloc", ilocData)...)
				if method == 1 {
					children = append(children, exifBox("idat", exif)...)
				}
				meta := exifBox("meta", append([]byte{0, 0, 0, 0}, children...))
				data := ftyp
				if method == 0 {
					data = append(data, exifBox("mdat", exif)...)
				}
				data = append(data, meta...)
				path := filepath.Join(t.TempDir(), "source-without-extension")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				got, source := OriginalTime(context.Background(), path, "image")
				if source != "exif" || got.Format("2006-01-02T15:04:05-07:00") != "2000-01-01T09:08:07+08:00" {
					t.Fatal(got, source)
				}
				// A truncated item must not claim a camera timestamp or panic.
				for n := 0; n < len(data); n++ {
					if err := os.WriteFile(path, data[:n], 0600); err != nil {
						t.Fatal(err)
					}
					if _, s := OriginalTime(context.Background(), path, "image"); s != "" {
						t.Fatal("truncated metadata accepted", n)
					}
				}
			})
		}
	}
}
