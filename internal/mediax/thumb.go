// Package mediax 负责媒体识别、缩略图与视频封面生成。
// 图片走 Go 标准解码 + x/image 缩放；视频与 Go 解码不了的图片用 ffmpeg。
package mediax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	_ "golang.org/x/image/bmp"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

// Thumb 缩略图产物。
type Thumb struct {
	Data    []byte // jpeg
	W, H    int
	Blur    []byte // 模糊封面（像素化+模糊，仅展示效果，非安全保障）
	HasBlur bool
}

// Info 媒体探测信息。
type Info struct {
	Kind     string // image|video|other
	Mime     string
	Width    int
	Height   int
	Duration float64
}

var imageMimes = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "avif": "image/avif", "bmp": "image/bmp",
	"heic": "image/heic", "heif": "image/heif",
}

var videoMimes = map[string]string{
	"mp4": "video/mp4", "m4v": "video/mp4", "webm": "video/webm",
	"mov": "video/quicktime", "mkv": "video/x-matroska",
	"ts": "video/mp2t", "m2ts": "video/mp2t", "mts": "video/mp2t",
	"avi": "video/x-msvideo", "flv": "video/x-flv",
}

// Detect 依据扩展名 + 内容嗅探识别媒体。
func Detect(ext string, sniffed string) Info {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if m, ok := imageMimes[ext]; ok {
		return Info{Kind: "image", Mime: m}
	}
	if m, ok := videoMimes[ext]; ok {
		return Info{Kind: "video", Mime: m}
	}
	switch {
	case strings.HasPrefix(sniffed, "image/"):
		return Info{Kind: "image", Mime: strings.SplitN(sniffed, ";", 2)[0]}
	case strings.HasPrefix(sniffed, "video/"):
		return Info{Kind: "video", Mime: strings.SplitN(sniffed, ";", 2)[0]}
	}
	return Info{Kind: "other", Mime: "application/octet-stream"}
}

// HasFFmpeg 探测 ffmpeg 是否可用（结果缓存）。
var (
	ffmpegOnce sync.Once
	ffmpegOK   bool
)

func HasFFmpeg() bool {
	ffmpegOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ffmpegOK = exec.CommandContext(ctx, "ffmpeg", "-version").Run() == nil
	})
	return ffmpegOK
}

// MaxPixels 防解码炸弹的像素上限。
const MaxPixels = 40_000_000

const PreviewMaxDimension = 1280
const PreviewTargetBytes = 100 << 10

