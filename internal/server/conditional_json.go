package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

func conditionalPage(path string) bool {
	for _, base := range []string{"/api/admin/", "/api/account/"} {
		if !strings.HasPrefix(path, base) {
			continue
		}
		switch strings.TrimPrefix(path, base) {
		case "files", "files/timeline", "files/navigation", "files/previews", "albums", "accounts", "storage", "settings", "status", "logs", "schedules", "tasks/summary", "telegram":
			return true
		}
	}
	return false
}

// Authentication runs for every request. Pure database views may validate
// inside auth before executing their query; other views hash the current JSON.
// Fast validators may retain bounded bodies inside the authenticated actor scope.
func conditionalJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !conditionalPage(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		out := &pageResponse{target: w, header: w.Header().Clone(), code: 200}
		next.ServeHTTP(out, r)
		if out.streaming {
			return
		}
		for key, values := range out.header {
			w.Header()[key] = values
		}
		if out.code == http.StatusOK && strings.HasPrefix(out.header.Get("Content-Type"), "application/json") {
			tag := fmt.Sprintf(`"%x"`, sha256.Sum256(out.body.Bytes()))
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Add("Vary", "Cookie, Authorization, X-Pimg-Owner")
			for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
				if candidate = strings.TrimSpace(candidate); candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
					w.Header().Del("Content-Length")
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
		w.WriteHeader(out.code)
		_, _ = w.Write(out.body.Bytes())
	})
}

type pageResponse struct {
	target           http.ResponseWriter
	header           http.Header
	body             bytes.Buffer
	code             int
	wrote, streaming bool
}

func (w *pageResponse) Header() http.Header {
	if w.streaming {
		return w.target.Header()
	}
	return w.header
}
func (w *pageResponse) WriteHeader(code int) {
	if !w.wrote {
		w.code = code
		w.wrote = true
	}
}
func (w *pageResponse) Write(data []byte) (int, error) {
	if w.streaming {
		return w.target.Write(data)
	}
	if w.body.Len()+len(data) > 4<<20 {
		for k, v := range w.header {
			w.target.Header()[k] = v
		}
		w.target.WriteHeader(w.code)
		if _, err := w.target.Write(w.body.Bytes()); err != nil {
			return 0, err
		}
		w.body.Reset()
		w.streaming = true
		return w.target.Write(data)
	}
	w.wrote = true
	return w.body.Write(data)
}
