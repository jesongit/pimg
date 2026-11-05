package server

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pimg/internal/mediax"
	"pimg/internal/store"
)

func (s *Server) registerFileBrowser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/file-browser", s.auth(s.handleFileBrowser))
	mux.HandleFunc("GET /api/admin/file-browser/entry", s.auth(s.handleBrowserEntry))
	mux.HandleFunc("POST /api/admin/file-browser/view", s.auth(s.handleBrowserView))
	mux.HandleFunc("GET /api/admin/file-browser/content", s.auth(s.handleBrowserContent))
	mux.HandleFunc("POST /api/admin/file-browser/extract", s.auth(s.handleBrowserExtract))
	mux.HandleFunc("GET /api/admin/file-browser/extract", s.auth(s.handleBrowserExtractStatus))
	mux.HandleFunc("POST /api/admin/file-browser/mkdir", s.auth(s.handleBrowserMkdir))
	mux.HandleFunc("POST /api/admin/file-browser/hardlink", s.auth(s.handleBrowserHardlink))
	mux.HandleFunc("GET /api/admin/file-browser/index", s.auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, 200, s.Svc.BrowserIndexStatus())
	}))
	mux.HandleFunc("POST /api/admin/file-browser/index/refresh", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !s.Svc.RefreshBrowserIndex("") {
			fail(w, 400, "尚未设置监听目录，或索引未能启动，请检查文件索引设置")
			return
		}
		writeJSON(w, 202, map[string]bool{"queued": true})
	}))
	// Bookmark bar: a per-admin tree of saved source folders. Deleting a
	// bookmark or folder only removes the entry, never the source directory.
	mux.HandleFunc("GET /api/admin/file-browser/bookmarks", s.auth(s.handleListFileBookmarks))
	mux.HandleFunc("POST /api/admin/file-browser/bookmarks/folders", s.auth(s.handleCreateFileBookmarkFolder))
	mux.HandleFunc("PATCH /api/admin/file-browser/bookmarks/folders/{id}", s.auth(s.handleRenameFileBookmarkFolder))
	mux.HandleFunc("DELETE /api/admin/file-browser/bookmarks/folders/{id}", s.auth(s.handleDeleteFileBookmarkFolder))
	mux.HandleFunc("POST /api/admin/file-browser/bookmarks/items", s.auth(s.handleCreateFileBookmark))
	mux.HandleFunc("DELETE /api/admin/file-browser/bookmarks/items/{id}", s.auth(s.handleDeleteFileBookmark))
	mux.HandleFunc("PATCH /api/admin/file-browser/bookmarks/move/{id}", s.auth(s.handleMoveFileBookmark))
}

func (s *Server) handleBrowserExtract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSONBody(r, &req, 8<<10); err != nil {
		fail(w, 400, "解压参数无效")
		return
	}
	abs, err := s.Svc.AbsoluteBrowserPath(req.Path)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	job, err := s.Svc.StartExtract(abs)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 202, job)
}

func (s *Server) handleBrowserExtractStatus(w http.ResponseWriter, r *http.Request) {
	abs, err := s.Svc.AbsoluteBrowserPath(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	job := s.Svc.ExtractStatus(abs)
	if job == nil {
		fail(w, 404, "无进行中的解压")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, job)
}

func (s *Server) handleBrowserMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSONBody(r, &req, 8<<10); err != nil {
		fail(w, 400, "参数无效")
		return
	}
	parent, err := s.Svc.AbsoluteBrowserPath(req.Parent)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') || strings.Contains(name, "..") {
		fail(w, 400, "文件夹名称无效")
		return
	}
	if err := s.Svc.CreateBrowserDir(parent, name); err != nil {
		fail(w, 409, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleBrowserHardlink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths  []string `json:"paths"`
		Target string   `json:"target"`
	}
	if err := readJSONBody(r, &req, 64<<10); err != nil {
		fail(w, 400, "参数无效")
		return
	}
	if len(req.Paths) == 0 {
		fail(w, 400, "未选择文件或文件夹")
		return
	}
	target, err := s.Svc.AbsoluteBrowserPath(req.Target)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	paths := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		abs, err := s.Svc.AbsoluteBrowserPath(p)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		paths = append(paths, abs)
	}
	if err := s.Svc.HardlinkBrowserItems(paths, target); err != nil {
		fail(w, 409, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleBrowserEntry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	entry, err := s.Svc.BrowserEntryInfo(r.URL.Query().Get("path"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fail(w, 404, "文件或目录已不存在")
			return
		}
		fail(w, 400, "无法读取文件或目录："+err.Error())
		return
	}
	writeJSON(w, 200, struct {
		Entry  store.BrowserEntry `json:"entry"`
		Parent string             `json:"parent"`
	}{Entry: entry, Parent: store.EncodeSourcePath(filepath.Dir(entry.Path))})
}

func (s *Server) handleFileBrowser(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if r.URL.Query().Get("offset") == "" {
		offset, err = 0, nil
	}
	if err != nil {
		fail(w, 400, "分页参数无效")
		return
	}
	recursive := r.URL.Query().Get("recursive")
	if recursive != "" && recursive != "0" && recursive != "1" {
		fail(w, 400, "展开参数无效")
		return
	}
	kind := r.URL.Query().Get("filter")
	switch kind {
	case "", "all", "image", "video", "folder", "other":
	default:
		fail(w, 400, "筛选参数无效")
		return
	}
	listing, err := s.Svc.BrowseLocalFiles(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("q"), offset, r.URL.Query().Get("sort"), store.BrowserOptions{Recursive: recursive == "1", Snapshot: r.URL.Query().Get("snapshot"), Kind: kind})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			fail(w, 400, "目录读取时间过长，请进入更小的子文件夹后重试")
			return
		}
		fail(w, 400, "无法读取目录："+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, listing)
}

