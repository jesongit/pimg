package mediax

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Only headers are written: these fixtures cannot allocate a large pixel buffer.
func pixelGuardPNG(w, h uint32) []byte {
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		_ = binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(kind)
		out.Write(data)
		crc := crc32.NewIEEE()
		_, _ = crc.Write([]byte(kind))
		_, _ = crc.Write(data)
		_ = binary.Write(&out, binary.BigEndian, crc.Sum32())
	}
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header, w)
	binary.BigEndian.PutUint32(header[4:], h)
	header[8] = 8
	chunk("IHDR", header)
	chunk("IDAT", nil)
	chunk("IEND", nil)
	return out.Bytes()
}

func pixelGuardWebP(w, h uint32) []byte {
	out := make([]byte, 26)
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	copy(out[8:], "WEBPVP8L")
	binary.LittleEndian.PutUint32(out[16:], 5)
	out[20] = 0x2f
	binary.LittleEndian.PutUint32(out[21:], (w-1)|((h-1)<<14))
	return out
}

func TestPixelLimitNeverFallsBack(t *testing.T) {
	// Executable discovery is disabled. Reaching a native decoder would return
	// an availability/exec error instead of the required policy sentinel.
	t.Setenv("PATH", t.TempDir())
	for _, format := range []struct {
		name string
		data []byte
	}{
		{"png", pixelGuardPNG(8000, 5001)},
		{"webp", pixelGuardWebP(8000, 5001)},
	} {
		t.Run(format.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "extensionless.part")
			if err := os.WriteFile(path, format.data, 0600); err != nil {
				t.Fatal(err)
			}
			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"Go", func() error { _, err := goThumbOption(path, 1280, false); return err }},
				{"preview", func() error { _, err := MakePreview(path, "image", 1280); return err }},
				{"thumb", func() error { _, err := MakeThumb(path, "image", 1280); return err }},
				{"FFmpeg", func() error { _, err := ffmpegThumbOption(path, 1280, false, false); return err }},
				{"compact", func() error { _, err := CompactPreview(format.data); return err }},
				{"compatible", func() error {
					return MakeCompatible(t.Context(), path, filepath.Join(t.TempDir(), "out.png"), "image", "")
				}},
			} {
				if err := call.run(); !errors.Is(err, ErrMaxPixels) {
					t.Errorf("%s: expected non-retryable policy rejection, got %v", call.name, err)
				}
			}
		})
	}
}

func TestImagePixelDimensions(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		ok   bool
	}{
		{8000, 5000, true}, {8000, 5001, false}, {1, MaxPixels, true},
		{MaxPixels + 1, 1, false}, {int(^uint(0) >> 1), 2, false},
		{0, 1, false}, {1, -1, false},
	} {
		err := checkImagePixels(tc.w, tc.h)
		if (err == nil) != tc.ok {
			t.Errorf("%dx%d: %v", tc.w, tc.h, err)
		}
		if tc.w > 0 && tc.h > 0 && !tc.ok && !errors.Is(err, ErrMaxPixels) {
			t.Errorf("missing sentinel: %v", err)
		}
	}
}

