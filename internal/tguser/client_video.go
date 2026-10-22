package tguser

import (
	"context"
	"errors"
	"math"
	"strings"
)

// VideoMetadata describes the actual uploaded file (or independently playable
// segment), as probed by the caller. SupportsStreaming must reflect its container
// and codecs; this attribute does not transcode or repair a file.
type VideoMetadata struct {
	Duration          float64
	Width             int
	Height            int
	SupportsStreaming bool
}

// VideoUploader is optional, keeping existing Transport implementations and
// storage test doubles compatible. Ordinary Upload always forces file display.
type VideoUploader interface {
	UploadVideo(context.Context, int64, int, string, string, []byte, []byte, string, int64, VideoMetadata) (Reference, error)
}

var _ VideoUploader = (*Client)(nil)

func (c *Client) UploadVideo(ctx context.Context, chatID int64, topicID int, name, mime string, data, thumb []byte, caption string, randomID int64, video VideoMetadata) (Reference, error) {
	if err := validateVideo(mime, video); err != nil {
		return Reference{}, err
	}
	return c.upload(ctx, chatID, topicID, name, mime, data, thumb, caption, randomID, &video)
}

func validateVideo(mime string, video VideoMetadata) error {
	if !strings.HasPrefix(strings.ToLower(mime), "video/") || strings.TrimPrefix(strings.ToLower(mime), "video/") == "" {
		return errors.New("视频上传需要有效的 video/* MIME 类型")
	}
	if video.Duration <= 0 || math.IsNaN(video.Duration) || math.IsInf(video.Duration, 0) || video.Width <= 0 || video.Height <= 0 || int64(video.Width) > math.MaxInt32 || int64(video.Height) > math.MaxInt32 {
		return errors.New("视频上传需要有效的时长、宽度和高度")
	}
	return nil
}
