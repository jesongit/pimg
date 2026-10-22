package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"pimg/internal/migration"
	"time"
)

func (s *Server) registerMigrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/migration", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"can_restore": migration.CanRestore(s.DB), "scopes": migration.AllScopes})
	}))
	mux.HandleFunc("POST /api/admin/migration/export", s.auth(s.handleMigrationExport))
	mux.HandleFunc("GET /api/admin/migration/export/{id}", s.auth(s.handleMigrationDownload))
	mux.HandleFunc("POST /api/admin/migration/import", s.auth(s.handleMigrationImport))
	mux.HandleFunc("POST /api/admin/migration/apply", s.auth(s.handleMigrationApply))
}
func (s *Server) handleMigrationExport(w http.ResponseWriter, r *http.Request) {
	if !s.migrationMu.TryLock() {
		fail(w, 409, "已有迁移操作进行中，请稍后重试")
		return
	}
	defer s.migrationMu.Unlock()
	var p struct {
		Scopes *[]string `json:"scopes"`
	}
	if e := readJSONBody(r, &p, 4096); e != nil {
		fail(w, 400, e.Error())
		return
	}
	scopes := migration.AllScopes
	if p.Scopes != nil {
		scopes = *p.Scopes
	}
	dir := migration.WorkDir(s.Cfg)
	if e := os.MkdirAll(dir, 0700); e != nil {
		fail(w, 500, "无法创建迁移目录")
		return
	}
	// Only expired, generated downloads are removed; source data is untouched.
	entries, _ := filepath.Glob(filepath.Join(dir, "export-*.tar.gz"))
	for _, p := range entries {
		if i, e := os.Stat(p); e == nil && time.Since(i.ModTime()) > 30*time.Minute {
			_ = os.Remove(p)
		}
	}
	id := migration.NewID()
	pname := filepath.Join(dir, "export-"+id+".tar.gz")
	f, e := os.OpenFile(pname, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		fail(w, 500, "无法创建迁移包")
		return
	}
	_, e = migration.Export(r.Context(), s.Svc, Version, scopes, f)
	ce := f.Close()
	if e != nil || ce != nil {
		_ = os.Remove(pname)
		s.Log.Error("迁移导出失败", "err", e)
		fail(w, 500, "导出失败，请检查磁盘空间和本地存储连接")
		return
	}
	info, _ := os.Stat(pname)
	writeJSON(w, 200, map[string]any{"download_url": "/api/admin/migration/export/" + id, "bytes": info.Size()})
}
func (s *Server) handleMigrationDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !migration.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(migration.WorkDir(s.Cfg), "export-"+id+".tar.gz")
	f, e := os.Open(p)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || time.Since(i.ModTime()) > 30*time.Minute {
		fail(w, 410, "下载已过期，请重新导出")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pimg-migration-%s.tar.gz"`, i.ModTime().Format("20060102-150405")))
	http.ServeContent(w, r, "pimg-migration.tar.gz", i.ModTime(), f)
}
func (s *Server) handleMigrationImport(w http.ResponseWriter, r *http.Request) {
	if !s.migrationMu.TryLock() {
		fail(w, 409, "已有迁移操作进行中，请稍后重试")
		return
	}
	defer s.migrationMu.Unlock()
	if !migration.CanRestore(s.DB) {
		fail(w, 409, "请在全新 Pimg 实例中恢复，现有账号和文件不会被覆盖")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<30)
	reader, e := r.MultipartReader()
	if e != nil {
		fail(w, 400, "请选择迁移包")
		return
	}
	part, e := reader.NextPart()
	if e != nil || part.FormName() != "file" {
		fail(w, 400, "请选择迁移包")
		return
	}
	defer part.Close()
	preview, e := migration.Stage(r.Context(), s.Cfg, part)
	if e != nil {
		fail(w, 400, "迁移包校验失败："+e.Error())
		return
	}
	writeJSON(w, 200, preview)
}
func (s *Server) handleMigrationApply(w http.ResponseWriter, r *http.Request) {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	if !migration.CanRestore(s.DB) {
		fail(w, 409, "目标实例已有数据，请使用全新实例")
		return
	}
	var p struct {
		ID string `json:"id"`
	}
	if e := readJSONBody(r, &p, 4096); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if e := migration.Commit(s.Cfg, p.ID); e != nil {
		fail(w, 400, e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"restarting": true})
	time.AfterFunc(700*time.Millisecond, func() {
		select {
		case s.Restart <- struct{}{}:
		default:
		}
	})
}