func TestFFmpegSourceDimensions(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		ok, limit bool
	}{
		{"valid", `{"streams":[{"width":8000,"height":5000}]}`, true, false},
		{"oversized", `{"streams":[{"width":8000,"height":5001}]}`, false, true},
		{"other stream", `{"streams":[{"width":64,"height":64},{"width":8000,"height":5001}]}`, false, true},
		{"absent", `{"streams":[]}`, false, false},
		{"zero", `{"streams":[{"width":0,"height":100}]}`, false, false},
		{"negative", `{"streams":[{"width":-1,"height":100}]}`, false, false},
		{"overflow", `{"streams":[{"width":18446744073709551616,"height":100}]}`, false, false},
		{"truncated", `{"streams":[`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkFFmpegDimensions([]byte(tc.out))
			if (err == nil) != tc.ok || errors.Is(err, ErrMaxPixels) != tc.limit {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestHEIFSourceDimensions(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		ok, limit bool
	}{
		{"primary", "MIME type: image/heic\nimage: 8000x5000 (id=1), primary\n  thumbnail: 320x240\n", true, false},
		{"oversized primary", "image: 8000x5001 (id=1), primary\n  thumbnail: 128x128\n", false, true},
		{"second image", "image: 64x64 (id=1), primary\nimage: 8000x5001 (id=2)\n", false, true},
		{"thumbnail only", "  thumbnail: 128x128\n", false, false},
		{"empty", "", false, false},
		{"zero", "image: 0x100 (id=1), primary\n", false, false},
		{"negative", "image: -1x100\n", false, false},
		{"truncated", "image:\n", false, false},
		{"malformed", "image: 10x10garbage\n", false, false},
		{"overflow", "image: 18446744073709551616x100\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHEIFDimensions([]byte(tc.out))
			if (err == nil) != tc.ok || errors.Is(err, ErrMaxPixels) != tc.limit {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

// Run the test executable itself so command output limits work on every OS,
// without a shell, FFmpeg, HEIF fixtures, or a large source image.
func TestImageMetadataOutputLimit(t *testing.T) {
	for _, size := range []int{16, 64 << 10, (64 << 10) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestImageMetadataHelper$")
			cmd.Env = append(os.Environ(), "PIMG_METADATA_HELPER_BYTES="+strconv.Itoa(size))
			out, err := imageMetadata(cmd)
			if size <= 64<<10 {
				if err != nil || len(out) != size {
					t.Fatalf("size=%d: bytes=%d err=%v", size, len(out), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "元数据过大") {
				t.Fatalf("excess output accepted: %v", err)
			}
		})
	}
}

func TestImageDecoderOutputLimit(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestImageMetadataHelper$")
	cmd.Env = append(os.Environ(), "PIMG_METADATA_HELPER_BYTES=16")
	out, err := imageDecoderOutput(cmd)
	if err != nil || len(out) != 64<<10 {
		t.Fatalf("diagnostics escaped limit: bytes=%d err=%v", len(out), err)
	}
}

func TestVideoDiagnosticOutputLimit(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestImageMetadataHelper$")
	cmd.Env = append(os.Environ(), "PIMG_METADATA_HELPER_BYTES=16")
	var diagnostic limitedVideoDiagnostic
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if len(diagnostic.Bytes()) != 4096 {
		t.Fatalf("diagnostics escaped limit: %d bytes", len(diagnostic.Bytes()))
	}
}

func TestImageMetadataHelper(t *testing.T) {
	value := os.Getenv("PIMG_METADATA_HELPER_BYTES")
	if value == "" {
		return
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 0 || size > 1<<20 {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, size))
	_, _ = os.Stderr.Write(bytes.Repeat([]byte{'e'}, 128<<10))
	os.Exit(0)
}

func TestSmallWebPAndAVIFPreviews(t *testing.T) {
	if !HasFFmpeg() {
		t.Skip("ffmpeg unavailable")
	}
	encoders, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ format, codec string }{{"webp", "libwebp"}, {"avif", "libaom-av1"}} {
		t.Run(tc.format, func(t *testing.T) {
			if !bytes.Contains(encoders, []byte(tc.codec)) {
				t.Skip("fixture encoder unavailable: " + tc.codec)
			}
			path := filepath.Join(t.TempDir(), "source."+tc.format)
			args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=96x64", "-frames:v", "1", "-c:v", tc.codec, "-threads", "1", path}
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
			if err := MakeCompatible(t.Context(), path, filepath.Join(t.TempDir(), "view.png"), "image", ""); err != nil {
				t.Fatalf("compatible image: %v", err)
			}
			for _, blurred := range []bool{false, true} {
				thumb, err := makeThumb(path, "image", 48, blurred)
				if err != nil {
					t.Fatal(err)
				}
				cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb.Data))
				if err != nil || format != "jpeg" || cfg.Width != 48 || cfg.Height != 32 || thumb.HasBlur != blurred {
					t.Fatalf("invalid preview: %+v %s %v blurred=%v", cfg, format, err, thumb.HasBlur)
				}
			}
		})
	}
}
