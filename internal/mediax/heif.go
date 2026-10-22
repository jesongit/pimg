package mediax

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Uploads and reconstructed sources have extensionless temporary names. Inspect
// the ISO BMFF file-type box rather than depending on the original filename.
func isHEIF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var header [4096]byte
	n, _ := io.ReadFull(f, header[:])
	if n < 16 || string(header[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(header[:4]))
	if size < 16 || size > n {
		return false
	}
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 {
			continue
		} // minor version, not a brand
		switch string(header[i : i+4]) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
			return true
		}
	}
	return false
}

// libheif decodes the primary image (including its orientation), never auxiliary
// depth images. A private directory contains any converter output and cleanup.
func heifPNG(ctx context.Context, source, dest string, maxDim int) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "heif-thumbnailer", "-p", "-s", strconv.Itoa(maxDim), source, dest).CombinedOutput()
	if err != nil {
		return fmt.Errorf("HEIF 解码失败: %w: %s", err, truncate(out, 200))
	}
	w, h, err := ImageInfo(dest)
	if err != nil {
		return err
	}
	if w <= 0 || h <= 0 || int64(w)*int64(h) > MaxPixels {
		return fmt.Errorf("HEIF 图片像素数超限(%dx%d)", w, h)
	}
	return nil
}

func heifThumb(source string, maxDim int, blurred bool) (*Thumb, error) {
	dir, err := os.MkdirTemp("", "pimg-heif-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "primary.png")
	if err = heifPNG(context.Background(), source, path, maxDim); err != nil {
		return nil, err
	}
	return goThumbOption(path, maxDim, blurred)
}
