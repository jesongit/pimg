package server

import (
	"net/http"
)

func (s *Server) handleOriginalView(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.MutationFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.MediaVersion == 2 {
		s.handleMediaOriginalView(w, r, f)
		return
	}
	var request struct {
		Retry bool `json:"retry"`
	}
	if err := readJSONBody(r, &request, 1024); err != nil {
		fail(w, 400, "原件加载参数无效")
		return
	}
	view, err := s.Svc.PrepareOriginal(r.Context(), f, request.Retry)
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	url := ""
	if view.Status == "ready" {
		url = requestContentURL(r, f.ID, "original") + "&cached=1"
	}
	writeJSON(w, 200, map[string]any{"status": view.Status, "loaded": view.Loaded, "total": view.Total, "percent": view.Percent,
		"source": view.Source, "expires_at": view.ExpiresAt, "error": view.Error, "url": url})
}
