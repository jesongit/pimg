package server

import (
	"net/http"
	"strconv"
)

// Intake receipts remain available for web uploads and imports after retiring
// the standalone uploader API.
func (s *Server) handleIngestEvents(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	events, err := s.DB.ListIngestEvents(r.URL.Query().Get("file_id"), r.URL.Query().Get("tool_file_id"), r.URL.Query().Get("upload_id"), before)
	if err != nil {
		fail(w, 500, "读取接管日志失败")
		return
	}
	next := int64(0)
	if len(events) == 100 {
		next = events[len(events)-1].ID
	}
	writeJSON(w, 200, map[string]any{"items": events, "next_before": next})
}
