package store

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"image"
	"io"
	"os"
	"sync"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/mediax"
)

// Only small authenticated preview bytes are cached, never original files or
// file keys. Entries are immutable, bounded by bytes/count and expire promptly.
type previewEntry struct {
	key     string
	data    []byte
	expires time.Time
}
type previewFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	users  int
	data   []byte
	err    error
}
type previewCache struct {
	mu           sync.Mutex
	entries      map[string]*list.Element
	lru          *list.List
	bytes, limit int
	ttl          time.Duration
	flights      map[string]*previewFlight
	slots        chan struct{}
}

func newPreviewCache(limit int, ttl time.Duration) *previewCache {
	return &previewCache{entries: make(map[string]*list.Element), lru: list.New(), limit: limit, ttl: ttl, flights: make(map[string]*previewFlight), slots: make(chan struct{}, 2)}
}
func (c *previewCache) remove(e *list.Element) {
	value := e.Value.(*previewEntry)
	c.bytes -= len(value.data)
	delete(c.entries, value.key)
	c.lru.Remove(e)
}
func (c *previewCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		c.remove(entry)
	}
	if flight := c.flights[key]; flight != nil {
		delete(c.flights, key)
		flight.cancel()
	}
}
func (c *previewCache) get(ctx context.Context, key string, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	// Expiry is absolute, so continuously viewing one image cannot retain it forever.
	for e := c.lru.Back(); e != nil; {
		previous := e.Prev()
		if !time.Now().Before(e.Value.(*previewEntry).expires) {
			c.remove(e)
		}
		e = previous
	}
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		data := e.Value.(*previewEntry).data
		c.mu.Unlock()
		return data, nil
	}
	f := c.flights[key]
	if f == nil {
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		f = &previewFlight{done: make(chan struct{}), cancel: cancel}
		c.flights[key] = f
		go func() {
			defer cancel()
			select {
			case c.slots <- struct{}{}:
				defer func() { <-c.slots }()
				f.data, f.err = load(jobCtx)
			case <-jobCtx.Done():
				f.err = jobCtx.Err()
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			// A cancelled flight may already have been replaced by a fresh request.
			if c.flights[key] == f {
				delete(c.flights, key)
				if f.err == nil && jobCtx.Err() == nil && len(f.data) <= 2<<20 && len(f.data) <= c.limit {
					entry := &previewEntry{key: key, data: f.data, expires: time.Now().Add(c.ttl)}
					c.entries[key] = c.lru.PushFront(entry)
					c.bytes += len(f.data)
					for c.bytes > c.limit || c.lru.Len() > 512 {
						c.remove(c.lru.Back())
					}
				}
			}
			close(f.done)
		}()
	}
	f.users++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		f.users--
		if f.users == 0 && c.flights[key] == f {
			delete(c.flights, key)
			f.cancel()
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.data, f.err
	}
}

// PreviewThumbnail bypasses the archive worker queue and shares concurrent
// reads. The HTTP caller must authenticate and check the current file state.
func (s *Service) PreviewThumbnail(ctx context.Context, f *dbx.File) ([]byte, error) {
	// Existing local previews never queue behind reconstruction of large videos.
	if data, err := os.ReadFile(s.previewPath(f)); err == nil {
		if !f.HasThumb {
			if err = s.recordPreview(f, data); err != nil {
				return nil, err
			}
		}
		return data, nil
	}
	return s.previews.get(ctx, f.ID+"/"+f.SHA256+"/thumb", func(ctx context.Context) ([]byte, error) {
		if data, err := os.ReadFile(s.previewPath(f)); err == nil {
			if !f.HasThumb {
				if err = s.recordPreview(f, data); err != nil {
					return nil, err
				}
			}
			return data, nil
		}
		data, err := s.regeneratePreview(ctx, f)
		if err != nil {
			return nil, err
		}
		data, err = mediax.CompactPreview(data)
		if err != nil {
			return nil, err
		}
		s.media.mu.Lock()
		defer s.media.mu.Unlock()
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if _, err = s.DB.MutationFile(f.ID); err != nil {
			return nil, err
		}
		if err = s.saveLocalPreview(f, data); err != nil {
			return nil, err
		}
		if err = s.recordPreview(f, data); err != nil {
			return nil, err
		}
		return data, nil
	})
}

func (s *Service) recordPreview(f *dbx.File, data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.DB.SetThumbInfo(f.ID, true, cfg.Width, cfg.Height, "image/jpeg", int64(len(data)), f.HasBThumb)
}

// Regenerate from the verified original when the local preview is lost. The
// single-flight above bounds concurrency; no preview is published to a provider.
func (s *Service) regeneratePreview(ctx context.Context, f *dbx.File) ([]byte, error) {
	finish := s.BeginForegroundTransfer()
	defer finish()
	if f.MediaVersion == 2 {
		work, err := os.MkdirTemp(s.ctDir, "media-preview-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		inputs, err := s.mediaInputs(ctx, f, work)
		if err != nil {
			return nil, err
		}
		if len(inputs) == 0 {
			return nil, errors.New("没有可生成预览的副本")
		}
		chosen := inputs[0]
		if f.Kind == "video" {
			mid := f.Duration / 2
			for _, input := range inputs {
				if input.part.Start <= mid && input.part.Start+input.part.Duration > mid {
					chosen = input
					break
				}
			}
		}
		input, cleanup, err := s.materializeMediaInput(ctx, f, chosen, work)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		preview, err := mediax.MakePreview(input.path, f.Kind, mediax.PreviewMaxDimension)
		if err != nil {
			return nil, err
		}
		return preview.Data, nil
	}
	if cached, _, err := s.openCachedOriginal(f); err == nil {
		defer cached.Close()
		if preview, err := mediax.MakePreview(cached.Name(), f.Kind, mediax.PreviewMaxDimension); err == nil {
			return preview.Data, nil
		}
	}
	input, err := s.SourceStream(ctx, f)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	output, err := os.CreateTemp(s.ctDir, "preview-source-*.part")
	if err != nil {
		return nil, err
	}
	defer os.Remove(output.Name())
	hash := sha256New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(conversionReader{ctx: ctx, source: input}, f.Size+1))
	err = errors.Join(err, output.Close())
	if err != nil {
		return nil, err
	}
	if n != f.Size || hexOf(hash) != f.SHA256 {
		return nil, errors.New("生成预览的原件校验失败")
	}
	preview, err := mediax.MakePreview(output.Name(), f.Kind, mediax.PreviewMaxDimension)
	if err != nil {
		return nil, err
	}
	return preview.Data, nil
}
