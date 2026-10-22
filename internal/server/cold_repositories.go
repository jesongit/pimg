package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"pimg/internal/dbx"
)

func (s *Server) handleColdRepositories(w http.ResponseWriter, r *http.Request) {
	repos, err := s.DB.ColdRepositories()
	if err != nil {
		fail(w, 500, "读取副仓失败")
		return
	}
	items := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		// Each adapter owns its private credentials. Only the current local
		// adapter has public configuration fields; unknown driver config is hidden.
		config := map[string]any{}
		if repo.Driver == "local" {
			config = dbx.ColdRepositoryConfig(repo.Config)
		}
		items = append(items, map[string]any{"id": repo.ID, "name": repo.Name, "driver": repo.Driver, "role": "secondary", "enabled": repo.Enabled, "config": config, "pending": repo.Pending, "synced": repo.Synced, "failed": repo.Failed, "running": repo.Running, "last_synced": repo.LastSynced, "last_error": repo.LastError})
	}
	cold, _ := s.Svc.StorageSelection()
	writeJSON(w, 200, map[string]any{"primary": map[string]any{"id": "primary", "driver": cold, "role": "primary", "enabled": true}, "items": items, "drivers": s.Svc.ReplicaDrivers()})
}
func (s *Server) handleAddColdRepository(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var p struct {
		Name   string          `json:"name"`
		Driver string          `json:"driver"`
		Config json.RawMessage `json:"config"`
		Role   string          `json:"role"`
	}
	if err := readJSONBody(r, &p, 8<<10); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if p.Role != "" && p.Role != "secondary" {
		fail(w, 400, "只能添加副仓，主仓由存储方案指定且始终保留")
		return
	}
	id, err := s.Svc.AddColdRepository(p.Name, p.Driver, p.Config)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "enabled": false})
}
func replicaID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "主仓不能停用或移除，请在主仓选择中更换存储方案")
		return 0, false
	}
	return id, true
}
func (s *Server) handlePatchColdRepository(w http.ResponseWriter, r *http.Request) {
	id, ok := replicaID(w, r)
	if !ok {
		return
	}
	var p struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSONBody(r, &p, 1024); err != nil || p.Enabled == nil {
		fail(w, 400, "请指定是否启用副仓")
		return
	}
	if err := s.DB.SetColdRepositoryEnabled(id, *p.Enabled); err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			fail(w, 404, "副仓不存在")
		} else {
			fail(w, 500, "保存副仓失败")
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleRemoveColdRepository(w http.ResponseWriter, r *http.Request) {
	id, ok := replicaID(w, r)
	if !ok {
		return
	}
	if err := s.DB.RemoveColdRepository(id); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	if s.handleGroupedTaskAction(w, r, "cancel") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "任务编号无效")
		return
	}
	if task, e := s.DB.GetTask(id); e == nil && (task.State == "pending" || task.State == "running") && (task.Type == dbx.TaskLocalImportScan || task.Type == dbx.TaskLocalImport || task.Type == dbx.TaskLocalImportIdle) {
		var p struct {
			BatchID int64 `json:"batch_id"`
		}
		if json.Unmarshal([]byte(task.Payload), &p) == nil {
			if e = s.Svc.ChangeLocalImport(p.BatchID, "cancel"); e != nil {
				fail(w, 409, e.Error())
				return
			}
			if e = s.interruptImport(p.BatchID); e != nil {
				fail(w, 500, "停止导入失败")
				return
			}
			writeJSON(w, 202, map[string]any{"id": id, "cancel_requested": true})
			return
		}
	}
	if s.Eng != nil {
		err = s.Eng.CancelTask(id)
	} else {
		err = s.DB.RequestTaskCancel(id)
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	task, err := s.DB.GetTask(id)
	if err != nil {
		fail(w, 500, "读取任务状态失败")
		return
	}
	writeJSON(w, 202, map[string]any{"id": id, "state": task.State, "cancel_requested": true})
}
