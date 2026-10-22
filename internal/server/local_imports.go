package server

import (
	"net/http"
	"path/filepath"
	"strconv"
)

func (s *Server) registerLocalImports(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/local-imports/browse", s.auth(s.handleImportBrowse))
	mux.HandleFunc("GET /api/admin/local-imports", s.auth(s.handleImportBatches))
	mux.HandleFunc("POST /api/admin/local-imports", s.auth(s.handleImportCreate))
	mux.HandleFunc("GET /api/admin/local-imports/{batch}", s.auth(s.handleImportBatch))
	mux.HandleFunc("POST /api/admin/local-imports/{batch}/{action}", s.auth(s.handleImportAction))
}

func (s *Server) handleImportBrowse(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	p := r.URL.Query().Get("path")
	if p == "" || p == "." {
		p = s.Svc.InitialImportPath()
	}
	node, err := s.Svc.ImportPathInfo(p)
	if err != nil {
		fail(w, 400, "无法打开此路径，请检查路径、挂载和读取权限")
		return
	}
	items, next, err := s.Svc.BrowseImports(node.Path, offset)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"path": node.Path, "parent": filepath.Dir(node.Path), "directory": node.Directory, "items": items, "next_offset": next})
}

func (s *Server) handleImportBatches(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT id FROM local_import_batches ORDER BY id DESC LIMIT 30`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			fail(w, 500, err.Error())
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	items := []any{}
	for _, id := range ids {
		b, e := s.Svc.LocalImportBatch(id)
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		items = append(items, b)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) handleImportCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths     []string `json:"paths"`
		AlbumID   int64    `json:"album_id"`
		Encrypted *bool    `json:"encrypted"`
		Recursive bool     `json:"recursive"`
		Schedule  string   `json:"schedule"`
	}
	if err := readJSONBody(r, &req, 64<<10); err != nil {
		fail(w, 400, err.Error())
		return
	}
	encrypted := req.Encrypted != nil && *req.Encrypted
	if req.Schedule == "" {
		req.Schedule = "idle"
	}
	id, err := s.Svc.CreateLocalImport(req.Paths, req.AlbumID, encrypted, req.Recursive, req.Schedule)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 202, map[string]any{"id": id})
}

func (s *Server) handleImportBatch(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("batch"), 10, 64)
	b, err := s.Svc.LocalImportBatch(id)
	if err != nil {
		fail(w, 404, "导入批次不存在")
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := s.Svc.LocalImportItems(id, offset, r.URL.Query().Get("state"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"batch": b, "items": items})
}

func (s *Server) handleImportAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("batch"), 10, 64)
	if err := s.Svc.ChangeLocalImport(id, r.PathValue("action")); err != nil {
		fail(w, 409, err.Error())
		return
	}
	if r.PathValue("action") == "cancel" {
		if err := s.interruptImport(id); err != nil {
			fail(w, 500, "停止导入失败")
			return
		}
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
