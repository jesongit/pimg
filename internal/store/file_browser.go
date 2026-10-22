package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pimg/internal/mediax"
)

// Browser entries are filesystem metadata, never managed files or archives.
type BrowserEntry struct {
	ImportPath
	Ext        string `json:"ext"`
	Kind       string `json:"kind"`
	MIME       string `json:"mime"`
	Version    string `json:"version"`
	ModifiedAt int64  `json:"modified_at"`
	Importable bool   `json:"importable"`
}

type BrowserLocation struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type BrowserListing struct {
	Path        string            `json:"path"`
	Parent      string            `json:"parent"`
	Breadcrumbs []BrowserLocation `json:"breadcrumbs"`
	Items       []BrowserEntry    `json:"items"`
	Total       int               `json:"total"`
	NextOffset  int               `json:"next_offset"`
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
	return BrowserEntry{ImportPath: ImportPath{Path: p, Name: filepath.Base(p), Directory: info.IsDir(), Size: info.Size()}, Ext: ext, Kind: media.Kind, MIME: media.Mime, Version: localVersion(info), ModifiedAt: info.ModTime().Unix(), Importable: !info.IsDir() && importable && media.Kind != "other"}
}

func (s *Service) BrowseLocalFiles(ctx context.Context, p, query string, offset int, ordering string) (BrowserListing, error) {
	switch ordering {
	case "", "name", "name_desc", "size_desc", "size_asc", "type", "type_desc":
	default:
		return BrowserListing{}, errors.New("目录排序参数无效")
	}
	if p == "" {
		p = s.InitialImportPath()
	}
	node, err := s.ImportPathInfo(p)
	if err != nil {
		return BrowserListing{}, err
	}
	if !node.Directory {
		node.Path = filepath.Dir(node.Path)
	}
	if offset < 0 || offset > 10_000_000 || len(query) > 512 {
		return BrowserListing{}, errors.New("目录查询参数无效")
	}
	root, err := openImportRoot(node.Path)
	if err != nil {
		return BrowserListing{}, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return BrowserListing{}, err
	}
	defer dir.Close()
	out := BrowserListing{Path: node.Path, Parent: filepath.Dir(node.Path), Items: []BrowserEntry{}}
	for current := node.Path; ; current = filepath.Dir(current) {
		name := filepath.Base(current)
		if current == filepath.Dir(current) {
			name = current
		}
		out.Breadcrumbs = append([]BrowserLocation{{Path: current, Name: name}}, out.Breadcrumbs...)
		if current == filepath.Dir(current) {
			break
		}
	}
	query = strings.ToLower(strings.TrimSpace(query))
	for {
		if err := ctx.Err(); err != nil {
			return BrowserListing{}, err
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || query != "" && !strings.Contains(strings.ToLower(entry.Name()), query) {
				continue
			}
			info, e := entry.Info()
			if e != nil || !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			out.Items = append(out.Items, s.browserEntry(filepath.Join(node.Path, entry.Name()), info))
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return BrowserListing{}, readErr
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.Directory != b.Directory {
			return a.Directory
		}
		if !a.Directory {
			switch ordering {
			case "size_desc", "size_asc":
				if a.Size != b.Size {
					if ordering == "size_desc" {
						return a.Size > b.Size
					}
					return a.Size < b.Size
				}
			case "type", "type_desc":
				if a.Ext != b.Ext {
					if ordering == "type_desc" {
						return a.Ext > b.Ext
					}
					return a.Ext < b.Ext
				}
			}
		}
		nameOrder := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		if nameOrder == 0 {
			nameOrder = cmp.Compare(a.Name, b.Name)
		}
		if ordering == "name_desc" {
			return nameOrder > 0
		}
		return nameOrder < 0
	})
	out.Total = len(out.Items)
	start := min(offset, out.Total)
	end := min(start+200, out.Total)
	if end < out.Total {
		out.NextOffset = end
	}
	out.Items = out.Items[start:end]
	return out, nil
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
	ext := ".mp4"
	if entry.Kind == "image" {
		ext = ".png"
	}
	key := sha256Hex([]byte(entry.Path + "\x00" + entry.Version))
	return filepath.Join(s.originalCacheDir(), "browser-"+key+"."+mode+ext)
}

func validBrowserMode(entry BrowserEntry, mode string) bool {
	return mode == "" || entry.Kind == "image" && mode == "image" || entry.Kind == "video" && (mode == "remux" || mode == "transcode")
}

func (s *Service) openBrowserCompatible(entry BrowserEntry, mode string) (*os.File, error) {
	file, err := os.Open(s.browserCompatiblePath(entry, mode))
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

// Probe streams when opening a video, including audio: a browser can otherwise
// render the picture while silently dropping an unsupported audio track.
func (s *Service) BrowserPlaybackMode(ctx context.Context, p, version string) (string, error) {
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
	if !info.HDR && info.PlayableMP4(entry.Ext) {
		return "", nil
	}
	if !info.HDR && info.AllVideoCompatible && info.AllAudioCompatible {
		return "remux", nil
	}
	return "transcode", nil
}

func (s *Service) PrepareBrowserView(ctx context.Context, p, version, mode string, retry bool) (OriginalView, error) {
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
	if file, e := s.openBrowserCompatible(entry, mode); e == nil {
		file.Close()
		return ready, nil
	}
	cache := s.media
	key := "browser:" + s.browserCompatiblePath(entry, mode)
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
	job := &originalJob{view: OriginalView{Status: "loading", Total: entry.Size}, cancel: cancel, done: make(chan struct{}), fileID: "browser:" + entry.Path}
	cache.jobs[key] = job
	cache.wg.Add(1)
	go func() {
		defer cache.wg.Done()
		defer close(job.done)
		defer cancel()
		var conversionErr error
		select {
		case cache.slots <- struct{}{}:
			conversionErr = s.makeBrowserCompatible(jobCtx, entry, mode)
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

func (s *Service) makeBrowserCompatible(ctx context.Context, entry BrowserEntry, mode string) error {
	source, _, err := s.OpenBrowserSource(entry.Path, entry.Version)
	if err != nil {
		return err
	}
	defer source.Close()
	root, err := openImportRoot(filepath.Dir(entry.Path))
	if err != nil {
		return err
	}
	defer root.Close()
	if err = os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		return err
	}
	// Snapshot only files which actually need compatibility processing. Converters
	// receive our private snapshot, never a path they could write in the source tree.
	input, err := os.CreateTemp(s.originalCacheDir(), "browser-source-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(input.Name())
	_, copyErr := io.Copy(input, importGuard{ctx: ctx, file: source, root: root, path: filepath.Base(entry.Path), version: entry.Version})
	if err = errors.Join(copyErr, input.Close()); err != nil {
		return err
	}
	output, err := os.CreateTemp(s.originalCacheDir(), "browser-view-*.part")
	if err != nil {
		return err
	}
	output.Close()
	defer os.Remove(output.Name())
	if err = mediax.MakeCompatible(ctx, input.Name(), output.Name(), entry.Kind, mode); err != nil {
		return err
	}
	check, _, err := s.OpenBrowserSource(entry.Path, entry.Version)
	if err != nil {
		return err
	}
	check.Close()
	if err = os.Rename(output.Name(), s.browserCompatiblePath(entry, mode)); err != nil {
		return fmt.Errorf("保存兼容查看缓存失败: %w", err)
	}
	return nil
}
