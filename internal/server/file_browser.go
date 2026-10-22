package server

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"pimg/internal/store"
)

func (s *Server) registerFileBrowser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/file-browser", s.auth(s.handleFileBrowser))
	mux.HandleFunc("POST /api/admin/file-browser/view", s.auth(s.handleBrowserView))
	mux.HandleFunc("GET /api/admin/file-browser/content", s.auth(s.handleBrowserContent))
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
	listing, err := s.Svc.BrowseLocalFiles(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("q"), offset, r.URL.Query().Get("sort"))
	if err != nil {
		fail(w, 400, "无法打开此路径，请检查目录、挂载和读取权限")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, listing)
}

func browserContentURL(p, version, mode string) string {
	return "/api/admin/file-browser/content?" + url.Values{"path": {p}, "version": {version}, "compatible": {mode}}.Encode()
}

func (s *Server) handleBrowserView(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path       string `json:"path"`
		Version    string `json:"version"`
		Compatible string `json:"compatible"`
		Retry      bool   `json:"retry"`
	}
	if err := readJSONBody(r, &req, 16<<10); err != nil {
		fail(w, 400, "文件查看参数无效")
		return
	}
	if req.Compatible == "" {
		mode, err := s.Svc.BrowserPlaybackMode(r.Context(), req.Path, req.Version)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		req.Compatible = mode
	}
	view, err := s.Svc.PrepareBrowserView(r.Context(), req.Path, req.Version, req.Compatible, req.Retry)
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
