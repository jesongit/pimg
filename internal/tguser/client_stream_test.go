package tguser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type fakeMediaDC struct {
	primary int
	opened  []int
	pool    *fakeMediaPool
	err     error
}

func (d *fakeMediaDC) Config() tg.Config { return tg.Config{ThisDC: d.primary} }
func (d *fakeMediaDC) DC(_ context.Context, dc int, _ int64) (telegram.CloseInvoker, error) {
	d.opened = append(d.opened, dc)
	if dc == d.primary {
		return nil, tgerr.New(400, "DC_ID_INVALID")
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.pool, nil
}

type fakeMediaPool struct {
	invoke telegram.InvokeFunc
	closed int
}

func (p *fakeMediaPool) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	return p.invoke(ctx, in, out)
}
func (p *fakeMediaPool) Close() error { p.closed++; return nil }

func TestUserDownloadReusesPrimaryDCAuthorization(t *testing.T) {
	payload := []byte("same-dc-carrier-and-encrypted-data")
	c, _ := streamFake(t, int64(len(payload)), func(_ int, req *tg.UploadGetFileRequest) ([]byte, error) {
		if req.Offset != 0 {
			t.Fatalf("unexpected offset %d", req.Offset)
		}
		return payload, nil
	})
	dataCenters := &fakeMediaDC{primary: 2}
	c.mt = dataCenters
	got, err := c.Download(context.Background(), Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, Size: int64(len(payload))})
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("same-DC download: got=%q err=%v", got, err)
	}
	if len(dataCenters.opened) != 0 {
		t.Fatalf("same-DC file attempted authorization export: %v", dataCenters.opened)
	}
}

func TestUserDownloadRoutesRemoteDCAndClosesPool(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			payload := []byte("remote-dc-carrier")
			primaryFileCalls := 0
			c, _ := streamFake(t, int64(len(payload)), func(_ int, _ *tg.UploadGetFileRequest) ([]byte, error) {
				primaryFileCalls++
				return nil, errors.New("wrong download DC")
			})
			remoteFileCalls := 0
			pool := &fakeMediaPool{invoke: func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				req, ok := in.(*tg.UploadGetFileRequest)
				if !ok {
					t.Fatalf("unexpected remote RPC %T", in)
				}
				remoteFileCalls++
				if req.Location.(*tg.InputDocumentFileLocation).ID != 81 {
					t.Fatal("remote route lost file identity")
				}
				if failure {
					return tgerr.New(400, "FILE_ID_INVALID")
				}
				out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: payload}
				return nil
			}}
			dataCenters := &fakeMediaDC{primary: 4, pool: pool}
			c.mt = dataCenters
			got, err := c.Download(context.Background(), Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, Size: int64(len(payload))})
			if failure {
				if err == nil {
					t.Fatal("remote transfer failure suppressed")
				}
			} else if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("remote download: got=%q err=%v", got, err)
			}
			if primaryFileCalls != 0 || remoteFileCalls != 1 || len(dataCenters.opened) != 1 || dataCenters.opened[0] != 2 || pool.closed != 1 {
				t.Fatalf("route/close primary=%d remote=%d dcs=%v closes=%d", primaryFileCalls, remoteFileCalls, dataCenters.opened, pool.closed)
			}
		})
	}
}

type generatedVideoReader struct {
	remaining int64
	maxRead   int
	read      int64
}

type enteredReadCloser struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (r *enteredReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.ReadCloser.Read(p)
}

func (r *generatedVideoReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	for i := range p {
		p[i] = 0x5a
	}
	n := len(p)
	r.remaining -= int64(n)
	r.read += int64(n)
	return n, nil
}

