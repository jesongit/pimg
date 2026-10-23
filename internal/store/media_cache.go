package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

// Originals are disposable, verified plaintext. Upload work files and durable
// album previews have separate directories and are never expired by this cache.
type mediaCache struct {
	mu     sync.Mutex
	jobs   map[string]*originalJob
	slots  chan struct{}
	wg     sync.WaitGroup
	closed bool
}
type originalJob struct {
	fileID string
	view   OriginalView
	cancel context.CancelFunc
	done   chan struct{}
}
type OriginalView struct {
	Status    string `json:"status"`
	Loaded    int64  `json:"loaded"`
	Total     int64  `json:"total"`
	Percent   int    `json:"percent"`
	Phase     string `json:"phase,omitempty"`
	Source    string `json:"source,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

func newMediaCache() *mediaCache {
	return &mediaCache{jobs: make(map[string]*originalJob), slots: make(chan struct{}, 2)}
}
func mediaKey(f *dbx.File) string { return sha256Hex([]byte(f.ID + "\x00" + f.SHA256)) }
func (s *Service) originalCacheDir() string {
	return filepath.Join(s.Cfg.Paths.RuntimeRoot(), "cache", "originals")
}
func (s *Service) previewDir() string { return filepath.Join(s.Cfg.Paths.RuntimeRoot(), "previews") }
func (s *Service) originalCachePath(f *dbx.File) string {
	return filepath.Join(s.originalCacheDir(), mediaKey(f)+".bin")
}
func (s *Service) previewPath(f *dbx.File) string {
	return filepath.Join(s.previewDir(), mediaKey(f)+"-v1.jpg")
}

func (s *Service) OriginalCacheHours() int {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.originalCacheHours
}
func (s *Service) SetOriginalCacheHours(hours int) {
	if hours < 1 || hours > 8760 {
		hours = 24
	}
	s.dyn.mu.Lock()
	s.dyn.originalCacheHours = hours
	s.dyn.mu.Unlock()
}

func (s *Service) openCachedOriginal(f *dbx.File) (*os.File, int64, error) {
	file, err := os.Open(s.originalCachePath(f))
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	expires := info.ModTime().Add(time.Duration(s.OriginalCacheHours()) * time.Hour).Unix()
	if !info.Mode().IsRegular() || info.Size() != f.Size || expires <= time.Now().Unix() {
		file.Close()
		return nil, 0, os.ErrNotExist
	}
	return file, expires, nil
}

// PrepareOriginal shares one bounded background load among all viewers. Polls
// report bytes actually written; 100% is reserved for full hash verification.
func (s *Service) PrepareOriginal(ctx context.Context, f *dbx.File, retry bool) (OriginalView, error) {
	if err := ctx.Err(); err != nil {
		return OriginalView{}, err
	}
	current, err := s.DB.MutationFile(f.ID)
	if err != nil {
		return OriginalView{}, err
	}
	if current.ArchiveState != "done" || current.PurgeState.String != "" {
		return OriginalView{}, errors.New("原件尚未就绪或正在删除")
	}
	f = current
	key := mediaKey(f)
	c := s.media
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return OriginalView{}, errors.New("服务正在停止")
	}
	if file, expires, err := s.openCachedOriginal(f); err == nil {
		file.Close()
		return OriginalView{Status: "ready", Loaded: f.Size, Total: f.Size, Percent: 100, Source: "local", ExpiresAt: expires}, nil
	}
	if j := c.jobs[key]; j != nil {
		if j.view.Status == "loading" || j.view.Status == "failed" && !retry {
			return j.view, nil
		}
		delete(c.jobs, key)
	}
	// Completed receipts are not a second, unbounded cache.
	for k, j := range c.jobs {
		if j.view.Status != "loading" {
			delete(c.jobs, k)
		}
	}
	if len(c.jobs) >= 8 {
		return OriginalView{}, errors.New("正在加载的原件较多，请稍后重试")
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	j := &originalJob{view: OriginalView{Status: "loading", Total: f.Size}, cancel: cancel, done: make(chan struct{})}
	c.jobs[key] = j
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(j.done)
		defer cancel()
		var err error
		select {
		case c.slots <- struct{}{}:
			err = s.loadOriginalCache(jobCtx, f, j)
			<-c.slots
		case <-jobCtx.Done():
			err = jobCtx.Err()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.jobs[key] != j {
			return
		}
		if err != nil {
			j.view.Status = "failed"
			j.view.Error = "原件加载失败，请重试。"
			return
		}
		j.view.Status = "ready"
		j.view.Loaded = f.Size
		j.view.Percent = 100
		j.view.ExpiresAt = time.Now().Add(time.Duration(s.OriginalCacheHours()) * time.Hour).Unix()
	}()
	return j.view, nil
}

type originalSource struct {
	name string
	open func() (io.ReadCloser, error)
}

func (s *Service) originalSources(ctx context.Context, f *dbx.File) []originalSource {
	var sources []originalSource
	addHot := func(id, scope, key string) {
		if b, e := s.hot(id); e == nil {
			if reader, ok := b.(storage.HotReader); ok {
				sources = append(sources, originalSource{id, func() (io.ReadCloser, error) { return reader.OpenObject(ctx, scope, key) }})
			}
		}
	}
	if f.Published && f.PublishedPath.String != "" {
		addHot(f.HotBackend, storage.Permanent, f.PublishedPath.String)
	}
	if obj, e := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix()); e == nil && obj.ReadyAt.Valid {
		addHot(obj.Backend, storage.Temporary, obj.ObjectKey)
	}
	targets := s.rankedArchives()
	targets = append(targets, f.ColdBackend)
	if objects, e := s.DB.ArchiveObjects(f.ID, ""); e == nil {
		for _, o := range objects {
			if o.Verified {
				targets = append(targets, o.InstanceID)
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range targets {
		if seen[id] {
			continue
		}
		seen[id] = true
		sourceCtx := context.WithValue(ctx, archiveSourceContext{}, id)
		sources = append(sources, originalSource{id, func() (io.ReadCloser, error) { return s.SourceStream(sourceCtx, f) }})
	}
	// Local replicas can avoid a network transfer even when a remote display
	// copy exists. Use the same observed health/throughput ranking for both.
	s.storages.mu.RLock()
	sort.SliceStable(sources, func(i, j int) bool {
		return s.storageLessLocked(sources[i].name, sources[j].name)
	})
	s.storages.mu.RUnlock()
	return sources
}

type progressWriter struct {
	target io.Writer
	update func(int)
}

func (w progressWriter) Write(data []byte) (int, error) {
	n, err := w.target.Write(data)
	w.update(n)
	return n, err
}

func (s *Service) loadOriginalCache(ctx context.Context, f *dbx.File, j *originalJob) (retErr error) {
	finish := s.BeginForegroundTransfer()
	defer finish()
	defer func() {
		if ctx.Err() == nil {
			if err := s.DB.RecordFileVerification(f, retErr); err != nil {
				s.Log.Warn("记录恢复校验结果失败", "file", f.ID)
			}
		}
	}()
	if err := os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		return err
	}
	var last error
	for _, source := range s.originalSources(ctx, f) {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.media.mu.Lock()
		j.view.Loaded = 0
		j.view.Percent = 0
		j.view.Source = source.name
		s.media.mu.Unlock()
		started := time.Now()
		input, err := source.open()
		if err != nil {
			s.recordStorageResult(source.name, 0, started, err)
			last = err
			continue
		}
		output, err := os.CreateTemp(s.originalCacheDir(), "load-*.part")
		if err != nil {
			input.Close()
			return err
		}
		_ = output.Chmod(0600)
		h := sha256New()
		writer := progressWriter{io.MultiWriter(output, h), func(n int) {
			s.media.mu.Lock()
			defer s.media.mu.Unlock()
			j.view.Loaded += int64(n)
			if f.Size > 0 {
				j.view.Percent = int(min(99, j.view.Loaded*100/f.Size))
			}
		}}
		count, copyErr := io.Copy(writer, io.LimitReader(contextReaderForCache{ctx, input}, f.Size+1))
		input.Close()
		if copyErr == nil && (count != f.Size || hexOf(h) != f.SHA256) {
			copyErr = errors.New("原件完整性校验失败")
		}
		if copyErr == nil {
			copyErr = output.Sync()
		}
		copyErr = errors.Join(copyErr, output.Close())
		if copyErr == nil {
			fresh, err := s.DB.MutationFile(f.ID)
			copyErr = err
			if err == nil && (fresh.PurgeState.String != "" || fresh.SHA256 != f.SHA256 || fresh.ArchiveState != "done") {
				copyErr = dbx.ErrNotFound
			}
		}
		s.media.mu.Lock()
		if copyErr == nil && (ctx.Err() != nil || s.media.jobs[mediaKey(f)] != j) {
			copyErr = context.Canceled
		}
		if copyErr == nil {
			copyErr = os.Rename(output.Name(), s.originalCachePath(f))
		}
		s.media.mu.Unlock()
		s.recordStorageResult(source.name, count, started, copyErr)
		if copyErr == nil {
			return nil
		}
		os.Remove(output.Name())
		last = copyErr
	}
	return last
}

type contextReaderForCache struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReaderForCache) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// CachedOriginal never starts network work. The HTTP layer rechecks access on
// every request, including a cache hit and each video range request.
func (s *Service) CachedOriginal(f *dbx.File) (*os.File, error) {
	current, err := s.DB.MutationFile(f.ID)
	if err != nil {
		return nil, err
	}
	if current.PurgeState.String != "" {
		return nil, dbx.ErrNotFound
	}
	if current.ArchiveState != "done" {
		return nil, dbx.ErrNotFound
	}
	if current.MediaVersion == 2 {
		file, _, e := s.OpenMediaView(context.Background(), current, 0, "", false)
		return file, e
	}
	file, _, err := s.openCachedOriginal(current)
	return file, err
}

func (s *Service) ViewOriginal(ctx context.Context, f *dbx.File) (*os.File, error) {
	if f.MediaVersion == 2 {
		fresh, e := s.mutableFile(f.ID)
		if e != nil {
			return nil, e
		}
		file, _, e := s.OpenMediaView(ctx, fresh, 0, "", true)
		return file, e
	}
	view, err := s.PrepareOriginal(ctx, f, true)
	if err != nil {
		return nil, err
	}
	if view.Status == "ready" {
		return s.CachedOriginal(f)
	}
	s.media.mu.Lock()
	job := s.media.jobs[mediaKey(f)]
	s.media.mu.Unlock()
	if job == nil {
		return nil, errors.New("原件加载已停止，请重试")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-job.done:
		return s.CachedOriginal(f)
	}
}

// Reuse a verified upload without immediately downloading it from Telegram.
func (s *Service) seedOriginalCache(f *dbx.File, plain string) {
	if info, err := os.Stat(plain); err != nil || info.Size() != f.Size {
		return
	}
	if os.MkdirAll(s.originalCacheDir(), 0700) != nil {
		return
	}
	s.media.mu.Lock()
	defer s.media.mu.Unlock()
	if s.media.jobs[mediaKey(f)] != nil {
		return
	}
	if os.Rename(plain, s.originalCachePath(f)) == nil {
		now := time.Now()
		_ = os.Chtimes(s.originalCachePath(f), now, now)
	}
}

func (s *Service) ForgetMediaViews(f *dbx.File) {
	if f.MediaVersion == 2 {
		if rows, err := s.DB.Query(`SELECT sha256 FROM media_publications WHERE file_id=? UNION SELECT m.sha256 FROM media_temporary m JOIN temp_objects t ON t.id=m.object_id WHERE t.file_id=?`, f.ID, f.ID); err == nil {
			var sums []string
			for rows.Next() {
				var sum string
				if rows.Scan(&sum) == nil {
					sums = append(sums, sum)
				}
			}
			rows.Close()
			for _, sum := range sums {
				_ = os.Remove(s.presentationCachePath(f, sum))
			}
		}
		if replicas, err := s.DB.MediaReplicas(f.ID); err == nil {
			for _, r := range replicas {
				for _, p := range r.Parts {
					copy := mediaPartFile(f, r, p)
					_ = os.Remove(s.originalCachePath(copy))
					for _, mode := range []string{"image", "remux", "transcode"} {
						_ = os.Remove(s.mediaCompatiblePath(f, p, mode))
					}
				}
			}
		}
	}
	s.previews.forget(f.ID + "/" + f.SHA256 + "/thumb")
	c := s.media
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, job := range c.jobs {
		if job.fileID == f.ID {
			job.cancel()
			delete(c.jobs, key)
		}
	}
	if j := c.jobs[mediaKey(f)]; j != nil {
		j.cancel()
		delete(c.jobs, mediaKey(f))
	}
	_ = os.Remove(s.originalCachePath(f))
	_ = os.Remove(s.previewPath(f))
}

func (s *Service) StopMediaLoads() {
	c := s.media
	c.mu.Lock()
	c.closed = true
	for _, j := range c.jobs {
		j.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// CleanupMediaCache enforces absolute TTL after restart and a 4 GiB LRU-style
// oldest-fill budget (one larger file may stand alone). In-use Windows files
// remain open safely and are retried at the next sweep.
func (s *Service) CleanupMediaCache() error {
	_, err := s.CleanupMediaCacheResult()
	return err
}

func (s *Service) CleanupMediaCacheResult() (int, error) {
	removed := 0
	entries, err := os.ReadDir(s.originalCacheDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s.media.mu.Lock()
	defer s.media.mu.Unlock()
	active := false
	for _, j := range s.media.jobs {
		if j.view.Status == "loading" {
			active = true
			break
		}
	}
	var files []os.FileInfo
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(s.originalCacheDir(), entry.Name())
		if filepath.Ext(entry.Name()) == ".part" {
			if !active {
				if os.Remove(path) == nil {
					removed++
				}
			}
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".bin" && ext != ".png" && ext != ".mp4" {
			continue
		}
		if time.Since(info.ModTime()) >= time.Duration(s.OriginalCacheHours())*time.Hour {
			if os.Remove(path) == nil {
				removed++
			}
			continue
		}
		files = append(files, info)
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
	for _, file := range files {
		if total <= 4<<30 || total == file.Size() {
			break
		}
		if os.Remove(filepath.Join(s.originalCacheDir(), file.Name())) == nil {
			removed++
			total -= file.Size()
		}
	}
	return removed, nil
}

func (s *Service) saveLocalPreview(f *dbx.File, data []byte) error {
	if err := os.MkdirAll(s.previewDir(), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.previewDir(), "preview-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_ = file.Chmod(0600)
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), s.previewPath(f)); err != nil {
		return fmt.Errorf("保存压缩预览失败: %w", err)
	}
	return nil
}
