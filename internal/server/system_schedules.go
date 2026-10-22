package server

import (
	"errors"
	"net/http"
	"time"

	"pimg/internal/dbx"
)

func (s *Server) handleSystemSchedules(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.PrepareSystemSchedules(time.Now().Unix(), false); err != nil {
		s.Log.Error("读取系统计划失败", "err", err)
		fail(w, 500, "读取系统计划失败")
		return
	}
	items, err := s.DB.SystemSchedules()
	if err != nil {
		fail(w, 500, "读取系统计划失败")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) handlePatchSystemSchedule(w http.ResponseWriter, r *http.Request) {
	var p dbx.SystemSchedulePatch
	if err := readJSONBody(r, &p, 4096); err != nil {
		fail(w, 400, "系统计划参数无效")
		return
	}
	if p.Enabled == nil && p.Cron == nil && p.Timezone == nil {
		fail(w, 400, "请指定需要修改的系统计划规则")
		return
	}
	item, err := s.DB.PatchSystemSchedule(r.PathValue("id"), p, time.Now().Unix())
	if errors.Is(err, dbx.ErrNotFound) {
		fail(w, 404, "系统计划不存在")
		return
	}
	if errors.Is(err, dbx.ErrInvalidSystemSchedule) {
		fail(w, 400, err.Error())
		return
	}
	if err != nil {
		s.Log.Error("保存系统计划失败", "err", err)
		fail(w, 500, "保存系统计划失败，原规则仍保留")
		return
	}
	if s.Eng != nil {
		s.Eng.WakeSchedules()
	}
	writeJSON(w, 200, map[string]any{"schedule": item})
}
