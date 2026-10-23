package server

import (
	"net/http"
	"pimg/internal/store"
)

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	var pending, failed, expired int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive_sync' AND state IN ('pending','running')`).Scan(&pending)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive_sync' AND state='failed'`).Scan(&failed)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM temp_objects WHERE deleted_at IS NULL AND expires_at<=unixepoch()`).Scan(&expired)
	writeJSON(w, 200, map[string]any{"data_root": s.Cfg.Paths.DataDir, "connection": s.Svc.StorageConnectionView(), "drivers": s.Svc.StorageDrivers(), "instances": s.Svc.StorageInstances(), "policy": s.Svc.StoragePolicy(), "pending_tasks": pending, "failed_tasks": failed, "expired_pending_cleanup": expired})
}
func (s *Server) handleSaveStorageInstance(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name    string               `json:"name"`
		Driver  string               `json:"driver"`
		Config  store.InstanceConfig `json:"config"`
		Enabled bool                 `json:"enabled"`
	}
	if e := readJSONBody(r, &p, 16<<10); e != nil {
		fail(w, 400, e.Error())
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	id, e := s.Svc.SaveStorageInstance(r.PathValue("id"), p.Name, p.Driver, p.Config, p.Enabled)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}
func (s *Server) handleStoragePolicy(w http.ResponseWriter, r *http.Request) {
	var p struct {
		store.StoragePolicy
		LocalMB *int `json:"local_mbps"`
	}
	if e := readJSONBody(r, &p, 8<<10); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if p.LocalMB != nil {
		if *p.LocalMB < 1 || *p.LocalMB > 1024 {
			fail(w, 400, "本地同步限速应为 1–1024 MiB/s")
			return
		}
		p.StoragePolicy.LocalMB = *p.LocalMB
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if e := s.Svc.SaveStoragePolicy(p.StoragePolicy); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if e := s.Svc.ReconcileStorage(); e != nil {
		fail(w, 500, e.Error())
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	s.handleStorage(w, r)
}
func (s *Server) handleCheckStorage(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "cold"
	}
	if e := s.Svc.CheckStorage(r.Context(), role, r.PathValue("id")); e != nil {
		fail(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Old clients receive an explicit retirement response; no rule or source is touched.
func (s *Server) handleRetiredStorageImports(w http.ResponseWriter, r *http.Request) {
	fail(w, http.StatusGone, "导入规则已移除，请使用图库中的导入；来源文件不会自动清理")
}

func (s *Server) handleStorageConnection(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ProxyURL *string `json:"proxy_url"`
	}
	if err := readJSONBody(r, &p, 4096); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := s.Svc.SaveStorageConnection(r.Context(), p.ProxyURL, nil, nil); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.handleStorage(w, r)
}
