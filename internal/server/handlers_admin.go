package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"pimg/internal/config"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/store"
)

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// handleGetSettings 返回站点设置（管理端全量）。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	logDays, err := s.DB.LogRetentionDays()
	if err != nil {
		fail(w, 500, "无法读取日志保留设置，请检查设置后重试")
		return
	}
	st := s.siteSettings()
	cold, hot := s.Svc.StorageSelection()
	r2Configured := false
	localCold, localHot := s.Cfg.Storage.LocalColdDir, s.Cfg.Storage.LocalHotDir
	for _, i := range s.Svc.StorageInstances() {
		if i.Driver == "r2" && i.Configured {
			r2Configured = true
		}
		if i.ID == "local" {
			localCold, localHot = i.Config.ColdDirectory, i.Config.Directory
			if i.Config.LocalLayout == 2 {
				localCold, localHot = filepath.Join(i.Config.Directory, "archives"), filepath.Join(i.Config.Directory, "display")
				if i.Config.LocalPaths != nil {
					localCold, localHot = i.Config.LocalPaths.Archives, i.Config.LocalPaths.Display
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"public_base_url": s.Svc.PublicBaseURL(),
		"site_name":       st.SiteName, "site_desc": st.SiteDesc, "logo_url": st.LogoURL,
		"theme": st.Theme, "footer_text": st.FooterText,
		"footer_links": st.FooterLinks,
		"icp_number":   st.ICPNumber, "icp_url": st.ICPURL,
		"police_number": st.PoliceNumber, "police_url": st.PoliceURL,
		"home_cover_mode": st.HomeCoverMode, "home_page_size": st.HomePageSize,
		"upload_max_mb": st.UploadMaxMB, "allowed_exts": st.AllowedExts,
		"default_allowed_exts": config.DefaultAllowedExts(), "effective_allowed_exts": s.Svc.AllowedExts(),
		"original_cache_hours": s.Svc.OriginalCacheHours(),
		"browser_watch_paths":  store.EncodeSourcePaths(s.Svc.BrowserWatchPaths()),
		"browser_index":        s.Svc.BrowserIndexStatus(),
		"log_retention_days":   logDays,
		"debug_enabled":        st.DebugEnabled, "debug_state": s.DB.Performance().State(),
		"retention_hours": st.RetentionHours, "presign_minutes": st.PresignMinutes,
		"tg_configured": s.Cfg.DevMock || s.Svc.TGReady() == nil,
		"r2_configured": r2Configured,
		"ffmpeg":        mediax.HasFFmpeg(),
		"cold_storage":  cold, "hot_storage": hot,
		"storage_options": s.Svc.StorageOptions(),
		"local_cold_dir":  localCold, "local_hot_dir": localHot,
	})
}

type settingsPatch map[string]any

var settingKeys = map[string]string{
	"browser_watch_paths": "browser_watch_paths",
	"public_base_url":     "public_base_url",
	"site_name":           "name", "site_desc": "desc", "logo_url": "logo",
	"footer_text": "footer", "icp_number": "icp_no", "icp_url": "icp_url",
	"footer_links":  "footer_links",
	"police_number": "police_no", "police_url": "police_url",
	"home_cover_mode": "cover_mode", "home_page_size": "page_size",
	"upload_max_mb": "upload_max_mb", "allowed_exts": "allowed_exts",
	"original_cache_hours": "original_cache_hours",
	"log_retention_days":   "log_retention_days",
	"debug_enabled":        "debug_enabled",
	"retention_hours":      "retention_hours", "presign_minutes": "presign_minutes",
	"cold_storage": "cold_storage", "hot_storage": "hot_storage",
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var patch settingsPatch
	if err := readJSONBody(r, &patch, 64<<10); err != nil {
		fail(w, 400, err.Error())
		return
	}
	// 校验
	var watchPaths []string
	for key, value := range patch {
		if _, ok := settingKeys[key]; !ok {
			fail(w, 400, "未知设置项 "+key)
			return
		}
		if key == "debug_enabled" {
			if _, ok := value.(bool); !ok {
				fail(w, 400, "debug_enabled 必须是布尔值")
				return
			}
			continue
		}
		if key == "browser_watch_paths" {
			items, ok := value.([]any)
			if !ok {
				fail(w, 400, "browser_watch_paths 必须是目录数组，清空请提交 []")
				return
			}
			paths := []string{}
			for _, item := range items {
				p, valid := item.(string)
				if !valid {
					fail(w, 400, "监听目录必须是字符串")
					return
				}
				paths = append(paths, p)
			}
			var err error
			watchPaths, err = s.Svc.ValidateBrowserWatchPaths(paths)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			encoded, _ := json.Marshal(store.EncodeSourcePaths(watchPaths))
			patch[key] = string(encoded)
			continue
		}
		if key == "footer_links" {
			links, err := parseFooterLinks(value)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			encoded, _ := json.Marshal(links)
			patch[key] = string(encoded)
			continue
		}
		if key != "home_page_size" && key != "upload_max_mb" && key != "retention_hours" && key != "presign_minutes" && key != "original_cache_hours" && key != "log_retention_days" {
			if text, ok := value.(string); !ok || len(text) > 4096 {
				fail(w, 400, key+" 必须是字符串且不超过 4096 字节")
				return
			}
		}
		if key == "public_base_url" {
			base := strings.TrimRight(strings.TrimSpace(value.(string)), "/")
			if base != "" {
				u, err := url.Parse(base)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(base, "\\\r\n\t ") {
					fail(w, 400, "公网地址须为完整的 http:// 或 https:// 站点地址，不含路径、账号或查询参数")
					return
				}
			}
			patch[key] = base
		}
	}
	cold, hot := s.Svc.StorageSelection()
	if v, ok := patch["cold_storage"]; ok {
		cold = v.(string)
	}
	if v, ok := patch["hot_storage"]; ok {
		hot = v.(string)
	}
	_, changeCold := patch["cold_storage"]
	_, changeHot := patch["hot_storage"]
	if changeCold || changeHot {
		if err := s.Svc.ValidateStorage(cold, hot); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	if v, ok := patch["home_cover_mode"]; ok {
		if v.(string) != "normal" && v.(string) != "placeholder" {
			fail(w, 400, "home_cover_mode 只支持 normal|placeholder")
			return
		}
	}
	for _, k := range []string{"home_page_size", "upload_max_mb", "retention_hours", "presign_minutes", "original_cache_hours", "log_retention_days"} {
		if v, ok := patch[k]; ok {
			n, valid := toInt(v)
			if !valid || n < 0 {
				fail(w, 400, k+" 必须是非负整数")
				return
			}
			if k == "home_page_size" && (n < 6 || n > 96) {
				fail(w, 400, "home_page_size 须在 6..96")
				return
			}
			if k == "upload_max_mb" && n > 65536 {
				fail(w, 400, "upload_max_mb 上限 65536")
				return
			}

			if k == "retention_hours" && n < 1 {
				fail(w, 400, "retention_hours 至少 1 小时")
				return
			}
			if k == "presign_minutes" && (n < 1 || n > 1440) {
				fail(w, 400, "presign_minutes 上限 1440")
				return
			}
			if k == "original_cache_hours" && (n < 1 || n > 8760) {
				fail(w, 400, "原件缓存时间须在 1..8760 小时")
				return
			}
			if k == "log_retention_days" && (n < 1 || n > dbx.MaxLogRetentionDays) {
				fail(w, 400, "日志保留时间须在 1..3650 天")
				return
			}
		}
	}
	preparedEnable := false
	defer func() {
		if preparedEnable && !s.DB.Performance().Enabled() {
			_ = s.DB.Performance().SetEnabled(false)
		}
	}()
	if value, ok := patch["debug_enabled"].(bool); ok && value {
		preparedEnable = !s.DB.Performance().Enabled()
		if err := s.DB.Performance().Prepare(); err != nil {
			fail(w, 503, "无法创建性能诊断数据库，请检查诊断目录权限和可用空间")
			return
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	for key, dbKey := range settingKeys {
		v, ok := patch[key]
		if !ok {
			continue
		}
		var val string
		switch t := v.(type) {
		case bool:
			val = fmt.Sprintf("%t", t)
		case string:
			val = t
		case float64:
			val = fmt.Sprintf("%d", int64(t))
		default:
			fail(w, 400, key+" 类型错误")
			return
		}
		if key != "footer_links" && key != "browser_watch_paths" && len(val) > 4096 {
			fail(w, 400, key+" 过长")
			return
		}
		if _, err := tx.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, "site."+dbKey, val); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if _, changed := patch["browser_watch_paths"]; changed {
		s.Svc.ApplyBrowserWatchPaths(watchPaths)
	}
	if changeCold || changeHot {
		if err := s.Svc.SetStorage(cold, hot); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := s.loadSiteSettings(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.applyDynamicFromSettings()
	if _, changed := patch["debug_enabled"]; changed {
		if err := s.DB.Performance().SetEnabled(s.siteSettings().DebugEnabled); err != nil {
			s.Log.Warn("应用调试开关时诊断数据处理失败", "err", err)
		}
	}
	if _, changed := patch["log_retention_days"]; changed && s.Eng != nil {
		s.Eng.WakeLogCleanup()
	}
	s.handleGetSettings(w, r)
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || math.Trunc(t) != t || t > float64(1<<31-1) {
			return 0, false
		}
		return int(t), true
	case int:
		return t, true
	}
	return 0, false
}

// applyDynamicFromSettings 把上传限制/保留期等热参数同步到服务层。
func (s *Server) applyDynamicFromSettings() {
	m, err := s.DB.AllSettings()
	if err != nil {
		return
	}
	if base, ok := m["site.public_base_url"]; ok {
		s.Svc.SetPublicBaseURL(base)
	}
	get := func(k string) int {
		n := 0
		fmt.Sscanf(m["site."+k], "%d", &n)
		return n
	}
	var exts []string
	for _, e := range strings.Split(m["site.allowed_exts"], ",") {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			exts = append(exts, e)
		}
	}
	retention, presign, maxMB := get("retention_hours"), get("presign_minutes"), get("upload_max_mb")
	if retention <= 0 {
		retention = 24
	}
	if presign <= 0 {
		presign = 30
	}
	if len(exts) == 0 {
		exts = config.DefaultAllowedExts()
	}
	s.Svc.SetDynamic(retention, presign, maxMB, exts)
	s.Svc.SetOriginalCacheHours(get("original_cache_hours"))
}

// ---------- 状态 ----------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.DB.GetStats()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	cold, hot := s.Svc.StorageSelection()
	ctx, cancel := withTimeout(r.Context(), 8*time.Second)
	defer cancel()
	policy := s.Svc.StoragePolicy()
	names := map[string]string{}
	for _, i := range s.Svc.StorageInstances() {
		names[i.ID] = i.Name
	}
	checks := []map[string]string{}
	checked := map[string]error{}
	check := func(id, role, purpose string) error {
		key := role + ":" + id
		err, exists := checked[key]
		if !exists {
			err = s.Svc.CheckStorage(ctx, role, id)
			checked[key] = err
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		checks = append(checks, map[string]string{"instance_id": id, "name": names[id], "purpose": purpose, "status": boolStatus(err == nil, message)})
		return err
	}
	var coldErr error
	for _, id := range policy.Archives {
		err := check(id, "cold", "归档")
		if id == cold {
			coldErr = err
		}
	}
	hotErr := check(policy.Permanent, "hot", "永久发布")
	check(policy.Temporary, "hot", "临时存储")
	checkResult := func(provider string, err error) map[string]string {
		message := ""
		if err != nil {
			message = err.Error()
		}
		return map[string]string{"provider": provider, "status": boolStatus(err == nil, message)}
	}
	failed, _ := s.DB.ListTasks("failed", 30)
	failItems := make([]map[string]any, 0, len(failed))
	for _, t := range failed {
		failItems = append(failItems, map[string]any{
			"id": t.ID, "type": t.Type, "file_id": t.FileID.String,
			"last_error": t.LastError.String, "attempts": t.Attempts,
		})
	}
	writeJSON(w, 200, map[string]any{
		"stats": map[string]any{
			"total": stats.Total, "archived": stats.Archived, "failed": stats.Failed,
			"published": stats.Published, "show_home": stats.ShowHome,
			"total_size": stats.TotalSize,
			"temp_live":  stats.TempLive, "tasks_failed": stats.TasksFailed,
			"tasks_pending": stats.TasksPending,
		},
		"cold_storage":    checkResult(cold, coldErr),
		"storage_checks":  checks,
		"hot_storage":     checkResult(hot, hotErr),
		"ffmpeg":          mediax.HasFFmpeg(),
		"failed_tasks":    failItems,
		"version":         Version,
		"retention_hours": s.Svc.RetentionHours(),
		"presign_minutes": s.Svc.PresignMinutes(),
	})
}

func boolStatus(ok bool, msg string) string {
	if ok {
		return "ok"
	}
	return "error: " + msg
}
