package server

import (
	"errors"
	"net/http"
	"strings"

	"pimg/internal/dbx"
)

// A read-only preview of a selected batch. The actual submission checks again;
// a preview grants no authority and reserves no files or tasks.
func (s *Server) handleConversionCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs       []string `json:"ids"`
		Encrypted *bool    `json:"encrypted"`
	}
	if err := readJSONBody(r, &req, 64<<10); err != nil || len(req.IDs) < 1 || len(req.IDs) > 200 || req.Encrypted == nil {
		fail(w, 400, "请指定 1–200 个文件及目标加密方式 encrypted")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/account/") {
		for _, id := range req.IDs {
			if _, err := s.DB.OwnedFile(dbx.ContextOwner(r.Context()), id); err != nil {
				fail(w, 404, "文件不存在")
				return
			}
		}
	}
	items := []map[string]string{}
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		item := map[string]string{"id": id, "state": "ready"}
		if f, err := s.DB.GetVisibleFile(id); err == nil {
			item["name"] = f.Name
		}
		if err := s.Svc.CheckConversion(id, *req.Encrypted); err != nil {
			item["state"], item["reason"] = "blocked", err.Error()
			if errors.Is(err, dbx.ErrConversionNotNeeded) {
				item["state"] = "skipped"
			}
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
