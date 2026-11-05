package mediax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrMaxPixels is a policy rejection, not a decoder failure. Callers must not
// retry the source with another decoder after receiving this error.
var ErrMaxPixels = errors.New("图片像素数超限")

func checkImagePixels(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("图片尺寸无效(%dx%d)", w, h)
	}
	// Division avoids overflow even for dimensions supplied by external tools.
	if w > MaxPixels/h {
		return fmt.Errorf("%w(%dx%d)", ErrMaxPixels, w, h)
	}
	return nil
}

// checkFFmpegImagePixels checks the source, not the scaled PNG. ffprobe may
// inspect coded frames to find dimensions, so its decoder has the same limit.
func checkFFmpegImagePixels(ctx context.Context, path string) error {
	if w, h, err := ImageInfo(path); err == nil {
		return checkImagePixels(w, h)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := imageMetadata(exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-max_pixels", strconv.Itoa(MaxPixels), "-select_streams", "v",
		"-show_entries", "stream=width,height", "-of", "json", path))
	if err != nil {
		return err
	}
	return checkFFmpegDimensions(out)
}

func checkFFmpegDimensions(out []byte) error {
	var info struct {
		Streams []struct{ Width, Height int }
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("无法读取图片尺寸: %w", err)
	}
	if len(info.Streams) == 0 {
		return errors.New("无法读取图片尺寸")
	}
	// FFmpeg can choose a stream other than v:0 when several are present.
	for _, stream := range info.Streams {
		if err := checkImagePixels(stream.Width, stream.Height); err != nil {
			return err
		}
	}
	return nil
}

// heif-info ships with the already required libheif-tools. Unlike ffprobe it
// understands HEIF grids and reports the original canvas before thumbnailing.
func checkHEIFPixels(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := imageMetadata(exec.CommandContext(ctx, "heif-info", path))
	if err != nil {
		return err
	}
	return checkHEIFDimensions(out)
}

func checkHEIFDimensions(out []byte) error {
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		// Only top-level images describe source canvases. A thumbnail line
		// cannot establish the source size; check every top-level image.
		if !strings.HasPrefix(line, "image:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "image:"))
		if len(fields) == 0 {
			return errors.New("无法读取 HEIF 图片尺寸")
		}
		width, height, ok := strings.Cut(fields[0], "x")
		w, werr := strconv.Atoi(width)
		h, herr := strconv.Atoi(height)
		if !ok || werr != nil || herr != nil {
			return errors.New("无法读取 HEIF 图片尺寸")
		}
		if err := checkImagePixels(w, h); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("无法读取 HEIF 图片尺寸")
	}
	return nil
}

// Metadata commands cannot grow Go buffers without bound, even for malformed
// inputs or excessive diagnostics. This is not a native-process memory limit.
func imageMetadata(cmd *exec.Cmd) ([]byte, error) {
	var out limitedImageMetadata
	var diagnostic limitedImageMetadata
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	err := cmd.Run()
	if out.overflow {
		return nil, errors.New("图片尺寸元数据过大")
	}
	if err != nil {
		return nil, fmt.Errorf("无法读取图片尺寸: %w: %s", err, truncate(diagnostic.buf.Bytes(), 200))
	}
	return out.buf.Bytes(), nil
}

// Decoder output is diagnostic only; successful conversion writes to a file.
func imageDecoderOutput(cmd *exec.Cmd) ([]byte, error) {
	var diagnostic limitedImageMetadata
	cmd.Stdout, cmd.Stderr = &diagnostic, &diagnostic
	err := cmd.Run()
	return diagnostic.buf.Bytes(), err
}

type limitedImageMetadata struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom would bypass Write's
	// limit when os/exec copies a pipe using io.Copy.
	buf      bytes.Buffer
	overflow bool
}

func (b *limitedImageMetadata) Write(p []byte) (int, error) {
	const limit = 64 << 10
	n := len(p)
	if n > limit-b.buf.Len() {
		b.overflow = true
		p = p[:limit-b.buf.Len()]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}
