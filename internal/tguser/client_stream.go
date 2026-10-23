package tguser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// StreamTransport avoids buffering an entire archive/video in memory. An input
// reader remains owned by the caller; cancellation also closes it if it is an
// io.Closer, allowing blocked pipes/files to stop promptly.
type StreamTransport interface {
	UploadStream(context.Context, int64, int, string, string, io.Reader, int64, []byte, string, int64, *VideoMetadata) (Reference, error)
	OpenStream(context.Context, Reference) (io.ReadCloser, error)
}

var _ StreamTransport = (*Client)(nil)

var errStreamLength = errors.New("Telegram 流长度与声明的文件大小不一致")

// Keep one file transfer bounded while overlapping RPC round trips. The SDK's
// default single 128 KiB upload part leaves high-latency links mostly idle.
// Source reads remain sequential, so callers' hashing and pacing still apply.
const (
	telegramUploadPartSize   = 512 << 10
	telegramUploadThreads    = 4
	telegramDownloadPartSize = 1 << 20
)

type exactStreamReader struct {
	ctx         context.Context
	source      io.Reader
	remaining   int64
	complete    bool
	lengthError bool
}

func (r *exactStreamReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.complete {
		return 0, io.EOF
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n > 0 {
			r.lengthError = true
			return 0, errStreamLength
		}
		if err == io.EOF {
			r.complete = true
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF {
		if r.remaining > 0 {
			r.lengthError = true
			return n, io.ErrUnexpectedEOF
		}
		r.complete = true
	}
	return n, err
}

func (c *Client) UploadStream(ctx context.Context, chatID int64, topicID int, name, mime string, source io.Reader, size int64, thumb []byte, caption string, randomID int64, video *VideoMetadata) (Reference, error) {
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	if source == nil || size <= 0 || topicID <= 0 || name == "" || mime == "" || randomID == 0 {
		return Reference{}, errors.New("Telegram 流上传参数或持久化操作编号无效")
	}
	operation, managed, err := uploadOperation(caption, randomID)
	if err != nil {
		return Reference{}, err
	}
	if video != nil {
		if err := validateVideo(mime, *video); err != nil {
			return Reference{}, err
		}
	}
	limit := c.Status().UploadLimitBytes
	if limit <= 0 {
		limit = defaultUploadLimit
	}
	if size > limit {
		return Reference{}, fmt.Errorf("Telegram 文件共 %d 字节，超过当前账号单文件上限 %d 字节", size, limit)
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return Reference{}, err
	}
	api, account, err := c.ready()
	if err != nil {
		return Reference{}, err
	}
	stopClose := context.AfterFunc(ctx, func() {
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stopClose()
	reader := &exactStreamReader{ctx: ctx, source: source, remaining: size}
	if managed {
		message, found, err := c.findPimgMessage(ctx, ch, topicID, operation, size, 0, "")
		if err != nil {
			return Reference{}, err
		}
		if found {
			// The caller hashes the full wire reader. Recovery skips only the
			// network upload, never the local read or exact-length validation.
			if err := consumeRecoveredUpload(reader); err != nil {
				if errors.Is(err, errStreamLength) {
					return Reference{}, err
				}
				return Reference{}, c.safeError(err)
			}
			return c.recoveredReference(message), nil
		}
	}
	u := uploader.NewUploader(api).WithPartSize(telegramUploadPartSize).WithThreads(telegramUploadThreads)
	file, err := u.Upload(ctx, uploader.NewUpload(name, reader, size))
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	if reader.lengthError {
		return Reference{}, errStreamLength
	}
	if err != nil {
		return Reference{}, c.safeError(err)
	}
	if reader.remaining != 0 || !reader.complete {
		return Reference{}, errStreamLength
	}
	media := &tg.InputMediaUploadedDocument{File: file, MimeType: mime, ForceFile: true, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}}
	if video != nil {
		media.ForceFile = false
		media.NosoundVideo = true
		media.Attributes = append(media.Attributes, &tg.DocumentAttributeVideo{Duration: video.Duration, W: video.Width, H: video.Height, SupportsStreaming: video.SupportsStreaming})
	}
	if len(thumb) > 0 {
		preview, err := u.FromBytes(ctx, "preview.jpg", thumb)
		if err != nil {
			return Reference{}, c.safeError(err)
		}
		media.SetThumb(preview)
	}
	req := &tg.MessagesSendMediaRequest{Peer: inputPeer(ch), Media: media, Message: caption, RandomID: randomID, Silent: true}
	if topicID != 1 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: topicID})
	}
	updates, err := api.MessagesSendMedia(ctx, req)
	if err != nil {
		if managed && ctx.Err() == nil {
			message, found, recoveryErr := c.findPimgMessage(ctx, ch, topicID, operation, size, 0, "")
			if recoveryErr != nil {
				return Reference{}, recoveryErr
			}
			if found {
				return c.recoveredReference(message), nil
			}
		}
		return Reference{}, c.safeError(err)
	}
	ref, err := c.sentReference(ctx, updates, chatID, topicID, randomID, account)
	if err != nil && managed && ctx.Err() == nil {
		message, found, recoveryErr := c.findPimgMessage(ctx, ch, topicID, operation, size, 0, "")
		if recoveryErr != nil {
			return Reference{}, recoveryErr
		}
		if found {
			return c.recoveredReference(message), nil
		}
	}
	if err == nil && ref.Size != size {
		return Reference{}, errors.New("Telegram 上传文件大小与发送内容不符")
	}
	if err == nil {
		_ = c.handleUpdates(ctx, updates)
	}
	return ref, err
}

