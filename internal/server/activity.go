package server

import (
	"errors"
	"net/http"
	"pimg/internal/dbx"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleUnifiedLogs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("record") == "all" {
		s.handleLogTimeline(w, r)
		return
	}
	if err := s.DB.ExpireTaskOperations(time.Now().Unix()); err != nil {
		fail(w, 500, "读取操作状态失败")
		return
	}
	q := r.URL.Query()
	kind := q.Get("record")
	if kind == "" {
		kind = "tasks"
	}
	if kind != "tasks" && kind != "receipts" {
		fail(w, 400, "记录类型无效")
		return
	}
	page := 1
	var pageErr error
	if q.Get("page") != "" {
		page, pageErr = strconv.Atoi(q.Get("page"))
	}
	if pageErr != nil || page < 1 || page > 1000000 {
		fail(w, 400, "页码超出范围")
		return
	}
	from := int64(0)
	if q.Get("from") != "" {
		var e error
		from, e = strconv.ParseInt(q.Get("from"), 10, 64)
		if e != nil || from < 0 {
			fail(w, 400, "起始时间无效")
			return
		}
	}
	sort := q.Get("sort")
	if sort == "" {
		sort = "created_desc"
	}
	if sort != "created_desc" && sort != "created_asc" && sort != "updated_desc" && (sort != "active" || kind != "tasks") && (sort != "attempts_desc" || kind != "tasks") {
		fail(w, 400, "排序无效")
		return
	}
	state := q.Get("state")
	if state == "all" {
		state = ""
	}
	allowed := map[string]bool{"": true}
	if kind == "tasks" {
		for _, v := range []string{"active", "finished", "terminal", "pending", "running", "retrying", "stopping", "done", "failed", "canceled"} {
			allowed[v] = true
		}
	} else {
		for _, v := range []string{"processing", "archived", "published", "duplicate", "failed", "deleted", "ignored"} {
			allowed[v] = true
		}
	}
	if !allowed[state] {
		fail(w, 400, "状态无效")
		return
	}
	search := strings.TrimSpace(q.Get("q"))
	if len(search) > 512 {
		fail(w, 400, "搜索内容过长")
		return
	}
	filter := dbx.LogQuery{FileID: q.Get("file_id"), State: state, Type: q.Get("type"), Search: search, Sort: sort, From: from, Page: page, Limit: 50, View: "all"}
	filter.Grouped = kind == "tasks"
	if raw := q.Get("group_id"); raw != "" {
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id < 1 {
			fail(w, 400, "操作编号无效")
			return
		}
		filter.GroupID = id
	}
	items := []map[string]any{}
	var total int
	var err error
	if kind == "tasks" {
		var tasks []dbx.ActivityTask
		tasks, total, err = s.DB.QueryActivityTasks(filter)
		for _, task := range tasks {
			row := activityTaskJSON(task)
			row["record"] = "task"
			items = append(items, row)
		}
	} else {
		var receipts []dbx.LogReceipt
		if q.Get("grouped") == "1" {
			receipts, total, err = s.DB.QueryGroupedLogReceipts(filter)
		} else {
			receipts, total, err = s.DB.QueryLogReceipts(filter)
		}
		for _, v := range receipts {
			items = append(items, map[string]any{"record": "receipt", "id": v.ID, "type": v.Type, "state": v.Outcome, "file_id": v.FileID, "file_name": v.Name, "file_exists": v.Exists, "album_name": v.AlbumName, "last_error": v.Error, "reason": v.Reason, "created_at": v.CreatedAt, "updated_at": v.UpdatedAt, "source": v.Source, "owner_id": v.OwnerID, "attempts": nil, "max_attempts": nil, "group": v.Group, "record_key": v.RecordKey})
		}
	}
	if err != nil {
		s.Log.Error("读取统一日志失败", "err", err)
		fail(w, 500, "读取日志失败")
		return
	}
	next := 0
	if page*50 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "next_page": next})
}

func activityTaskJSON(a dbx.ActivityTask) map[string]any {
	t := a.Task
	if g := a.Group; g != nil {
		canCancel := (t.State == "pending" || t.State == "running") && !a.CancelRequested
		canRetry := (t.State == "failed" || t.State == "canceled") && g.InputState != "open" && (g.OperationID == 0 || g.Steps > 0 && (g.FailedSteps > 0 || g.CanceledFiles > 0))
		g.CanCancel, g.CanRetry = &canCancel, &canRetry
	}
	return map[string]any{"id": t.ID, "type": t.Type, "file_id": t.FileID.String, "state": t.State, "attempts": t.Attempts, "max_attempts": t.MaxAttempts, "last_error": t.LastError.String, "result": jsonOrNull(t.Result.String), "created_at": t.CreatedAt, "updated_at": t.UpdatedAt, "done_at": t.DoneAt.Int64, "run_after": t.RunAfter, "cancel_requested": a.CancelRequested, "idle_only": dbx.IdleTask(t.Type), "file_name": a.FileName, "album_id": a.AlbumID, "album_name": a.AlbumName, "owner_id": a.OwnerID, "group": a.Group, "file_exists": a.FileExists}
}

func (s *Server) handleTaskSummary(w http.ResponseWriter, r *http.Request) {
	c, err := s.DB.ActivityTaskCounts(r.URL.Query().Get("file_id"))
	if err != nil {
		fail(w, 500, "读取任务统计失败")
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleActivityTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	view := q.Get("view")
	if view != "current" && view != "attention" && view != "history" && view != "all" {
		fail(w, 400, "任务范围无效")
		return
	}
	page := atoiOr(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		fail(w, 400, "页码超出范围")
		return
	}
	items, total, err := s.DB.ActivityTasks(q.Get("file_id"), view, q.Get("state"), page, 50)
	if err != nil {
		fail(w, 500, "读取任务失败")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		out = append(out, activityTaskJSON(a))
	}
	next := 0
	if page*50 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": total, "next_page": next})
}

func (s *Server) handleReceiptLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	items, err := s.DB.IntakeReceipts(q.Get("file_id"), q.Get("outcome"), before)
	if err != nil {
		fail(w, 500, "读取上传与导入日志失败")
		return
	}
	next := int64(0)
	if len(items) > 50 {
		next = items[49].ID
		items = items[:50]
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_before": next})
}

func (s *Server) handleReceiptDetails(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	events, err := s.DB.ReceiptEvents(id)
	if errors.Is(err, dbx.ErrNotFound) {
		fail(w, 404, "记录不存在")
		return
	}
	if err != nil {
		fail(w, 500, "读取记录过程失败")
		return
	}
	fileID := ""
	for _, e := range events {
		if e.FileID != "" {
			fileID = e.FileID
		}
	}
	tasks := []map[string]any{}
	if fileID != "" {
		items, _, e := s.DB.ActivityTasks(fileID, "receipt", "", 1, 50)
		if e != nil {
			fail(w, 500, "读取关联任务失败")
			return
		}
		for _, a := range items {
			tasks = append(tasks, activityTaskJSON(a))
		}
	}
	writeJSON(w, 200, map[string]any{"events": events, "tasks": tasks})
}