func browserContentURL(p, version, mode string) string {
	return "/api/admin/file-browser/content?" + url.Values{"path": {p}, "version": {version}, "compatible": {mode}}.Encode()
}

// browserCapsJSON is the client's canPlayType probe result. Absent fields keep
// the strict baseline, so a stale client only loses direct-play coverage.
type browserCapsJSON struct {
	HEVC8  bool `json:"hevc8"`
	HEVC10 bool `json:"hevc10"`
	AV1    bool `json:"av1"`
	VP9    bool `json:"vp9"`
	MP3    bool `json:"mp3"`
	Opus   bool `json:"opus"`
	AC3    bool `json:"ac3"`
	FLAC   bool `json:"flac"`
}

func (c browserCapsJSON) caps() mediax.BrowserCaps {
	return mediax.BrowserCaps{HEVC8: c.HEVC8, HEVC10: c.HEVC10, AV1: c.AV1, VP9: c.VP9, MP3: c.MP3, Opus: c.Opus, AC3: c.AC3, FLAC: c.FLAC}
}

// encodeBrowserCaps renders a capability set for the content URL. The content
// endpoint reconstructs the same set to pick the right ffmpeg mapping.
func encodeBrowserCaps(caps mediax.BrowserCaps) string {
	var names []string
	for _, flag := range []struct {
		name  string
		value bool
	}{{"hevc8", caps.HEVC8}, {"hevc10", caps.HEVC10}, {"av1", caps.AV1}, {"vp9", caps.VP9}, {"mp3", caps.MP3}, {"opus", caps.Opus}, {"ac3", caps.AC3}, {"flac", caps.FLAC}} {
		if flag.value {
			names = append(names, flag.name)
		}
	}
	return strings.Join(names, ",")
}

func parseBrowserCaps(value string) (mediax.BrowserCaps, error) {
	var caps mediax.BrowserCaps
	if value == "" {
		return caps, nil
	}
	for _, name := range strings.Split(value, ",") {
		switch name {
		case "hevc8":
			caps.HEVC8 = true
		case "hevc10":
			caps.HEVC10 = true
		case "av1":
			caps.AV1 = true
		case "vp9":
			caps.VP9 = true
		case "mp3":
			caps.MP3 = true
		case "opus":
			caps.Opus = true
		case "ac3":
			caps.AC3 = true
		case "flac":
			caps.FLAC = true
		default:
			return caps, fmt.Errorf("未知能力 %q", name)
		}
	}
	return caps, nil
}

