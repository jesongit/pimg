package server

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pimg/internal/mediax"
	"pimg/internal/store"
)

func (s *Server) registerFileBrowser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/file-browser", s.auth(s.handleFileBrowser))
	mux.HandleFunc("POST /api/admin/file-browser/view", s.auth(s.handleBrowserView))
	mux.HandleFunc("GET /api/admin/file-browser/content", s.auth(s.handleBrowserContent))
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
	listing, err := s.Svc.BrowseLocalFiles(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("q"), offset, r.URL.Query().Get("sort"), store.BrowserOptions{Recursive: recursive == "1", Snapshot: r.URL.Query().Get("snapshot")})
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
	if req.Compatible == "" || req.Compatible == "auto" {
		// "auto" is the explicit fallback after playback failed: decide with
		// the strict baseline so a codec the browser claimed but failed to
		// decode is never kept, then force a compatibility mode.
		decisionCaps := caps
		if req.Compatible == "auto" {
			decisionCaps = mediax.BrowserCaps{}
		}
		mode, duration, err := s.Svc.BrowserPlaybackMode(r.Context(), req.Path, req.Version, decisionCaps)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		if req.Compatible == "auto" && mode == "" {
			mode = "remux"
		}
		req.Compatible, probeDuration = mode, duration
	}
	if req.Compatible == "remux" || req.Compatible == "transcode" {
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
	file, entry, err := s.Svc.OpenBrowserContent(q.Get("path"), q.Get("version"), mode)
	if err != nil {
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
