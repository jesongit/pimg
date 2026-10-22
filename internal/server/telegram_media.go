package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

// handleTelegramArchive serves exact stored bytes after token-only auth.
// Encryption keys and Telegram credentials never enter the response or URL.
func (s *Server) handleTelegramArchive(w http.ResponseWriter, r *http.Request) {
	f, release, err := s.Svc.LockTelegramArchive(r.Context(), r.PathValue("id"), r.PathValue("kind"))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, store.ErrArchiveLinkUnavailable) {
			fail(w, 404, "归档链接不可用")
		} else {
			fail(w, 500, "归档链接暂时无法读取，请稍后重试。")
		}
		return
	}
	defer release()
	s.serveRawArchive(w, r, f, r.PathValue("kind"))
}

func (s *Server) serveRawArchive(w http.ResponseWriter, r *http.Request, f *dbx.File, kind string) {
	if s.Svc.UserTelegramEnabled() {
		index := 0
		if value := r.URL.Query().Get("part"); value != "" {
			var err error
			index, err = strconv.Atoi(value)
			if err != nil || index < 0 {
				fail(w, 400, "载体分片编号无效")
				return
			}
		}
		size, carrier, err := s.Svc.TelegramStoredPartSize(f, kind, index)
		if err != nil {
			fail(w, 404, "TG 归档分片暂时不可用")
			return
		}
		name, mimeType := f.OrigName, f.Mime
		if kind != "original" {
			name, mimeType = f.ID+"."+kind+".jpg", "image/jpeg"
		}
		if carrier {
			name, mimeType = fmt.Sprintf("%s.%s.%d.pimg.png", f.ID, kind, index), "image/png"
		}
		if named := store.TelegramFilename(f, kind, index); named != "" {
			name = named
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		reader := &archiveReader{ctx: r.Context(), size: size, open: func() (io.ReadCloser, error) {
			rc, _, _, err := s.Svc.OpenTelegramStoredPart(r.Context(), f, kind, index)
			return rc, err
		}}
		defer reader.Close()
		if err := reader.probe(); err != nil {
			fail(w, 500, "TG 归档暂时无法读取，请稍后重试。")
			return
		}
		http.ServeContent(w, r, name, time.Time{}, reader)
		return
	}
	size, err := s.Svc.RawArchiveSize(f, kind)
	if err != nil {
		fail(w, 500, "TG 归档暂时无法读取，请稍后重试。")
		return
	}
	filename, contentType, disposition := store.MediaFilename(f), f.Mime, "inline"
	if kind != "original" {
		filename, contentType = f.ID+"."+kind+".jpg", "image/jpeg"
	}
	if f.Storage != "direct" {
		filename, contentType, disposition = filename+".enc", "application/octet-stream", "attachment"
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.Header().Set("Accept-Ranges", "bytes")
		return
	}
	reader := &archiveReader{ctx: r.Context(), size: size, open: func() (io.ReadCloser, error) { return s.Svc.RawArchiveStream(r.Context(), f, kind) }}
	defer reader.Close()
	if err := reader.probe(); err != nil {
		if r.Context().Err() == nil {
			fail(w, 500, "TG 归档暂时无法读取，请稍后重试。")
		}
		return
	}
	http.ServeContent(w, r, filename, time.Time{}, reader)
	if reader.err != nil && !errors.Is(reader.err, context.Canceled) {
		s.Log.Error("TG 原始归档读取中断", "file_id", f.ID)
	}
}