func TestUserUploadStreamUsesBoundedPartsForLargeVideo(t *testing.T) {
	const size int64 = 12 << 20
	source := &generatedVideoReader{remaining: size}
	var received int64
	parts := 0
	sends := 0
	var mu sync.Mutex
	seen := map[int]bool{}
	active, peak := 0, 0
	firstWave := make(chan struct{})
	c := fakeClient(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.UploadSaveBigFilePartRequest:
			if len(req.Bytes) != 512<<10 || req.FilePart < 0 || req.FilePart >= 24 || req.FileTotalParts != 24 {
				return fmt.Errorf("invalid bounded protocol part: %d/%d/%d", len(req.Bytes), req.FilePart, req.FileTotalParts)
			}
			for _, b := range req.Bytes {
				if b != 0x5a {
					return errors.New("stream data changed")
				}
			}
			mu.Lock()
			if seen[req.FilePart] {
				mu.Unlock()
				return errors.New("duplicate protocol part")
			}
			seen[req.FilePart] = true
			received += int64(len(req.Bytes))
			parts++
			active++
			if active > peak {
				peak = active
			}
			if parts == 4 {
				close(firstWave)
			}
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			select {
			case <-firstWave:
			case <-ctx.Done():
				return ctx.Err()
			}
			out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		case *tg.MessagesSendMediaRequest:
			sends++
			media := req.Media.(*tg.InputMediaUploadedDocument)
			if media.ForceFile || !media.NosoundVideo || req.RandomID != 777 {
				t.Fatal("video mode or stable operation id lost")
			}
			if _, ok := media.File.(*tg.InputFileBig); !ok {
				t.Fatal("known large size did not use big-file RPC")
			}
			attr := media.Attributes[1].(*tg.DocumentAttributeVideo)
			if attr.Duration != 30 || attr.W != 1280 || attr.H != 720 || !attr.SupportsStreaming {
				t.Fatal("stream metadata lost")
			}
			m := docMessage(41, 9, 7, nil, "fresh")
			m.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).Size = size
			out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: m}}}
		default:
			t.Fatalf("unexpected stream RPC %T", in)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ref, err := c.UploadStream(ctx, 41, 7, "segment.mp4", "video/mp4", source, size, nil, "app:pimg video", 777, &VideoMetadata{Duration: 30, Width: 1280, Height: 720, SupportsStreaming: true})
	if err != nil || ref.Size != size || source.read != size || received != size || parts != 24 || sends != 1 || source.maxRead > 512<<10 || peak != 4 || active != 0 {
		t.Fatal(ref, err, received, parts, sends, source.maxRead, peak, active)
	}
}

func TestUserUploadStreamCancellationStopsAllParallelParts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	active := 0
	started := make(chan struct{})
	c := fakeClient(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.UploadSaveBigFilePartRequest); !ok {
			return fmt.Errorf("canceled upload unexpectedly published: %T", in)
		}
		mu.Lock()
		active++
		if active == 4 {
			close(started)
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		_, err := c.UploadStream(ctx, 41, 7, "cancel.bin", "application/octet-stream", &generatedVideoReader{remaining: 12 << 20}, 12<<20, nil, "", 777, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("parallel upload did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left parallel upload running")
	}
	mu.Lock()
	defer mu.Unlock()
	if active != 0 {
		t.Fatal("upload returned with active RPCs", active)
	}
}

func TestUserOpenStreamUsesBoundedOneMiBRequests(t *testing.T) {
	const size = 3<<20 + 17
	payload := bytes.Repeat([]byte{0x6d}, size)
	var offsets []int64
	c, _ := streamFake(t, size, func(_ int, req *tg.UploadGetFileRequest) ([]byte, error) {
		if req.Limit != 1<<20 {
			return nil, fmt.Errorf("unexpected download block: %d", req.Limit)
		}
		offsets = append(offsets, req.Offset)
		return payload[req.Offset:min(req.Offset+int64(req.Limit), size)], nil
	})
	reader, err := c.OpenStream(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, Size: size})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, payload) || len(offsets) != 4 {
		t.Fatal("download bytes/request count", len(got), offsets, err)
	}
	for i, offset := range offsets {
		if offset != int64(i)<<20 {
			t.Fatal("out-of-order stream", offsets)
		}
	}
}

