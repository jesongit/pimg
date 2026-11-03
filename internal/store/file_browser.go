package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pimg/internal/mediax"
)

// Browser entries are filesystem metadata, never managed files or archives.
type BrowserEntry struct {
	ImportPath
	Ext          string `json:"ext"`
	Kind         string `json:"kind"`
	MIME         string `json:"mime"`
	Version      string `json:"version"`
	ModifiedAt   int64  `json:"modified_at"`
	CreatedAt    int64  `json:"created_at"`
	RelativePath string `json:"relative_path"`
	Importable   bool   `json:"importable"`
	modifiedNS   int64
	createdNS    int64
}

type BrowserLocation struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type BrowserListing struct {
	Path               string            `json:"path"`
	Parent             string            `json:"parent"`
	Breadcrumbs        []BrowserLocation `json:"breadcrumbs"`
	Items              []BrowserEntry    `json:"items"`
	Total              int               `json:"total"`
	NextOffset         int               `json:"next_offset"`
	Recursive          bool              `json:"recursive"`
	SkippedDirectories int               `json:"skipped_directories"`
	Snapshot           string            `json:"snapshot,omitempty"`
	ScanLimited        bool              `json:"scan_limited,omitempty"`
	Indexed            bool              `json:"indexed,omitempty"`
	IndexRoot          string            `json:"index_root,omitempty"`
	IndexBuilding      bool              `json:"index_building,omitempty"`
	IndexUpdating      bool              `json:"index_updating,omitempty"`
	IndexError         string            `json:"index_error,omitempty"`
}

func (s *Service) browserEntry(p string, info fs.FileInfo) BrowserEntry {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	media := mediax.Detect(ext, "")
	importable := false
	for _, allowed := range s.AllowedExts() {
		if strings.EqualFold(allowed, ext) {
			importable = true
			break
		}
	}
	return BrowserEntry{ImportPath: ImportPath{Path: p, Name: filepath.Base(p), Directory: info.IsDir(), Size: info.Size()}, Ext: ext, Kind: media.Kind, MIME: media.Mime, Version: localVersion(info), ModifiedAt: info.ModTime().Unix(), modifiedNS: info.ModTime().UnixNano(), Importable: !info.IsDir() && importable && media.Kind != "other"}
}

