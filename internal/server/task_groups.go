package server

import (
	"net/http"
	"pimg/internal/dbx"
	"strconv"
)

func (s *Server) handleTaskChildren(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	page := atoiOr(r.URL.Query().Get("page"), 1)
	if err != nil || id <= 0 || page < 1 || page > 1000000 {
		fail(w, 400, "任务编号或页码无效")
		return
	}
	items, total, err := s.DB.GroupedTaskMembers(id, page, 50)
	if err != nil {
		fail(w, 404, "任务不存在")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, activityTaskJSON(item))
	}
	next := 0
	if page*50 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": total, "next_page": next})
}

func (s *Server) interruptImport(id int64) error {
	rows, err := s.DB.Query(`SELECT id FROM tasks WHERE type IN('local_import_scan','local_import','local_import_idle')
 AND state='running' AND CASE WHEN json_valid(payload) THEN CAST(json_extract(payload,'$.batch_id') AS INTEGER) END=?`, id)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if s.Eng != nil {
		s.Eng.InterruptTasks(ids)
		s.Eng.Wake()
	}
	return nil
}

func (s *Server) handleGroupedTaskAction(w http.ResponseWriter, r *http.Request, action string) bool {
	if r.URL.Query().Get("group") != "1" {
		return false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "任务编号无效")
		return true
	}
	if task, e := s.DB.GetTask(id); e == nil && task.Type == dbx.TaskSystemSchedule {
		fail(w, 409, dbx.ErrSystemScheduleManaged.Error())
		return true
	}
	group, err := s.DB.GetTaskGroup(id)
	if err != nil {
		fail(w, 404, "任务不存在")
		return true
	}
	if group.Group == nil {
		fail(w, 409, "任务分组已变化，请刷新后重试")
		return true
	}
	if action == "cancel" {
		isImport := group.Group.Kind == "import"
		preserveArchive := isImport || group.Group.Kind == "upload"
		if isImport {
			if err = s.Svc.ChangeLocalImport(group.Group.BatchID, "cancel"); err != nil {
				fail(w, 409, err.Error())
				return true
			}
			if err = s.interruptImport(group.Group.BatchID); err != nil {
				fail(w, 500, "停止导入失败")
				return true
			}
		}
		running, count, e := s.DB.CancelTaskGroup(id, preserveArchive)
		if e != nil {
			fail(w, 500, "停止任务失败")
			return true
		}
		if s.Eng != nil {
			s.Eng.InterruptTasks(running)
			s.Eng.Wake()
		}
		writeJSON(w, 202, map[string]any{"ok": true, "affected": count, "accepted_archives_continue": preserveArchive})
		return true
	}
	if group.Group.Kind == "import" {
		b, e := s.Svc.LocalImportBatch(group.Group.BatchID)
		if e != nil {
			fail(w, 404, "导入批次不存在")
			return true
		}
		if b.State == "failed" || b.State == "done_errors" || b.State == "canceled" {
			if e = s.Svc.ChangeLocalImport(b.ID, "retry"); e != nil {
				fail(w, 409, e.Error())
				return true
			}
			if s.Eng != nil {
				s.Eng.Wake()
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return true
		}
	}
	count := 0
	if group.Group.OperationID > 0 {
		op, e := s.ownedOperation(r, group.Group.OperationID)
		if e != nil {
			fail(w, 404, "操作不存在")
			return true
		}
		if e = s.DB.RetryTaskOperation(op.ID, op.OwnerID); e != nil {
			fail(w, 409, e.Error())
			return true
		}
	}
	count, err = s.DB.RetryLatestTaskGroup(id)
	if err != nil {
		fail(w, 409, err.Error())
		return true
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "affected": count})
	return true
}

func (s *Server) interruptFileWork(id string) error {
	rows, err := s.DB.Query(`SELECT id FROM tasks WHERE file_id=? AND type!='purge_file' AND state='running' AND cancel_requested=1`, id)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, n)
	}
	err = rows.Err()
	rows.Close()
	if err == nil && s.Eng != nil {
		s.Eng.InterruptTasks(ids)
	}
	return err
}