func TestUserUploadStreamRejectsShortAndLongBeforeMessageCreation(t *testing.T) {
	for _, actual := range []string{"123", "12345"} {
		t.Run(actual, func(t *testing.T) {
			sends := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch in.(type) {
				case *tg.UploadSaveFilePartRequest:
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
				case *tg.MessagesSendMediaRequest:
					sends++
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			_, err := c.UploadStream(context.Background(), 41, 7, "carrier.png", "image/png", bytes.NewBufferString(actual), 4, nil, "", 1, nil)
			if !errors.Is(err, errStreamLength) || sends != 0 {
				t.Fatal("bad size published a message", err, sends)
			}
		})
	}
}

func TestUserUploadStreamCancellationClosesBlockedSource(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		t.Fatal("blocked source unexpectedly reached RPC")
		return nil
	})
	reader, writer := io.Pipe()
	defer writer.Close()
	blocked := &enteredReadCloser{ReadCloser: reader, entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.UploadStream(ctx, 41, 7, "carrier.png", "image/png", blocked, 100, nil, "", 1, nil)
		done <- err
	}()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("upload source read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled upload left input read blocked")
	}
}

func streamFake(t *testing.T, size int64, behavior func(int, *tg.UploadGetFileRequest) ([]byte, error)) (*Client, *int) {
	t.Helper()
	reads := 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.ChannelsGetMessagesRequest:
			reads++
			m := docMessage(41, 9, 7, nil, "refreshed")
			m.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).Size = size
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{m}}
		case *tg.UploadGetFileRequest:
			data, err := behavior(reads, req)
			if err != nil {
				return err
			}
			out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: data}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	return c, &reads
}

func TestUserOpenStreamRefreshesBeforeDeliveryAndVerifiesLength(t *testing.T) {
	payload := bytes.Repeat([]byte("raw-carrier-bytes"), 90000)
	c, reads := streamFake(t, int64(len(payload)), func(read int, req *tg.UploadGetFileRequest) ([]byte, error) {
		if read == 1 {
			return nil, tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		}
		start := int(req.Offset)
		end := start + req.Limit
		if start > len(payload) {
			start = len(payload)
		}
		if end > len(payload) {
			end = len(payload)
		}
		return payload[start:end], nil
	})
	reader, err := c.OpenStream(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, DocumentID: 81, Size: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, payload) || *reads != 2 {
		t.Fatal("source refresh or raw bytes incorrect", len(got), err, *reads)
	}
	for _, actual := range []int{2, 4} {
		t.Run(string(rune('0'+actual)), func(t *testing.T) {
			c, _ := streamFake(t, 3, func(_ int, _ *tg.UploadGetFileRequest) ([]byte, error) { return bytes.Repeat([]byte{'x'}, actual), nil })
			reader, err := c.OpenStream(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, Size: 3})
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(reader)
			_ = reader.Close()
			if !errors.Is(err, errStreamLength) {
				t.Fatal("wrong download length accepted", err)
			}
		})
	}
}

func TestUserOpenStreamNeverDuplicatesPrefixOnLateReferenceExpiry(t *testing.T) {
	var prefix []byte
	c, reads := streamFake(t, 2<<20, func(_ int, req *tg.UploadGetFileRequest) ([]byte, error) {
		if req.Offset != 0 {
			return nil, tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		}
		prefix = bytes.Repeat([]byte{'p'}, req.Limit)
		return prefix, nil
	})
	reader, err := c.OpenStream(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, Size: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err == nil || !bytes.Equal(got, prefix) || *reads != 1 {
		t.Fatal("late refresh duplicated prefix", len(got), err, *reads)
	}
}

func TestUserOpenStreamCloseCancelsInFlightRPC(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	c := fakeClient(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		switch in.(type) {
		case *tg.ChannelsGetMessagesRequest:
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{docMessage(41, 9, 7, []byte("data"), "new")}}
		case *tg.UploadGetFileRequest:
			once.Do(func() { close(entered) })
			<-ctx.Done()
			close(exited)
			return ctx.Err()
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	reader, err := c.OpenStream(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("download not started")
	}
	closed := make(chan struct{})
	go func() { _ = reader.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel RPC")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before transfer stopped")
	}
}
