package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleLogCleanup(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if err := readJSONBody(r, &request, 1024); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.DB.CleanupLogs(ctx, time.Now().Unix())
	if err != nil {
		s.Log.Error("清理日志失败", "err", err)
		fail(w, 500, "日志清理未完成，请检查保留设置后重试")
		return
	}
	if result.More && s.Eng != nil {
		s.Eng.WakeLogCleanup()
	}
	s.Log.Info("手动清理过期日志", "tasks", result.Tasks, "events", result.Events, "retention_days", result.RetentionDays)
	writeJSON(w, 200, result)
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSONBody(r, &request, 1024); err != nil {
		fail(w, 400, "删除日志请求无效")
		return
	}
	if request.Confirm != "DELETE_ALL_LOGS" {
		fail(w, 400, "删除全部日志需要 confirm=DELETE_ALL_LOGS")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.DB.ClearLogs(ctx, time.Now().Unix())
	if err != nil {
		s.Log.Error("删除日志失败", "err", err)
		fail(w, 500, "日志删除未完成，请重试；运行与恢复所需数据仍保留")
		return
	}
	if result.More && s.Eng != nil {
		s.Eng.WakeLogCleanup()
	}
	s.Log.Info("已删除现有日志", "hidden", result.Hidden, "removed_tasks", result.RemovedTasks, "removed_events", result.RemovedEvents)
	writeJSON(w, 200, result)
}
