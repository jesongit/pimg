package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"strings"
	"time"
)

func canPreview(f *dbx.File) bool {
	return f.HasThumb || f.Kind == "image" || f.Kind == "video"
}

func privateContentURL(id, kind string) string {
	return "/api/admin/files/" + id + "/content?kind=" + kind
}

func requestContentURL(r *http.Request, id, kind string) string {
	if strings.HasPrefix(r.URL.Path, "/api/account/") {
		return "/api/account/files/" + id + "/content?kind=" + kind
	}
	return privateContentURL(id, kind)
}

// handleContent serves private media only after session/token authentication.
// Encrypted originals are streamed without creating plaintext hot copies.
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.MutationFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.ArchiveState != "done" || f.PurgeState.String != "" {
		fail(w, 409, "原件尚未就绪或正在删除")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "thumb" && kind != "original" {
		fail(w, 400, "无效的预览类型")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Vary", "Cookie, Authorization")
	if kind == "thumb" {
		if !canPreview(f) {
			fail(w, 404, "没有缩略图")
			return
		}
		data, err := s.Svc.PreviewThumbnail(r.Context(), f)
		if err != nil {
			if r.Context().Err() == nil {
				fail(w, 500, "预览读取失败，请重试。")
			}
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeContent(w, r, "preview.jpg", time.Time{}, bytes.NewReader(data))
		return
	}
	w.Header().Set("Content-Type", f.Mime)
	if f.MediaVersion == 2 {
		s.serveMediaOriginal(w, r, f, false)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": store.MediaFilename(f)}))
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(f.Size))
		w.Header().Set("Accept-Ranges", "bytes")
		return
	}
	var source io.ReadSeekCloser
	if r.URL.Query().Get("cached") == "1" {
		source, err = s.Svc.CachedOriginal(f)
	} else {
		source, err = s.Svc.ViewOriginal(r.Context(), f)
	}
	if err != nil {
		if r.Context().Err() == nil {
			if r.URL.Query().Get("cached") == "1" {
				fail(w, 409, "原件缓存不可用，请重新加载原件。")
			} else {
				fail(w, 500, "原件读取失败，请重试。")
			}
		}
		return
	}
	defer source.Close()
	// Cached originals can still occupy the outbound link for a long time. Keep
	// that foreground activity visible until ServeContent finishes or disconnects.
	finish := s.Svc.BeginForegroundTransfer()
	defer finish()
	http.ServeContent(w, r, store.MediaFilename(f), time.Time{}, source)
}

// Tink's Go reader is sequential. Seek keeps HTTP ranges correct without
// inventing a new encryption format; a distant range authenticates/skips its
// prefix. Memory stays bounded. Large video seeks still need prefix IO.
type archiveReader struct {
	ctx                  context.Context
	size, pos, sourcePos int64
	open                 func() (io.ReadCloser, error)
	source               io.ReadCloser
	prefix               []byte
	err                  error
}

func (a *archiveReader) probe() error {
	source, err := a.open()
	if err != nil {
		return err
	}
	a.source = source
	buf := make([]byte, min(a.size, 64<<10))
	n, err := io.ReadFull(source, buf)
	if err != nil {
		return err
	}
	a.prefix = buf[:n]
	a.sourcePos = int64(n)
	return nil
}
func (a *archiveReader) Read(p []byte) (int, error) {
	if err := a.ctx.Err(); err != nil {
		a.err = err
		return 0, err
	}
	if a.pos >= a.size {
		return 0, io.EOF
	}
	if a.pos < int64(len(a.prefix)) {
		n := copy(p, a.prefix[a.pos:])
		a.pos += int64(n)
		return n, nil
	}
	if a.source == nil || a.pos < a.sourcePos {
		if a.source != nil {
			a.source.Close()
		}
		source, err := a.open()
		if err != nil {
			a.err = err
			return 0, err
		}
		a.source = source
		a.sourcePos = 0
	}
	if a.pos > a.sourcePos {
		n, err := io.CopyN(io.Discard, contextReader{a.ctx, a.source}, a.pos-a.sourcePos)
		a.sourcePos += n
		if err != nil {
			a.err = err
			return 0, err
		}
	}
	if int64(len(p)) > a.size-a.pos {
		p = p[:a.size-a.pos]
	}
	n, err := a.source.Read(p)
	a.sourcePos += int64(n)
	a.pos += int64(n)
	if err == io.EOF && a.pos < a.size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		a.err = err
	}
	return n, err
}
func (a *archiveReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += a.pos
	case io.SeekEnd:
		offset += a.size
	default:
		return 0, errors.New("invalid seek")
	}
	if offset < 0 {
		return 0, errors.New("negative seek")
	}
	a.pos = offset
	return offset, nil
}
func (a *archiveReader) Close() error {
	if a.source != nil {
		return a.source.Close()
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Publication is checked for every request; knowing an ID never exposes a
// private thumbnail, even if its plaintext preview exists on the local disk.
func (s *Server) handlePublicPreview(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.MutationFile(r.PathValue("id"))
	if err != nil || f.Hidden || (f.MediaVersion != 2 && f.Storage != "direct") || !f.Published || f.PurgeState.String != "" || f.ArchiveState != "done" || !canPreview(f) {
		http.NotFound(w, r)
		return
	}
	data, err := s.Svc.PreviewThumbnail(r.Context(), f)
	if err != nil {
		fail(w, 503, "封面暂时无法读取，请重试")
		return
	}
	current, err := s.DB.MutationFile(f.ID)
	if err != nil || !current.Published || (current.MediaVersion != 2 && current.Storage != "direct") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "preview.jpg", time.Time{}, bytes.NewReader(data))
}

// Album display grants thumbnail access only. Never redirect to publications,
// temporary originals or archive streams; recheck revocation after preview IO.
func (s *Server) handleExhibitThumbnail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	if !s.DB.FileExhibited(id) {
		http.NotFound(w, r)
		return
	}
	f, err := s.DB.MutationFile(id)
	if err != nil || !f.HasThumb {
		http.NotFound(w, r)
		return
	}
	data, err := s.Svc.PreviewThumbnail(r.Context(), f)
	if err != nil {
		fail(w, 503, "缩略图暂时无法读取，请重试")
		return
	}
	if !s.DB.FileExhibited(id) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "thumbnail.jpg", time.Time{}, bytes.NewReader(data))
}
