package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"pimg/internal/dbx"
)

// Only the administrator upload entry point may receive on behalf of another
// account. Never derive this authority from an owner_id query or client header.
func (s *Server) uploadOwner(r *http.Request, albumID int64) (string, error) {
	owner := dbx.ContextOwner(r.Context())
	if albumID > 0 {
		a, err := s.DB.GetAlbum(albumID)
		if err != nil {
			return "", fmt.Errorf("相册不存在")
		}
		if strings.HasPrefix(r.URL.Path, "/api/admin/") {
			owner = a.OwnerID
		} else if a.OwnerID != owner {
			return "", fmt.Errorf("相册不存在")
		}
	}
	if !s.DB.AccountActive(owner) {
		return "", fmt.Errorf("请选择已启用用户的相册")
	}
	return owner, nil
}

// Account requests never accept an owner override. Administrator galleries
// default to self; viewing another account or all accounts is explicit.
func (s *Server) galleryOwner(r *http.Request) (string, error) {
	if strings.HasPrefix(r.URL.Path, "/api/account/") {
		return dbx.ContextOwner(r.Context()), nil
	}
	owner := r.URL.Query().Get("owner_id")
	if owner == "" || owner == "self" {
		return dbx.AdminOwner, nil
	}
	if owner == "all" {
		return "", nil
	}
	if owner == dbx.AdminOwner {
		return owner, nil
	}
	var exists bool
	if err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM accounts WHERE id=?)`, owner).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", errors.New("账号不存在")
	}
	return owner, nil
}

func (s *Server) handleAccountBatch(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if readJSONBody(r, &req, 64<<10) != nil || len(req.IDs) == 0 || len(req.IDs) > 200 {
		fail(w, 400, "请指定 1–200 个文件")
		return
	}
	if req.Action != "publish" && req.Action != "unpublish" && req.Action != "move_album" && req.Action != "purge" && req.Action != "set_media_time" && req.Action != "convert" {
		fail(w, 403, "无权执行此批量操作")
		return
	}
	owner := dbx.ContextOwner(r.Context())
	for _, id := range req.IDs {
		if _, err := s.DB.OwnedFile(owner, id); err != nil {
			fail(w, 404, "文件不存在")
			return
		}
	}
	if req.Action == "move_album" {
		if req.AlbumID <= 0 {
			id, err := s.DB.EnsureOwnerDefaultAlbum(owner)
			if err != nil {
				fail(w, 500, "默认相册不可用")
				return
			}
			req.AlbumID = id
		}
		if _, err := s.DB.OwnedAlbum(owner, req.AlbumID); err != nil {
			fail(w, 404, "相册不存在")
			return
		}
	}
	body, _ := json.Marshal(req)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	s.handleBatch(w, r)
}
