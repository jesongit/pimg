package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"pimg/internal/dbx"
)

func bookmarkOwner(r *http.Request) string {
	return dbx.ContextOwner(r.Context())
}

func bookmarkStatus(err error) int {
	switch {
	case errors.Is(err, dbx.ErrBookmarkName), errors.Is(err, dbx.ErrBookmarkPathEmpty), errors.Is(err, dbx.ErrBookmarkCycle):
		return http.StatusBadRequest
	case errors.Is(err, dbx.ErrBookmarkFolder), errors.Is(err, dbx.ErrBookmarkNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleListFileBookmarks(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.ListFileBookmarks(bookmarkOwner(r))
	if err != nil {
		fail(w, 500, "无法读取收藏栏")
		return
	}
	// Resolve whether each saved path is a directory or a file so the bar can
	// open a file directly. An unreadable stale path is treated as a directory
	// so clicking it surfaces the normal error page with its remove action.
	var enrich func(nodes []*dbx.FileBookmarkNode)
	enrich = func(nodes []*dbx.FileBookmarkNode) {
		for _, node := range nodes {
			if node.Type == "folder" {
				enrich(node.Children)
				continue
			}
			node.Directory = true
			if info, err := s.Svc.ImportPathInfo(node.Path); err == nil {
				node.Directory = info.Directory
			}
		}
	}
	enrich(items)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]any{"items": items})
}

type bookmarkFolderReq struct {
	Parent int64  `json:"parent"`
	Name   string `json:"name"`
}

func (s *Server) handleCreateFileBookmarkFolder(w http.ResponseWriter, r *http.Request) {
	var req bookmarkFolderReq
	if err := readJSONBody(r, &req, 8<<10); err != nil || req.Parent < 0 {
		fail(w, 400, "收藏文件夹参数无效")
		return
	}
	id, err := s.DB.CreateFileBookmarkFolder(bookmarkOwner(r), req.Parent, req.Name)
	if err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) handleRenameFileBookmarkFolder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "收藏文件夹参数无效")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSONBody(r, &req, 8<<10); err != nil {
		fail(w, 400, "收藏文件夹参数无效")
		return
	}
	if err := s.DB.RenameFileBookmarkFolder(bookmarkOwner(r), id, req.Name); err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteFileBookmarkFolder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "收藏文件夹参数无效")
		return
	}
	if err := s.DB.DeleteFileBookmarkFolder(bookmarkOwner(r), id); err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type bookmarkItemReq struct {
	Folder int64  `json:"folder"`
	Path   string `json:"path"`
	Name   string `json:"name"`
}

func (s *Server) handleCreateFileBookmark(w http.ResponseWriter, r *http.Request) {
	var req bookmarkItemReq
	if err := readJSONBody(r, &req, 16<<10); err != nil || req.Folder < 0 {
		fail(w, 400, "收藏参数无效")
		return
	}
	info, err := s.Svc.ImportPathInfo(req.Path)
	if err != nil {
		fail(w, 409, "收藏的路径不存在或不可读取："+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = info.Name
	}
	id, err := s.DB.CreateFileBookmark(bookmarkOwner(r), req.Folder, req.Path, name)
	if err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) handleDeleteFileBookmark(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "收藏参数无效")
		return
	}
	if err := s.DB.DeleteFileBookmark(bookmarkOwner(r), id); err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMoveFileBookmark(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "收藏参数无效")
		return
	}
	var req struct {
		Parent   int64 `json:"parent"`
		Position int   `json:"position"`
	}
	if err := readJSONBody(r, &req, 8<<10); err != nil || req.Parent < 0 {
		fail(w, 400, "收藏移动参数无效")
		return
	}
	if err := s.DB.MoveFileBookmark(bookmarkOwner(r), id, req.Parent, req.Position); err != nil {
		fail(w, bookmarkStatus(err), bookmarkErrorText(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func bookmarkErrorText(err error) string {
	switch {
	case errors.Is(err, dbx.ErrBookmarkName):
		return "收藏名称无效"
	case errors.Is(err, dbx.ErrBookmarkFolder):
		return "收藏文件夹不存在"
	case errors.Is(err, dbx.ErrBookmarkNotFound):
		return "收藏不存在"
	case errors.Is(err, dbx.ErrBookmarkPathEmpty):
		return "收藏目录无效"
	case errors.Is(err, dbx.ErrBookmarkCycle):
		return "不能把收藏文件夹移动到自身或子文件夹"
	default:
		return "收藏操作失败"
	}
}
