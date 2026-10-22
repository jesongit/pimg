package server

import (
	"net/http"
	"strconv"
	"strings"

	"pimg/internal/dbx"
)

func (s *Server) handleLogTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := 1
	var err error
	if raw := q.Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
	}
	if err != nil || page < 1 || page > 1000000 {
		fail(w, 400, "页码超出范围")
		return
	}
	search := strings.TrimSpace(q.Get("q"))
	if len(search) > 512 || len(q.Get("file_id")) > 256 {
		fail(w, 400, "搜索内容过长")
		return
	}
	state := q.Get("state")
	if state != "" && state != "done" && state != "failed" && state != "issues" && state != "canceled" {
		fail(w, 400, "日志状态无效")
		return
	}
	filter := dbx.LogQuery{FileID: q.Get("file_id"), Search: search, State: state, Page: page, Limit: 50}
	if raw := q.Get("group_id"); raw != "" {
		filter.GroupID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.GroupID < 1 {
			fail(w, 400, "操作编号无效")
			return
		}
	}
	entries, total, err := s.DB.QueryUnifiedLogs(filter)
	if err != nil {
		s.Log.Error("读取日志失败", "err", err)
		fail(w, 500, "读取日志失败，请重试")
		return
	}
	items := []map[string]any{}
	for _, entry := range entries {
		var row map[string]any
		if entry.Task != nil {
			context, e := s.DB.ActivityContext(entry.Task)
			if e != nil {
				fail(w, 500, "读取操作内容失败，请重试")
				return
			}
			row = activityTaskJSON(*entry.Task)
			row["context"] = context
		} else {
			v := entry.Receipt
			row = map[string]any{"id": v.ID, "type": v.Type, "state": v.Outcome, "file_id": v.FileID, "file_name": v.Name, "file_exists": v.Exists, "album_name": v.AlbumName, "last_error": v.Error, "reason": v.Reason, "created_at": v.CreatedAt, "updated_at": v.UpdatedAt, "done_at": v.UpdatedAt, "source": v.Source, "owner_id": v.OwnerID, "attempts": nil, "max_attempts": nil, "group": nil}
		}
		row["record"], row["record_key"] = entry.Record, entry.RecordKey
		items = append(items, row)
	}
	ids := []string{}
	for _, item := range items {
		if id, ok := item["file_id"].(string); ok && id != "" && item["file_exists"] == false {
			ids = append(ids, id)
		}
	}
	successors, e := s.DB.FileSuccessors(ids)
	if e != nil {
		fail(w, 500, "读取文件新位置失败，请重试")
		return
	}
	for _, item := range items {
		if id, ok := item["file_id"].(string); ok && successors[id] != nil {
			item["file_successor"] = successors[id]
		}
	}
	next := 0
	if page*50 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "next_page": next})
}
