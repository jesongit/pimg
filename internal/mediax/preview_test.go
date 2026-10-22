package mediax

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCompactPreviewPreservesCompliantJPEG(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 128, 96)), &jpeg.Options{Quality: 82}); err != nil {
		t.Fatal(err)
	}
	got, err := CompactPreview(b.Bytes())
	if err != nil || !bytes.Equal(got, b.Bytes()) {
		t.Fatal("already compressed cover was re-encoded", err)
	}
}

func TestShortTransportStreamPreview(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "source.m2ts")
	args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=5", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mpegts", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	thumb, err := MakePreview(path, "video", 1280)
	if err != nil {
		t.Fatal(err)
	}
	img, format, err := image.Decode(bytes.NewReader(thumb.Data))
	if err != nil || format != "jpeg" || img.Bounds().Dx() != 128 || img.Bounds().Dy() != 96 {
		t.Fatal("invalid short TS cover", err)
	}
}

func TestCompactPreviewBoundsBytesAndDimensions(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1800, 2400))
	rand.New(rand.NewSource(42)).Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var original bytes.Buffer
	if err := png.Encode(&original, img); err != nil {
		t.Fatal(err)
	}
	preview, err := CompactPreview(original.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	info, format, err := image.DecodeConfig(bytes.NewReader(preview))
	if err != nil || format != "jpeg" || info.Width > 1280 || info.Height > 1280 || len(preview) > PreviewTargetBytes {
		t.Fatalf("invalid preview: %v %s %dx%d %d", err, format, info.Width, info.Height, len(preview))
	}
	difference := info.Width*4 - info.Height*3
	if info.Height < 600 || difference < -4 || difference > 4 {
		t.Fatalf("lost recognizable size or aspect: %dx%d", info.Width, info.Height)
	}
	t.Logf("original=%d bytes, preview=%d bytes, dimensions=%dx%d", original.Len(), len(preview), info.Width, info.Height)
}

func TestVideoPreviewUsesMiddleFrameIncludingShortVideo(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "short.mp4")
	// At the beginning it is red. At the midpoint of this sub-second clip it
	// is blue; seeking to a hard-coded second would produce no frame at all.
	args := []string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=red:s=320x180:r=20:d=0.2", "-f", "lavfi", "-i", "color=c=blue:s=320x180:r=20:d=0.6", "-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-c:v", "libx264", "-pix_fmt", "yuv420p", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	thumb, err := MakeThumb(path, "video", 1280)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(thumb.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, b, _ := img.At(160, 90).RGBA()
	if b < r*3 || len(thumb.Data) > PreviewTargetBytes {
		t.Fatal("cover is not the blue midpoint frame")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "cover.jpg"), thumb.Data, 0600); err != nil {
		t.Fatal(err)
	}
}
