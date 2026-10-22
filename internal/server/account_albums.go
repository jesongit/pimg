package server

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"pimg/internal/dbx"
)

func (s *Server) ownedEditableAlbum(w http.ResponseWriter, r *http.Request) (*dbx.Album, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 404, "相册不存在")
		return nil, false
	}
	a, err := s.DB.OwnedAlbum(dbx.ContextOwner(r.Context()), id)
	if err != nil {
		fail(w, 404, "相册不存在")
		return nil, false
	}
	if a.IsDefault || a.IsHome {
		fail(w, 400, "固定相册不能修改或删除")
		return nil, false
	}
	return a, true
}

func (s *Server) handleAccountAlbumPatch(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownedEditableAlbum(w, r)
	if !ok {
		return
	}
	var req albumReq
	if readJSONBody(r, &req, 2048) != nil || !validAccountAlbumName(req.Name) || req.TopicID != nil || req.OwnerID != "" {
		fail(w, 400, "请填写 1–128 字的相册名称")
		return
	}
	var err error
	if _, e := s.DB.GetTopicBinding(a.ID); e == nil {
		err = s.DB.RenameLinkedAlbum(a.ID, strings.TrimSpace(req.Name))
	} else {
		err = s.DB.RenameAlbum(a.ID, strings.TrimSpace(req.Name))
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func validAccountAlbumName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && utf8.RuneCountInString(name) <= 128 && !strings.ContainsAny(name, "\x00\r\n")
}

func (s *Server) handleAccountAlbumDelete(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownedEditableAlbum(w, r)
	if !ok {
		return
	}
	var task int64
	var err error
	owner := dbx.ContextOwner(r.Context())
	if b, e := s.DB.GetTopicBinding(a.ID); e == nil {
		if b.State != "active" && b.State != "deleting" {
			fail(w, 409, "相册归档正在初始化，请稍后重试删除")
			return
		}
		task, err = s.DB.QueueLinkedAlbumDelete(a.ID, dbx.TaskPayloadJSON(map[string]any{"album_id": a.ID}), owner)
	} else {
		err = s.DB.DeleteEmptyOwnerAlbum(owner, a.ID)
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task_id": task})
}