type downloadSnapshot struct {
	api      *tg.Client
	location tg.InputFileLocationClass
	size     int64
	closer   io.Closer
}

// mediaDatacenterClient exposes the connection metadata needed to route file
// downloads without creating a second authorization on the current datacenter.
type mediaDatacenterClient interface {
	Config() tg.Config
	DC(context.Context, int, int64) (telegram.CloseInvoker, error)
}

func (c *Client) downloadSnapshot(ctx context.Context, ref Reference) (downloadSnapshot, error) {
	raw, err := c.sourceRaw(ctx, ref)
	if err != nil {
		return downloadSnapshot{}, err
	}
	api, account, err := c.ready()
	if err != nil {
		return downloadSnapshot{}, err
	}
	fresh, ok := c.normalize(raw, account)
	if !ok || fresh.Reference.DocumentID == 0 {
		return downloadSnapshot{}, ErrNotFound
	}
	if ref.DocumentID != 0 && fresh.Reference.DocumentID != ref.DocumentID {
		return downloadSnapshot{}, errors.New("Telegram 原消息的文件已更改")
	}
	if ref.Size > 0 && fresh.Reference.Size != ref.Size {
		return downloadSnapshot{}, errors.New("Telegram 原文件大小与归档记录不符")
	}
	location, size, dc, err := mediaLocation(raw)
	if err != nil {
		return downloadSnapshot{}, err
	}
	if size < 0 {
		return downloadSnapshot{}, errors.New("Telegram 文件大小无效")
	}
	snapshot := downloadSnapshot{api: api, location: location, size: size}
	c.mu.RLock()
	mt := c.mt
	c.mu.RUnlock()
	// DC() exports/imports authorization even when asked for the primary DC.
	// Telegram rejects exporting an authorization to that same DC with
	// DC_ID_INVALID. Reuse the already authorized API for local files; only
	// files hosted in another DC need a separate, explicitly routed pool.
	if mt != nil && dc > 0 && dc != mt.Config().ThisDC {
		pool, err := mt.DC(ctx, dc, 1)
		if err != nil {
			return downloadSnapshot{}, c.safeError(err)
		}
		snapshot.api = tg.NewClient(c.middleware(pool))
		snapshot.closer = pool
	}
	return snapshot, nil
}

type telegramStream struct {
	reader *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (s *telegramStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *telegramStream) Close() error {
	s.once.Do(func() { s.cancel(); _ = s.reader.CloseWithError(context.Canceled) })
	<-s.done
	return nil
}

// OpenStream refreshes the source message before exposing a reader. A stale
// reference can be retried before any payload is delivered; after delivery has
// begun an error terminates the stream rather than duplicating a byte prefix.
func (c *Client) OpenStream(parent context.Context, ref Reference) (io.ReadCloser, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if err := c.checkReference(ref); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	snapshot, err := c.downloadSnapshot(ctx, ref)
	if err != nil {
		cancel()
		return nil, err
	}
	reader, writer := io.Pipe()
	stream := &telegramStream{reader: reader, cancel: cancel, done: make(chan struct{})}
	stopClose := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	go func() {
		defer close(stream.done)
		defer cancel()
		defer stopClose()
		var result error
		for attempt := 0; attempt < 2; attempt++ {
			written := &sizeWriter{Writer: writer, remaining: snapshot.size}
			_, result = downloader.NewDownloader().WithPartSize(telegramDownloadPartSize).Download(snapshot.api, snapshot.location).Stream(ctx, written)
			if snapshot.closer != nil {
				_ = snapshot.closer.Close()
			}
			if tgerr.Is(result, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID") && attempt == 0 && written.remaining == snapshot.size {
				snapshot, result = c.downloadSnapshot(ctx, ref)
				if result == nil {
					continue
				}
			}
			if result == nil && written.remaining != 0 {
				result = errStreamLength
			}
			break
		}
		if result != nil {
			if !errors.Is(result, errStreamLength) {
				result = c.safeError(result)
			}
			_ = writer.CloseWithError(result)
		} else {
			_ = writer.Close()
		}
	}()
	return stream, nil
}