// ImageInfo 解析图片尺寸。
func ImageInfo(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// MakeThumb 为文件生成缩略图。maxDim 为长边上限。
func MakeThumb(path, kind string, maxDim int) (*Thumb, error) {
	return makeThumb(path, kind, maxDim, true)
}

// MakePreview creates only the locally managed clear preview, without generating
// an unused blurred derivative.
func MakePreview(path, kind string, maxDim int) (*Thumb, error) {
	return makeThumb(path, kind, maxDim, false)
}
func makeThumb(path, kind string, maxDim int, blurred bool) (*Thumb, error) {
	if maxDim <= 0 {
		maxDim = PreviewMaxDimension
	}
	if kind == "image" {
		if t, err := goThumbOption(path, maxDim, blurred); err == nil {
			return t, nil
		}
		if isHEIF(path) {
			return heifThumb(path, maxDim, blurred)
		}
		// Go 解码失败（webp/avif 等）→ ffmpeg 兜底
	}
	if !HasFFmpeg() {
		return nil, errors.New("无法生成缩略图（图片解码失败且 ffmpeg 不可用）")
	}
	return ffmpegThumbOption(path, maxDim, kind == "video", blurred)
}

func goThumb(path string, maxDim int) (*Thumb, error) { return goThumbOption(path, maxDim, true) }
func goThumbOption(path string, maxDim int, blurred bool) (*Thumb, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, errors.New("图片像素数超限")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx()*b.Dy() > MaxPixels {
		return nil, fmt.Errorf("图片像素数超限(%dx%d)", b.Dx(), b.Dy())
	}
	t := scaleToFit(img, maxDim)
	data, err := compactJPEG(t)
	if err != nil {
		return nil, err
	}
	preview, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if !blurred {
		return &Thumb{Data: data, W: preview.Width, H: preview.Height}, nil
	}
	blur := pixelBlur(scaleToFit(t, max(preview.Width, preview.Height)), 28)
	blurData, err := encodeJPEG(blur, 70)
	if err != nil {
		return nil, err
	}
	return &Thumb{Data: data, W: preview.Width, H: preview.Height, Blur: blurData, HasBlur: true}, nil
}

func ffmpegThumb(path string, maxDim int, video bool) (*Thumb, error) {
	return ffmpegThumbOption(path, maxDim, video, true)
}
func ffmpegThumbOption(path string, maxDim int, video, blurred bool) (*Thumb, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("pimg-thumb-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error"}
	var seek string
	if video {
		duration, _, _ := ProbeVideo(path)
		seek = fmt.Sprintf("%.6f", duration/2)
	}
	output := []string{"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease", maxDim, maxDim), "-c:v", "png", "-pix_fmt", "rgb24", tmp}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := func(accurate bool) (*Thumb, error) {
		input := append([]string(nil), args...)
		if video && !accurate {
			input = append(input, "-ss", seek)
		}
		input = append(input, "-i", path)
		if video && accurate {
			input = append(input, "-ss", seek)
		}
		input = append(input, output...)
		if out, err := exec.CommandContext(ctx, "ffmpeg", input...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg 生成封面失败: %v: %s", err, truncate(out, 200))
		}
		return goThumbOption(tmp, maxDim, blurred)
	}
	thumb, err := run(false)
	// Short transport streams may have no decodable frame after a demuxer seek.
	// Retry by decoding from the beginning, keeping the same total deadline.
	if err != nil && video && ctx.Err() == nil {
		_ = os.Remove(tmp)
		return run(true)
	}
	return thumb, err
}

// CompactPreview keeps recognizable detail while bounding the local preview.
// JPEG preserves the existing .jpg cover links and needs no external service.
func CompactPreview(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, errors.New("预览图像素数超限")
	}
	if format == "jpeg" && cfg.Width <= PreviewMaxDimension && cfg.Height <= PreviewMaxDimension && len(data) <= PreviewTargetBytes {
		return bytes.Clone(data), nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return compactJPEG(scaleToFit(img, PreviewMaxDimension))
}

func compactJPEG(img image.Image) ([]byte, error) {
	for {
		for _, quality := range []int{82, 74, 66, 58, 50, 42} {
			data, err := encodeJPEG(img, quality)
			if err != nil {
				return nil, err
			}
			if len(data) <= PreviewTargetBytes {
				return data, nil
			}
		}
		img = scaleToFit(img, max(1, max(img.Bounds().Dx(), img.Bounds().Dy())*4/5))
	}
}

// ProbeVideo 用 ffprobe 读取视频时长与分辨率。
func ProbeVideo(path string) (dur float64, w, h int) {
	if !HasFFmpeg() {
		return 0, 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration", "-of", "json", path).Output()
	if err != nil {
		return 0, 0, 0
	}
	var j struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &j) != nil || len(j.Streams) == 0 {
		return 0, 0, 0
	}
	fmt.Sscanf(j.Format.Duration, "%f", &dur)
	return dur, j.Streams[0].Width, j.Streams[0].Height
}

func scaleToFit(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return src
	}
	sc := float64(maxDim) / float64(max(w, h))
	nw, nh := max(1, int(float64(w)*sc)), max(1, int(float64(h)*sc))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// pixelBlur 像素化模糊：缩到很小再放大，叠加轻度 boxblur 观感。
func pixelBlur(src image.Image, block int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := max(1, w/block), max(1, h/block)
	small := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), src, b, draw.Over, nil)
	big := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.NearestNeighbor.Scale(big, big.Bounds(), small, small.Bounds(), draw.Over, nil)
	return big
}

func encodeJPEG(img image.Image, q int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
