package mediax

import (
	"bytes"
	"encoding/binary"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHEIFSignatureWithoutExtension(t *testing.T) {
	for _, tc := range []struct {
		brand, compatible string
		want              bool
	}{
		{"heic", "hevc", true}, {"mif1", "heic", true}, {"heix", "", true},
		{"isom", "mp42", false}, {"qt  ", "", false}, {"avif", "av01", false},
	} {
		b := make([]byte, 20)
		binary.BigEndian.PutUint32(b, uint32(len(b)))
		copy(b[4:], "ftyp")
		copy(b[8:], tc.brand)
		copy(b[16:], tc.compatible)
		p := filepath.Join(t.TempDir(), "source.part")
		os.WriteFile(p, b, 0600)
		if got := isHEIF(p); got != tc.want {
			t.Errorf("%s/%s: %v", tc.brand, tc.compatible, got)
		}
		for _, malformed := range [][]byte{b[:12], append([]byte{255, 255, 255, 255}, b[4:]...), []byte("not HEIC")} {
			os.WriteFile(p, malformed, 0600)
			if isHEIF(p) {
				t.Fatal("accepted malformed ftyp")
			}
		}
	}
}

func TestRealHEIFPreviewAndCompatible(t *testing.T) {
	fixture := os.Getenv("PIMG_TEST_HEIC_FIXTURE")
	if fixture == "" {
		t.Skip("set PIMG_TEST_HEIC_FIXTURE")
	}
	if _, err := exec.LookPath("heif-thumbnailer"); err != nil {
		t.Skip("libheif tools unavailable")
	}
	source, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "extensionless.part")
	os.WriteFile(p, source, 0600)
	thumb, err := MakePreview(p, "image", PreviewMaxDimension)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb.Data))
	if err != nil || format != "jpeg" || cfg.Width < 640 || cfg.Width > 1280 || cfg.Height <= 0 || cfg.Height > 854 || len(thumb.Data) > PreviewTargetBytes || abs(cfg.Width*854-cfg.Height*1280) > 1280 {
		t.Fatalf("invalid HEIC preview %v %s %+v bytes=%d", err, format, cfg, len(thumb.Data))
	}
	dest := filepath.Join(t.TempDir(), "compatible.part")
	if err = MakeCompatible(t.Context(), p, dest, "image", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err = image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "png" {
		t.Fatalf("invalid compatible image: %v %s", err, format)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(source, after) {
		t.Fatal("source changed")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
