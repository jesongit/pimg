package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pimg/internal/dbx"
)

func (s *Server) registerOperations(mux *http.ServeMux) {
	for _, base := range []string{"/api/admin", "/api/account"} {
		guard := s.accountAuth
		if base == "/api/admin" {
			guard = s.auth
		}
		mux.HandleFunc("POST "+base+"/operations", guard(s.handleOperationCreate))
		mux.HandleFunc("GET "+base+"/operations/{operation}", guard(s.handleOperationGet))
		mux.HandleFunc("GET "+base+"/operations/{operation}/items", guard(s.handleOperationItems))
		mux.HandleFunc("POST "+base+"/operations/{operation}/{action}", guard(s.handleOperationAction))
	}
}

func operationScope(kind string, album int64, encrypted bool) string {
	switch kind {
	case "upload":
		return fmt.Sprintf("album:%d;encrypted:%t", album, encrypted)
	case "move_album":
		return fmt.Sprintf("album:%d", album)
	case "convert":
		return dbx.ConversionScope(encrypted)
	}
	return ""
}

func (s *Server) handleOperationCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key       string `json:"key"`
		Kind      string `json:"kind"`
		Total     int    `json:"total_files"`
		AlbumID   int64  `json:"album_id"`
		Encrypted *bool  `json:"encrypted"`
		MediaAt   *int64 `json:"media_at"`
	}
	if err := readJSONBody(r, &req, 4096); err != nil || req.AlbumID < 0 {
		fail(w, 400, "操作参数无效")
		return
	}
	owner := dbx.ContextOwner(r.Context())
	if (req.Kind == "show_home" || req.Kind == "hide_home") && !strings.HasPrefix(r.URL.Path, "/api/admin/") {
		fail(w, 403, "无权更改站点首页")
		return
	}
	if req.Kind == "upload" {
		var err error
		owner, err = s.uploadOwner(r, req.AlbumID)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	if req.Kind == "move_album" && req.AlbumID == 0 && strings.HasPrefix(r.URL.Path, "/api/account/") {
		var err error
		req.AlbumID, err = s.DB.EnsureOwnerDefaultAlbum(owner)
		if err != nil {
			fail(w, 500, "默认相册不可用")
			return
		}
	}
	if req.AlbumID > 0 && strings.HasPrefix(r.URL.Path, "/api/account/") {
		if _, err := s.DB.OwnedAlbum(owner, req.AlbumID); err != nil {
			fail(w, 404, "相册不存在")
			return
		}
	}
	if req.Kind == "convert" && req.Encrypted == nil {
		fail(w, 400, "请指定目标加密方式 encrypted")
		return
	}
	scope := operationScope(req.Kind, req.AlbumID, req.Encrypted != nil && *req.Encrypted)
	if req.Kind == "set_media_time" {
		if req.MediaAt == nil || !dbx.ValidMediaTime(*req.MediaAt) {
			fail(w, 400, "请指定有效的文件日期（精确到秒）")
			return
		}
		scope = dbx.MediaDateScope(*req.MediaAt)
	}
	op, err := s.DB.BeginTaskOperation(owner, req.Kind, req.Key, scope, req.Total)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"operation_id": op.ID, "task_id": op.TaskID})
}

func (s *Server) ownedOperation(r *http.Request, id int64) (*dbx.TaskOperation, error) {
	owner := dbx.ContextOwner(r.Context())
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		if err := s.DB.QueryRow(`SELECT owner_id FROM task_operations WHERE id=? AND request_key!=''`, id).Scan(&owner); err != nil {
			return nil, dbx.ErrNotFound
		}
	}
	return s.DB.TaskOperationByID(id, owner)
}

func (s *Server) checkedOperation(r *http.Request, id int64, kind, scope string) (*dbx.TaskOperation, error) {
	// Upload handlers resolve an explicitly selected album's owner first.
	// A caller cannot append content using an operation from another target.
	op, err := s.DB.TaskOperationByID(id, dbx.ContextOwner(r.Context()))
	if err != nil {
		return nil, err
	}
	if op.Kind != kind || op.Scope != scope {
		return nil, dbx.ErrTaskOperationConflict
	}
	if err = s.DB.TouchTaskOperation(op.ID, op.OwnerID); err != nil {
		return nil, err
	}
	return op, nil
}

func (s *Server) operationFromPath(w http.ResponseWriter, r *http.Request) (*dbx.TaskOperation, bool) {
	id, err := strconv.ParseInt(r.PathValue("operation"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "操作编号无效")
		return nil, false
	}
	if err = s.DB.ExpireTaskOperations(time.Now().Unix()); err != nil {
		fail(w, 500, "读取操作失败")
		return nil, false
	}
	op, err := s.ownedOperation(r, id)
	if err != nil {
		fail(w, 404, "操作不存在")
		return nil, false
	}
	return op, true
}

func (s *Server) handleOperationGet(w http.ResponseWriter, r *http.Request) {
	op, ok := s.operationFromPath(w, r)
	if !ok {
		return
	}
	group, err := s.DB.GetTaskOperation(op.ID, op.OwnerID)
	if err != nil {
		fail(w, 404, "操作不存在")
		return
	}
	context, e := s.DB.ActivityContext(group)
	if e != nil {
		fail(w, 500, "读取操作内容失败")
		return
	}
	row := activityTaskJSON(*group)
	row["context"] = context
	writeJSON(w, 200, map[string]any{"operation": row})
}

func (s *Server) handleOperationItems(w http.ResponseWriter, r *http.Request) {
	op, ok := s.operationFromPath(w, r)
	if !ok {
		return
	}
	page := atoiOr(r.URL.Query().Get("page"), 1)
	state := r.URL.Query().Get("state")
	if page < 1 || page > 1000000 || state != "" && state != "all" && state != "failed" {
		fail(w, 400, "明细参数无效")
		return
	}
	if state == "all" {
		state = ""
	}
	items, total, err := s.DB.TaskOperationItems(op.ID, op.OwnerID, state, page, 50)
	if err != nil {
		fail(w, 500, "读取操作明细失败")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		ids := []string{}
		for _, item := range items {
			if !item.FileExists && item.FileID != "" {
				ids = append(ids, item.FileID)
			}
		}
		successors, e := s.DB.FileSuccessors(ids)
		if e != nil {
			fail(w, 500, "读取文件新位置失败，请重试")
			return
		}
		for i := range items {
			items[i].FileSuccessor = successors[items[i].FileID]
		}
	}
	next := 0
	if page*50 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "next_page": next})
}

func (s *Server) handleOperationAction(w http.ResponseWriter, r *http.Request) {
	op, ok := s.operationFromPath(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action == "seal" {
		err := s.DB.SealTaskOperation(op.ID, op.OwnerID)
		if err != nil && !errors.Is(err, dbx.ErrTaskOperationClosed) {
			fail(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if action != "cancel" && action != "retry" {
		fail(w, 404, "操作不存在")
		return
	}
	r.SetPathValue("id", strconv.FormatInt(op.TaskID, 10))
	q := r.URL.Query()
	q.Set("group", "1")
	r.URL.RawQuery = q.Encode()
	s.handleGroupedTaskAction(w, r, action)
}