// Open every source read-only through os.Root; check the opened file as well as
// the directory entry so replacement races cannot reuse a previous view.
func (s *Service) OpenBrowserSource(p, version string) (*os.File, BrowserEntry, error) {
	abs, err := s.absoluteImportPath(p)
	if err != nil || version == "" {
		return nil, BrowserEntry{}, errors.New("文件参数无效，请刷新目录")
	}
	root, err := openImportRoot(filepath.Dir(abs))
	if err != nil {
		return nil, BrowserEntry{}, err
	}
	defer root.Close()
	info, err := importInfo(root, filepath.Base(abs))
	if err != nil {
		return nil, BrowserEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, BrowserEntry{}, errors.New("只能查看普通媒体文件")
	}
	entry := s.browserEntry(abs, info)
	if entry.Version != version {
		return nil, entry, ErrSourceChanged
	}
	if entry.Kind != "image" && entry.Kind != "video" {
		return nil, entry, errors.New("此格式不支持在线查看")
	}
	file, err := root.Open(filepath.Base(abs))
	if err != nil {
		return nil, entry, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || localVersion(opened) != version {
		file.Close()
		return nil, entry, ErrSourceChanged
	}
	return file, entry, nil
}

func (s *Service) browserCompatiblePath(entry BrowserEntry, mode string) string {
	return s.browserCompatiblePathFor(entry, mode, "")
}

// browserCompatiblePathFor places a remux produced for a non-strict browser
// capability set under a fingerprinted name: those outputs keep the original
// HEVC/AV1/AC-3 streams and must never be served to a weaker browser.
func (s *Service) browserCompatiblePathFor(entry BrowserEntry, mode, fp string) string {
	ext := ".mp4"
	if entry.Kind == "image" {
		ext = ".png"
	}
	key := sha256Hex([]byte(entry.Path + "\x00" + entry.Version))
	if mode == "remux" && fp != "" {
		return filepath.Join(s.originalCacheDir(), "browser-"+key+"."+mode+"-"+fp+ext)
	}
	return filepath.Join(s.originalCacheDir(), "browser-"+key+"."+mode+ext)
}

func validBrowserMode(entry BrowserEntry, mode string) bool {
	return mode == "" || entry.Kind == "image" && mode == "image" || entry.Kind == "video" && (mode == "remux" || mode == "transcode")
}

func (s *Service) openBrowserCompatible(entry BrowserEntry, mode string) (*os.File, error) {
	return s.openBrowserCompatibleFor(entry, mode, "")
}

func (s *Service) openBrowserCompatibleFor(entry BrowserEntry, mode, fp string) (*os.File, error) {
	file, err := os.Open(s.browserCompatiblePathFor(entry, mode, fp))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || time.Since(info.ModTime()) >= time.Duration(s.OriginalCacheHours())*time.Hour {
		file.Close()
		return nil, errors.New("兼容缓存已过期")
	}
	return file, nil
}

func (s *Service) OpenBrowserContent(p, version, mode string) (*os.File, BrowserEntry, error) {
	source, entry, err := s.OpenBrowserSource(p, version)
	if err != nil {
		return nil, entry, err
	}
	if !validBrowserMode(entry, mode) {
		source.Close()
		return nil, entry, errors.New("查看方式与媒体类型不匹配")
	}
	if mode == "" {
		return source, entry, nil
	}
	source.Close()
	file, err := s.openBrowserCompatible(entry, mode)
	return file, entry, err
}

var ErrBrowserViewBusy = errors.New("正在准备的文件较多，请稍后重试")

// StreamBrowserCompatible plays a compatible video without waiting for the
// whole conversion. When the cached file already exists it is returned for
// normal range serving. Otherwise the caller becomes the stream leader: the
// conversion writes fragmented MP4 to both the cache and w while this call
// blocks, then returns (nil, true, nil) once the client stream is complete.
// The streamed flag marks that response bytes may already be written, so late
// failures can only abort the body. Waiting openers get the finished cache
// file instead.
func (s *Service) StreamBrowserCompatible(ctx context.Context, p, version, mode string, caps mediax.BrowserCaps, w io.Writer) (file *os.File, streamed bool, err error) {
	source, entry, openErr := s.OpenBrowserSource(p, version)
	if openErr != nil {
		return nil, false, openErr
	}
	defer source.Close()
	if entry.Kind != "video" || !validBrowserMode(entry, mode) || mode == "" {
		return nil, false, errors.New("查看方式与媒体类型不匹配")
	}
	fp := mediax.CapsFingerprint(caps)
	if cached, e := s.openBrowserCompatibleFor(entry, mode, fp); e == nil {
		return cached, false, nil
	}
	key := "stream:" + s.browserCompatiblePathFor(entry, mode, fp)
	cache := s.media
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return nil, false, errors.New("服务正在停止")
	}
	if existing := cache.streams[key]; existing != nil {
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-existing.done:
		}
		if existing.err != nil {
			return nil, false, existing.err
		}
		cached, e := s.openBrowserCompatibleFor(entry, mode, fp)
		if e != nil {
			return nil, false, errors.New("兼容版本已生成，请重新打开文件")
		}
		return cached, false, nil
	}
	if len(cache.streams) >= 8 {
		cache.mu.Unlock()
		return nil, false, ErrBrowserViewBusy
	}
	leader := &browserStream{done: make(chan struct{})}
	cache.streams[key] = leader
	cache.mu.Unlock()

	defer func() {
		cache.mu.Lock()
		delete(cache.streams, key)
		cache.mu.Unlock()
		close(leader.done)
	}()
	cache.streamSlots <- struct{}{}
	defer func() { <-cache.streamSlots }()
	if err = os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		leader.err = err
		return nil, false, err
	}
	dest := s.browserCompatiblePathFor(entry, mode, fp)
	// From here the response body may already be flowing; any failure can only
	// abort the stream and let the video element try the next playback mode.
	streamed = true
	// Linux subprocesses inherit the validated read-only descriptor, so a
	// multi-GB source streams without a full copy first.
	if err = mediax.MakeCompatibleFileStream(ctx, source, dest, mode, caps, w); err != nil {
		leader.err = err
		return nil, true, err
	}
	// The leader has already streamed the response body; followers may serve
	// the completed cache file.
	return nil, true, nil
}

