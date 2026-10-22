package server

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func mediaPartIndex(r *http.Request) (int, error) {
	value := r.URL.Query().Get("part")
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

func (s *Server) handleMediaOriginalView(w http.ResponseWriter, r *http.Request, f *dbx.File) {
	var request struct {
		Retry      bool   `json:"retry"`
		Part       int    `json:"part"`
		Compatible string `json:"compatible"`
	}
	if err := readJSONBody(r, &request, 2048); err != nil {
		fail(w, 400, "原件查看参数无效")
		return
	}
	view, err := s.Svc.PrepareMediaView(r.Context(), f, request.Part, request.Compatible, request.Retry)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	url := ""
	if view.Status == "ready" {
		url = requestContentURL(r, f.ID, "original") + "&part=" + strconv.Itoa(request.Part)
		if request.Compatible != "" {
			url += "&compatible=" + request.Compatible
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]any{"status": view.Status, "loaded": view.Loaded, "total": view.Total, "percent": view.Percent, "source": view.Source, "error": view.Error, "url": url, "part": view.Part, "compatible": view.Compatible})
}

func (s *Server) serveMediaOriginal(w http.ResponseWriter, r *http.Request, f *dbx.File, download bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie, Authorization")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if download && r.URL.Query().Get("part") == "" {
		input, err := s.Svc.SourceStream(r.Context(), f)
		if err != nil {
			fail(w, 409, "此视频保存为无损片段，请选择片段下载")
			return
		}
		defer input.Close()
		w.Header().Set("Content-Type", f.Mime)
		w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": store.MediaFilename(f)}))
		finish := s.Svc.BeginForegroundTransfer()
		defer finish()
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, input)
		}
		return
	}
	index, err := mediaPartIndex(r)
	if err != nil {
		fail(w, 400, "片段编号无效")
		return
	}
	compatible := r.URL.Query().Get("compatible")
	if compatible != "" && compatible != "image" && compatible != "remux" && compatible != "transcode" {
		fail(w, 400, "查看方式无效")
		return
	}
	file, part, err := s.Svc.OpenMediaView(r.Context(), f, index, compatible, download)
	if err != nil {
		fail(w, 409, "本地内容尚未就绪，请重新加载原件")
		return
	}
	defer file.Close()
	contentType := part.MIME
	name := store.MediaPartName(f, part)
	if compatible != "" {
		if f.Kind == "image" {
			contentType = "image/png"
			name += ".view.png"
		} else {
			contentType = "video/mp4"
			name += ".view.mp4"
		}
	}
	w.Header().Set("Content-Type", contentType)
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	finish := s.Svc.BeginForegroundTransfer()
	defer finish()
	http.ServeContent(w, r, name, time.Time{}, file)
}

func (s *Server) handleRemoveLocalMedia(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.MutationFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if err = readJSONBody(r, &request, 1024); err != nil || !request.Confirm {
		fail(w, 400, "请确认移除本地副本；仅剩视频片段时将无法恢复字节一致的源文件")
		return
	}
	if err = s.Svc.RemoveLocalMedia(r.Context(), f, r.PathValue("instance")); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