func validCapsFingerprint(fp string) bool {
	if len(fp) != 8 {
		return false
	}
	for _, r := range fp {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) handleBrowserView(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path       string           `json:"path"`
		Version    string           `json:"version"`
		Compatible string           `json:"compatible"`
		Retry      bool             `json:"retry"`
		NativeHEVC bool             `json:"native_hevc"`
		Native     *browserCapsJSON `json:"native"`
	}
	if err := readJSONBody(r, &req, 16<<10); err != nil {
		fail(w, 400, "文件查看参数无效")
		return
	}
	caps := mediax.BrowserCaps{}
	if req.Native != nil {
		caps = req.Native.caps()
	}
	caps.HEVC8 = caps.HEVC8 || req.NativeHEVC
	var probeDuration float64
	var prepare bool
	if req.Compatible == "" || req.Compatible == "auto" {
		// "auto" is the explicit fallback after playback failed: decide with
		// the strict baseline so a codec the browser claimed but failed to
		// decode is never kept, then force a compatibility mode.
		decisionCaps := caps
		if req.Compatible == "auto" {
			decisionCaps = mediax.BrowserCaps{}
		}
		mode, duration, needsPrepare, err := s.Svc.BrowserPlaybackMode(r.Context(), req.Path, req.Version, decisionCaps)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		if req.Compatible == "auto" && mode == "" {
			mode = "remux"
		}
		req.Compatible, probeDuration, prepare = mode, duration, needsPrepare
	}
	if req.Compatible == "remux" || req.Compatible == "transcode" {
		if prepare {
			// Expensive conversions (HDR tone mapping) run to completion first:
			// the client shows real progress and then plays a complete file with
			// full seeking, instead of a stream that can never catch up.
			view, err := s.Svc.PrepareBrowserView(r.Context(), req.Path, req.Version, req.Compatible, req.Retry, caps)
			if err != nil {
				status := http.StatusConflict
				if errors.Is(err, store.ErrBrowserViewBusy) {
					status = http.StatusTooManyRequests
				}
				fail(w, status, err.Error())
				return
			}
			address := ""
			if view.Status == "ready" {
				address = browserContentURL(req.Path, req.Version, req.Compatible)
			}
			w.Header().Set("Cache-Control", "private, no-store")
			writeJSON(w, 200, struct {
				store.OriginalView
				URL        string  `json:"url"`
				Compatible string  `json:"compatible"`
				Duration   float64 `json:"duration"`
			}{view, address, req.Compatible, probeDuration})
			return
		}
		// Videos no longer wait here: the content endpoint streams the
		// conversion, so playback starts at the first ready fragment. The
		// probed duration gives the player a stable progress denominator
		// while the fragmented stream is still growing.
		view := store.OriginalView{Status: "ready", Percent: 100, Source: "filesystem"}
		address := browserContentURL(req.Path, req.Version, req.Compatible) + "&stream=1"
		if encoded := encodeBrowserCaps(caps); encoded != "" {
			address += "&caps=" + url.QueryEscape(encoded)
			if fp := mediax.CapsFingerprint(caps); fp != "" {
				address += "&capsfp=" + fp
			}
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, 200, struct {
			store.OriginalView
			URL        string  `json:"url"`
			Compatible string  `json:"compatible"`
			Duration   float64 `json:"duration"`
		}{view, address, req.Compatible, probeDuration})
		return
	}
	view, err := s.Svc.PrepareBrowserView(r.Context(), req.Path, req.Version, req.Compatible, req.Retry, caps)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, store.ErrBrowserViewBusy) {
			status = http.StatusTooManyRequests
		}
		fail(w, status, err.Error())
		return
	}
	address := ""
	if view.Status == "ready" {
		address = browserContentURL(req.Path, req.Version, req.Compatible)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, struct {
		store.OriginalView
		URL        string `json:"url"`
		Compatible string `json:"compatible"`
	}{view, address, req.Compatible})
}

func (s *Server) handleBrowserContent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("compatible")
	caps, err := parseBrowserCaps(q.Get("caps"))
	if err != nil {
		fail(w, 400, "播放能力参数无效")
		return
	}
	if fp := q.Get("capsfp"); fp != "" && !validCapsFingerprint(fp) {
		fail(w, 400, "播放能力参数无效")
		return
	}
	if q.Get("stream") == "1" && (mode == "remux" || mode == "transcode") {
		s.streamBrowserContent(w, r, q.Get("path"), q.Get("version"), mode, caps)
		return
	}
	var file *os.File
	var entry store.BrowserEntry
	if q.Get("download") == "1" && mode == "" {
		file, entry, err = s.Svc.OpenBrowserDownload(q.Get("path"), q.Get("version"))
	} else {
		file, entry, err = s.Svc.OpenBrowserContent(q.Get("path"), q.Get("version"), mode)
	}
	if err != nil {
		if s.Log != nil {
			// diagnose problematic playbacks: which file, mode and stream flag
			s.Log.Warn("content 未就绪", "path", q.Get("path"), "compatible", mode, "stream", q.Get("stream"), "err", err.Error())
		}
		fail(w, 409, "文件已变化、不可读取或兼容版本未就绪，请刷新或重新打开文件")
		return
	}
	defer file.Close()
	contentType, name := entry.MIME, entry.Name
	if mode != "" {
		if entry.Kind == "image" {
			contentType, name = "image/png", name+".view.png"
		} else {
			contentType, name = "video/mp4", name+".view.mp4"
		}
	}
	disposition := "inline"
	if q.Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie, Authorization")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	finish := s.Svc.BeginForegroundTransfer()
	defer finish()
	http.ServeContent(w, r, name, time.Time{}, file)
}

// streamBrowserContent serves a compatible video that may still be converting:
// a complete cache file is range-served as usual, otherwise the conversion is
// streamed to the client as fragmented MP4 while it is produced.
func (s *Server) streamBrowserContent(w http.ResponseWriter, r *http.Request, p, version, mode string, caps mediax.BrowserCaps) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie, Authorization")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "video/mp4")
	finish := s.Svc.BeginForegroundTransfer()
	defer finish()
	file, streamed, err := s.Svc.StreamBrowserCompatible(r.Context(), p, version, mode, caps, w)
	if err != nil && !streamed {
		// Nothing was written yet, so the client can still receive a real
		// error and fall back to the next playback mode.
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if file != nil {
		defer file.Close()
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": "view.mp4"}))
		http.ServeContent(w, r, "view.mp4", time.Time{}, file)
	}
	// A leader that already streamed (successfully or not) has written the
	// response body; aborting it tells the video element to try the next mode.
}