// Probe streams when opening a video, including audio: a browser can otherwise
// render the picture while silently dropping an unsupported audio track. caps
// reports what the requesting browser decodes; the zero value is the strict
// universal baseline.
func (s *Service) BrowserPlaybackMode(ctx context.Context, p, version string, caps mediax.BrowserCaps) (string, error) {
	source, entry, err := s.OpenBrowserSource(p, version)
	if err != nil {
		return "", err
	}
	defer source.Close()
	if entry.Kind != "video" {
		return "", nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := mediax.InspectVideo(probeCtx, source.Name())
	if err != nil {
		return "", err
	}
	if !info.HDR && info.SupportedMP4(entry.Ext, caps) {
		return "", nil
	}
	if !info.HDR && info.AllVideoSupported(caps) && info.AllAudioSupported(caps) {
		return "remux", nil
	}
	return "transcode", nil
}

func (s *Service) PrepareBrowserView(ctx context.Context, p, version, mode string, retry bool, caps mediax.BrowserCaps) (OriginalView, error) {
	source, entry, err := s.OpenBrowserSource(p, version)
	if err != nil {
		return OriginalView{}, err
	}
	defer source.Close()
	if !validBrowserMode(entry, mode) {
		return OriginalView{}, errors.New("查看方式与媒体类型不匹配")
	}
	ready := OriginalView{Status: "ready", Total: entry.Size, Loaded: entry.Size, Percent: 100, Source: "filesystem"}
	if mode == "" {
		return ready, nil
	}
	fp := mediax.CapsFingerprint(caps)
	if file, e := s.openBrowserCompatibleFor(entry, mode, fp); e == nil {
		file.Close()
		return ready, nil
	}
	cache := s.media
	key := "browser:" + s.browserCompatiblePathFor(entry, mode, fp)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return OriginalView{}, errors.New("服务正在停止")
	}
	if job := cache.jobs[key]; job != nil && (job.view.Status == "loading" || job.view.Status == "failed" && !retry) {
		return job.view, nil
	}
	for k, job := range cache.jobs {
		if job.view.Status != "loading" {
			delete(cache.jobs, k)
		}
	}
	if len(cache.jobs) >= 8 {
		return OriginalView{}, ErrBrowserViewBusy
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	job := &originalJob{view: OriginalView{Status: "loading", Total: entry.Size, Phase: "queued"}, cancel: cancel, done: make(chan struct{}), fileID: "browser:" + entry.Path}
	cache.jobs[key] = job
	cache.wg.Add(1)
	go func() {
		defer cache.wg.Done()
		defer close(job.done)
		defer cancel()
		var conversionErr error
		select {
		case cache.slots <- struct{}{}:
			conversionErr = s.makeBrowserCompatible(jobCtx, entry, mode, caps, fp, func(phase string, percent int) {
				cache.mu.Lock()
				job.view.Phase, job.view.Percent = phase, max(job.view.Percent, min(99, percent))
				cache.mu.Unlock()
			})
			<-cache.slots
		case <-jobCtx.Done():
			conversionErr = jobCtx.Err()
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if conversionErr != nil {
			job.view.Status, job.view.Error = "failed", conversionErr.Error()
		} else {
			job.view = ready
		}
	}()
	return job.view, nil
}

func (s *Service) makeBrowserCompatible(ctx context.Context, entry BrowserEntry, mode string, caps mediax.BrowserCaps, fp string, progress func(string, int)) error {
	source, _, err := s.OpenBrowserSource(entry.Path, entry.Version)
	if err != nil {
		return err
	}
	defer source.Close()
	if err = os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		return err
	}
	output, err := os.CreateTemp(s.originalCacheDir(), "browser-view-*.part")
	if err != nil {
		return err
	}
	output.Close()
	defer os.Remove(output.Name())
	phase := mode
	if entry.Kind == "image" {
		phase = "image"
	}
	update := func(value float64) { progress(phase, int(value*99)) }
	if runtime.GOOS == "linux" && entry.Kind == "video" {
		progress(phase, 0)
		err = mediax.MakeCompatibleFile(ctx, source, output.Name(), mode, caps, update)
	} else {
		// Platforms without inherited seekable descriptors retain the guarded
		// snapshot path. Images keep the same converter isolation as before.
		root, e := openImportRoot(filepath.Dir(entry.Path))
		if e != nil {
			return e
		}
		defer root.Close()
		input, e := os.CreateTemp(s.originalCacheDir(), "browser-source-*.part")
		if e != nil {
			return e
		}
		defer os.Remove(input.Name())
		progress("copy", 0)
		var copied int64
		_, copyErr := io.Copy(progressWriter{input, func(n int) { copied += int64(n); progress("copy", int(copied*10/max(1, entry.Size))) }}, importGuard{ctx: ctx, file: source, root: root, path: filepath.Base(entry.Path), version: entry.Version})
		if e = errors.Join(copyErr, input.Close()); e != nil {
			return e
		}
		progress(phase, 10)
		err = mediax.MakeCompatibleCapsProgress(ctx, input.Name(), output.Name(), entry.Kind, mode, caps, func(value float64) { progress(phase, 10+int(value*89)) })
	}
	if err != nil {
		return err
	}
	check, _, err := s.OpenBrowserSource(entry.Path, entry.Version)
	if err != nil {
		return err
	}
	before, beforeErr := source.Stat()
	after, afterErr := check.Stat()
	check.Close()
	if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) {
		return ErrSourceChanged
	}
	if err = os.Rename(output.Name(), s.browserCompatiblePathFor(entry, mode, fp)); err != nil {
		return fmt.Errorf("保存兼容查看缓存失败: %w", err)
	}
	return nil
}
