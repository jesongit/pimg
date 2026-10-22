package mediax

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOriginalTimeFromImageContainers(t *testing.T) {
	jpegData, err := os.ReadFile("testdata/taken-20000101.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(jpegData)); err != nil {
		t.Fatal("fixture must be a real JPEG", err)
	}
	tiff := imageEXIF("testdata/taken-20000101.jpg")[6:]
	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 12+len(tiff))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(tiff)))
	copy(chunk[4:], "eXIf")
	copy(chunk[8:], tiff)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	p := pngBuffer.Bytes()
	pngData := append(append(append([]byte{}, p[:33]...), chunk...), p[33:]...)
	if _, err := png.Decode(bytes.NewReader(pngData)); err != nil {
		t.Fatal("fixture must be a real PNG", err)
	}
	// The WebP metadata chunk uses a little-endian RIFF size and optional pad.
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPEXIF"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(webp[16:20], uint32(len(tiff)))
	webp = append(webp, tiff...)
	if len(tiff)&1 != 0 {
		webp = append(webp, 0)
	}
	binary.LittleEndian.PutUint32(webp[4:8], uint32(len(webp)-8))
	for name, data := range map[string][]byte{"photo.jpg": jpegData, "photo.png": pngData, "metadata.webp": webp} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, source := OriginalTime(context.Background(), path, "image")
			if got.Format("2006-01-02T15:04:05-07:00") != "2000-01-01T09:08:07+08:00" || source != "exif" {
				t.Fatalf("lost original time: %v %s", got, source)
			}
		})
	}
}

func TestOriginalTimeMalformedMetadataAndOffsets(t *testing.T) {
	data := imageEXIF("testdata/taken-20000101.jpg")[6:]
	for n := 0; n < len(data); n++ {
		if _, ok := exifOriginalTime(data[:n]); ok && n < 76 {
			t.Fatalf("truncated timestamp accepted at %d", n)
		}
	}
	broken := bytes.Clone(data)
	binary.LittleEndian.PutUint32(broken[4:8], 0xfffffff0)
	if _, ok := exifOriginalTime(broken); ok {
		t.Fatal("invalid IFD offset accepted")
	}
	if got, ok := parseOriginalTime("1999:12:31 23:30:00-03:00"); !ok || got.UTC().Format("20060102") != "20000101" {
		t.Fatal("offset ignored", got)
	}
	if _, ok := parseOriginalTime("0000:00:00 00:00:00"); ok {
		t.Fatal("camera placeholder accepted")
	}
}

func TestVideoOriginalTimeBeforeProcessing(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "original.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=size=32x32:rate=1", "-t", "1", "-c:v", "libx264", "-metadata", "creation_time=2000-01-01T01:02:03Z", path).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	got, source := OriginalTime(context.Background(), path, "video")
	if source != "video_metadata" || got.UTC().Format("2006-01-02T15:04:05Z") != "2000-01-01T01:02:03Z" {
		t.Fatal("recording time missing", got, source)
	}
}
