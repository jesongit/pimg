// Package server 提供 HTTP API 与前端静态资源。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	frontendembed "pimg/frontend"
	"pimg/internal/config"
	"pimg/internal/dbx"
	"pimg/internal/diagnostics"
	"pimg/internal/store"
	"pimg/internal/tasks"
)

var frontendFS = frontendembed.FS

// Version 版本号。
const Version = "1.0.38"

type Server struct {
	Cfg                              *config.Config
	DB                               *dbx.DB
	Svc                              *store.Service
	Eng                              *tasks.Engine
	Log                              *slog.Logger
	Diagnostics                      *diagnostics.Buffer
	diagnosticIPs, diagnosticProxies []netip.Prefix
	diagnosticRedactor               *strings.Replacer
	Restart                          chan struct{}
	migrationMu                      sync.Mutex
	queryValidators                  queryValidators

	siteMu     sync.RWMutex
	settingsMu sync.Mutex
	site       SiteSettings

	guard        *loginGuard
	factorMu     sync.Mutex
	guestAlbumID int64 // 固定「游客相册」：首页展示其内容
}

func New(cfg *config.Config, db *dbx.DB, svc *store.Service, eng *tasks.Engine, log *slog.Logger) (*Server, error) {
	s := &Server{Cfg: cfg, DB: db, Svc: svc, Eng: eng, Log: log, guard: newLoginGuard(), Restart: make(chan struct{}, 1)}
	var err error
	if s.diagnosticIPs, err = config.ParseIPRanges(cfg.Diagnostics.AllowIPs); err != nil {
		return nil, err
	}
	if s.diagnosticProxies, err = config.ParseIPRanges(cfg.Diagnostics.TrustedProxies); err != nil {
		return nil, err
	}
	s.Diagnostics = diagnostics.NewBuffer(cfg.MasterKey, cfg.Admin.Password, cfg.Telegram.APIHash, cfg.Telegram.BotToken, cfg.R2.SecretAccessKey, cfg.R2.AccessKeyID, cfg.R2.CDNAPIToken)
	pairs := []string{}
	for _, secret := range []string{cfg.MasterKey, cfg.Admin.Password, cfg.Telegram.APIHash, cfg.Telegram.BotToken, cfg.R2.SecretAccessKey, cfg.R2.AccessKeyID, cfg.R2.CDNAPIToken} {
		if secret != "" {
			pairs = append(pairs, secret, "[REDACTED]")
		}
	}
	s.diagnosticRedactor = strings.NewReplacer(pairs...)

	if err := s.loadSiteSettings(); err != nil {
		return nil, err
	}
	s.applyDynamicFromSettings()
	if db.Performance() == nil {
		resourceSample := diagnostics.NewResourceSampler(cfg.Paths.RuntimeRoot())
		p := diagnostics.NewPerformance(s.performanceDatabasePath(), func(ctx context.Context) map[string]any { return s.performanceSample(ctx, resourceSample) })
		db.SetPerformance(p)
	}
	if err := db.Performance().SetEnabled(s.siteSettings().DebugEnabled); err != nil {
		s.Log.Warn("性能诊断初始化失败，基础服务继续运行", "err", err)
	}
	return s, nil
}

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAccountRoutes(mux)
	s.registerMigrationRoutes(mux)
	mux.HandleFunc("GET /api/diagnostics/status", s.diagnosticAuth(s.handleDiagnosticStatus))
	mux.HandleFunc("GET /api/diagnostics/logs", s.diagnosticAuth(s.handleDiagnosticLogs))
	mux.HandleFunc("GET /api/diagnostics/activity", s.diagnosticAuth(s.handleDiagnosticActivity))
	mux.HandleFunc("GET /api/diagnostics/metrics", s.diagnosticAuth(s.handleDiagnosticMetrics))

	// 公共
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/site", s.handleSite)
	mux.HandleFunc("GET /api/home", s.handleHome)
	mux.HandleFunc("GET /api/exhibit/files/{id}/thumb", s.handleExhibitThumbnail)
	mux.HandleFunc("GET /api/placeholder.svg", s.handlePlaceholder)
	mux.HandleFunc("GET /media/{scope}/{key...}", s.Svc.ServeLocalMedia)
	mux.HandleFunc("GET /media/storage/{instance}/{scope}/{key...}", s.Svc.ServeLocalMedia)
	mux.HandleFunc("GET /media/tg/{id}/{kind}", s.tokenAuth(s.handleTelegramArchive))
	mux.HandleFunc("GET /media/archive/{id}/{instance}/{part}", s.tokenAuth(s.Svc.ServeStoredMedia))

	// 认证
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.registerTwoFactorRoutes(mux)
	mux.HandleFunc("POST /api/auth/logout", s.accountAuth(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.accountAuth(s.handleMe))
	mux.HandleFunc("POST /api/auth/password", s.auth(s.handleChangePassword))
	mux.HandleFunc("POST /api/auth/account", s.auth(s.handleChangePassword))

	// 管理 API
	mux.HandleFunc("GET /api/admin/ingest-events", s.auth(s.handleIngestEvents))
	mux.HandleFunc("POST /api/admin/upload", s.auth(s.handleUpload))
	mux.HandleFunc("GET /api/admin/files", s.auth(s.handleListFiles))
	mux.HandleFunc("GET /api/admin/files/timeline", s.auth(s.handleFileTimeline))
	mux.HandleFunc("GET /api/admin/files/navigation", s.auth(s.handleFileNavigation))
	mux.HandleFunc("GET /api/admin/files/previews", s.auth(s.handlePreviews))
	mux.HandleFunc("POST /api/admin/files/conversion-check", s.auth(s.handleConversionCheck))
	mux.HandleFunc("POST /api/admin/files/similar-purge", s.auth(s.handleSimilarPurge))
	mux.HandleFunc("GET /api/admin/files/{id}", s.auth(s.handleGetFile))
	mux.HandleFunc("GET /api/admin/files/{id}/links", s.auth(s.handleFileLinks))
	mux.HandleFunc("PATCH /api/admin/files/{id}", s.auth(s.handlePatchFile))
	mux.HandleFunc("DELETE /api/admin/files/{id}", s.auth(s.handleDeleteFile))
	mux.HandleFunc("POST /api/admin/files/{id}/publish", s.auth(s.handlePublish))
	mux.HandleFunc("POST /api/admin/files/{id}/convert", s.auth(s.handleConvertFile))
	mux.HandleFunc("POST /api/admin/files/{id}/transfer", s.auth(s.handleTransferFile))
	mux.HandleFunc("POST /api/admin/files/{id}/unpublish", s.auth(s.handleUnpublish))
	mux.HandleFunc("POST /api/admin/files/{id}/temp", s.auth(s.handleTempView))
	mux.HandleFunc("POST /api/admin/files/{id}/original", s.auth(s.handleOriginalView))
	mux.HandleFunc("DELETE /api/admin/files/{id}/local/{instance}", s.auth(s.handleRemoveLocalMedia))
	mux.HandleFunc("POST /api/admin/files/{id}/verify", s.auth(s.handleVerify))
	mux.HandleFunc("POST /api/admin/files/{id}/retry", s.auth(s.handleRetry))
	mux.HandleFunc("GET /api/admin/files/{id}/download", s.auth(s.handleDownload))
	mux.HandleFunc("GET /api/admin/files/{id}/content", s.auth(s.handleContent))
	mux.HandleFunc("GET /previews/{id}", s.handlePublicPreview)
	mux.HandleFunc("GET /published/{id}", s.Svc.ServeMediaPublication)
	mux.HandleFunc("GET /temporary/{id}", s.Svc.ServeMediaTemporary)
	mux.HandleFunc("POST /api/admin/batch", s.auth(s.handleBatch))
	s.registerOperations(mux)

	mux.HandleFunc("GET /api/admin/albums", s.auth(s.handleListAlbums))
	mux.HandleFunc("POST /api/admin/albums", s.auth(s.handleCreateAlbum))
	mux.HandleFunc("PATCH /api/admin/albums/{id}", s.auth(s.handlePatchAlbum))
	mux.HandleFunc("DELETE /api/admin/albums/{id}", s.auth(s.handleDeleteAlbum))

	mux.HandleFunc("GET /api/admin/tasks", s.auth(s.handleListTasks))
	mux.HandleFunc("GET /api/admin/tasks/summary", s.auth(s.handleTaskSummary))
	mux.HandleFunc("GET /api/admin/logs/receipts", s.auth(s.handleReceiptLogs))
	mux.HandleFunc("GET /api/admin/logs", s.auth(s.handleUnifiedLogs))
	mux.HandleFunc("POST /api/admin/logs/cleanup", s.auth(s.handleLogCleanup))
	mux.HandleFunc("POST /api/admin/logs/clear", s.auth(s.handleClearLogs))
	mux.HandleFunc("GET /api/admin/schedules", s.auth(s.handleSystemSchedules))
	mux.HandleFunc("PATCH /api/admin/schedules/{id}", s.auth(s.handlePatchSystemSchedule))
	mux.HandleFunc("GET /api/admin/logs/receipts/{id}", s.auth(s.handleReceiptDetails))
	mux.HandleFunc("POST /api/admin/tasks/{id}/retry", s.auth(s.handleRetryTask))
	mux.HandleFunc("POST /api/admin/tasks/{id}/cancel", s.auth(s.handleCancelTask))
	mux.HandleFunc("GET /api/admin/tasks/{id}/children", s.auth(s.handleTaskChildren))
	mux.HandleFunc("GET /api/admin/storage", s.auth(s.handleStorage))
	mux.HandleFunc("GET /api/admin/logs/tasks/{id}/files", s.auth(s.handleLogFiles))
	mux.HandleFunc("POST /api/admin/storage/probe", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.DB.EnqueueUniqueTask(dbx.TaskStorageProbe, "", "{}")
		if err != nil {
			fail(w, 500, "安排存储检测失败")
			return
		}
		if s.Eng != nil {
			s.Eng.Wake()
		}
		writeJSON(w, 202, map[string]any{"task_id": id})
	}))
	s.registerLocalImports(mux)
	s.registerFileBrowser(mux)
	mux.HandleFunc("POST /api/admin/storage/instances", s.auth(s.handleSaveStorageInstance))
	mux.HandleFunc("PATCH /api/admin/storage/instances/{id}", s.auth(s.handleSaveStorageInstance))
	mux.HandleFunc("POST /api/admin/storage/instances/{id}/check", s.auth(s.handleCheckStorage))
	mux.HandleFunc("PATCH /api/admin/storage/connection", s.auth(s.handleStorageConnection))
	mux.HandleFunc("PATCH /api/admin/storage/policy", s.auth(s.handleStoragePolicy))
	mux.HandleFunc("POST /api/admin/storage/import-rules", s.auth(s.handleRetiredStorageImports))
	mux.HandleFunc("PATCH /api/admin/storage/import-rules/{id}", s.auth(s.handleRetiredStorageImports))
	mux.HandleFunc("POST /api/admin/storage/import-rules/{id}/scan", s.auth(s.handleRetiredStorageImports))
	mux.HandleFunc("GET /api/admin/storage/receipts", s.auth(s.handleRetiredStorageImports))
	mux.HandleFunc("GET /api/admin/cold-repositories", s.auth(s.handleColdRepositories))
	mux.HandleFunc("POST /api/admin/cold-repositories", s.auth(s.handleAddColdRepository))
	mux.HandleFunc("PATCH /api/admin/cold-repositories/{id}", s.auth(s.handlePatchColdRepository))
	mux.HandleFunc("DELETE /api/admin/cold-repositories/{id}", s.auth(s.handleRemoveColdRepository))

	mux.HandleFunc("GET /api/admin/tokens", s.auth(s.handleListTokens))
	mux.HandleFunc("POST /api/admin/tokens", s.auth(s.handleCreateToken))
	mux.HandleFunc("DELETE /api/admin/tokens/{id}", s.auth(s.handleRevokeToken))

	mux.HandleFunc("GET /api/admin/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/admin/telegram", s.auth(s.handleTelegramStatus))
	mux.HandleFunc("POST /api/admin/telegram/config", s.auth(s.handleTelegramConfig))
	mux.HandleFunc("POST /api/admin/telegram/reconnect", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Svc.ReconnectTelegram(r.Context()); err != nil {
			s.telegramError(w, err)
			return
		}
		writeJSON(w, 200, s.Svc.TelegramStatus())
	}))
	mux.HandleFunc("POST /api/admin/telegram/auth/{action}", s.auth(s.handleTelegramAuth))
	mux.HandleFunc("POST /api/admin/telegram/logout", s.auth(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("action", "logout")
		s.handleTelegramAuth(w, r)
	}))
	mux.HandleFunc("GET /api/admin/telegram/groups", s.auth(s.handleTelegramGroups))
	mux.HandleFunc("GET /api/admin/telegram/topics", s.auth(s.handleTelegramTopics))
	mux.HandleFunc("POST /api/admin/albums/{id}/scan", s.auth(s.handleTelegramScan))
	mux.HandleFunc("POST /api/admin/albums/{id}/telegram", s.auth(s.handleTelegramBind))
	mux.HandleFunc("GET /api/admin/settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("PATCH /api/admin/settings", s.auth(s.handlePatchSettings))

	// 已移除或未知的 API 返回 404，不能落入前端页面回退。
	mux.HandleFunc("/api/", http.NotFound)

	// 静态前端（SPA 回退）
	mux.Handle("/", s.staticHandler())

	return s.wrap(conditionalJSON(mux))
}

func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 安全响应头
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		var span *diagnostics.Span
		if p := s.DB.Performance(); p.Enabled() && !strings.HasPrefix(r.URL.Path, "/api/diagnostics/") && !strings.HasPrefix(r.URL.Path, "/assets/") {
			id := randomToken()[:20]
			r = r.WithContext(diagnostics.WithRequestID(r.Context(), id))
			h.Set("X-Pimg-Request-ID", id)
			span = p.Start(r.Context(), "http", "")
		}
		completed := false
		if span != nil {
			defer func() {
				code := sw.code
				if !completed {
					code = 500
				}
				span.SetName(diagnosticRoute(r))
				span.FinishResult(nil, code, 0)
			}()
		}
		next.ServeHTTP(sw, r)
		completed = true
		// 日志只记录方法与路径，绝不记录查询串（可能含签名）与密钥
		if !strings.HasPrefix(r.URL.Path, "/assets/") && !(strings.HasPrefix(r.URL.Path, "/api/diagnostics/") && sw.code < 400) {
			fields := []any{"method", r.Method, "path", r.URL.Path, "code", sw.code, "ms", time.Since(start).Milliseconds()}
			if span != nil {
				fields = append(fields, "request_id", diagnostics.RequestID(r.Context()))
			}
			s.Log.Info("http", fields...)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------- 静态 ----------

func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(frontendFS, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// SPA 回退
			r.URL.Path = "/"
		}
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data: blob: https: http://127.0.0.1:*; media-src 'self' https: http://127.0.0.1:*; style-src 'self' 'unsafe-inline'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; connect-src 'self' blob:")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------- 通用辅助 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": msg}})
}

func readJSONBody(r *http.Request, v any, limit int64) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

var errUnauthorized = errors.New("未登录")

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "version": Version})
}

// ---------- 站点设置 ----------

// SiteSettings 站点与运行参数（存 settings 表，可后台修改）。
type SiteSettings struct {
	SiteName       string       `json:"site_name"`
	SiteDesc       string       `json:"site_desc"`
	LogoURL        string       `json:"logo_url"`
	Theme          string       `json:"theme"` // dark|light
	FooterText     string       `json:"footer_text"`
	FooterLinks    []FooterLink `json:"footer_links"`
	ICPNumber      string       `json:"icp_number"`
	ICPURL         string       `json:"icp_url"`
	PoliceNumber   string       `json:"police_number"`
	PoliceURL      string       `json:"police_url"`
	HomeCoverMode  string       `json:"home_cover_mode"` // normal|blur|placeholder
	HomePageSize   int          `json:"home_page_size"`
	UploadMaxMB    int          `json:"upload_max_mb"`
	AllowedExts    string       `json:"allowed_exts"`
	RetentionHours int          `json:"retention_hours"`
	PresignMinutes int          `json:"presign_minutes"`
	DebugEnabled   bool         `json:"debug_enabled"`
}

func defaultSite() SiteSettings {
	return SiteSettings{
		SiteName:       "Pimg",
		FooterLinks:    defaultFooterLinks(),
		SiteDesc:       "隐私优先的个人图床",
		Theme:          "dark",
		HomeCoverMode:  "normal",
		HomePageSize:   24,
		UploadMaxMB:    0,
		AllowedExts:    "",
		RetentionHours: 24,
		PresignMinutes: 30,
	}
}

func (s *Server) loadSiteSettings() error {
	m, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	st := defaultSite()
	get := func(k string, dst *string) {
		if v, ok := m["site."+k]; ok && v != "" {
			*dst = v
		}
	}
	get("name", &st.SiteName)
	get("desc", &st.SiteDesc)
	get("logo", &st.LogoURL)
	get("theme", &st.Theme)
	get("footer", &st.FooterText)
	if value, ok := m["site.footer_links"]; ok {
		var raw any
		if json.Unmarshal([]byte(value), &raw) == nil {
			if links, err := parseFooterLinks(raw); err == nil {
				st.FooterLinks = links
			}
		}
	}
	get("icp_no", &st.ICPNumber)
	get("icp_url", &st.ICPURL)
	get("police_no", &st.PoliceNumber)
	get("police_url", &st.PoliceURL)
	get("cover_mode", &st.HomeCoverMode)
	if v, ok := m["site.page_size"]; ok && v != "" {
		fmt.Sscanf(v, "%d", &st.HomePageSize)
	}
	get("allowed_exts", &st.AllowedExts)
	st.DebugEnabled = m["site.debug_enabled"] == "true"
	for key, dst := range map[string]*int{"upload_max_mb": &st.UploadMaxMB, "retention_hours": &st.RetentionHours, "presign_minutes": &st.PresignMinutes} {
		if value, ok := m["site."+key]; ok {
			fmt.Sscanf(value, "%d", dst)
		}
	}
	if st.RetentionHours <= 0 {
		st.RetentionHours = 24
	}
	if st.PresignMinutes <= 0 {
		st.PresignMinutes = 30
	}
	s.siteMu.Lock()
	s.site = st
	s.siteMu.Unlock()
	return nil
}

func (s *Server) siteSettings() SiteSettings {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.site
}

func (s *Server) saveSetting(key, val string) error {
	if err := s.DB.SetSetting("site."+key, val); err != nil {
		return err
	}
	return s.loadSiteSettings()
}

// ---------- 公共接口 ----------

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	st := s.siteSettings()
	writeJSON(w, 200, map[string]any{
		"site_name": st.SiteName, "site_desc": st.SiteDesc, "logo_url": st.LogoURL,
		"theme": st.Theme, "footer_text": st.FooterText,
		"footer_links": st.FooterLinks,
		"icp_number":   st.ICPNumber, "icp_url": st.ICPURL,
		"police_number": st.PoliceNumber, "police_url": st.PoliceURL,
	})
}

const placeholderSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="480" height="600" viewBox="0 0 480 600">
<rect width="480" height="600" fill="#16181d"/>
<g fill="#3a3f4a"><rect x="150" y="210" width="180" height="130" rx="8" fill="none" stroke="#3a3f4a" stroke-width="6"/>
<circle cx="205" cy="258" r="14"/><path d="M160 330l55-60 40 42 30-32 55 50" fill="none" stroke="#3a3f4a" stroke-width="10" stroke-linejoin="round" stroke-linecap="round"/></g>
<text x="240" y="400" font-family="system-ui,sans-serif" font-size="20" fill="#57606f" text-anchor="middle">私人影像</text>
</svg>`

func (s *Server) handlePlaceholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, placeholderSVG)
}

// filePublicJSON 首页条目（不含任何私有信息/临时签名）。
func (s *Server) filePublicJSON(f *dbx.File) map[string]any {
	st := s.siteSettings()
	thumb := "/api/placeholder.svg"
	if st.HomeCoverMode != "placeholder" && f.HasThumb {
		thumb = "/api/exhibit/files/" + f.ID + "/thumb"
	}
	w, h := f.Width, f.Height
	if w == 0 {
		w, h = 4, 5
	}
	return map[string]any{
		"id": f.ID, "name": f.Name, "kind": f.Kind, "ext": f.Ext,
		"width": w, "height": h, "duration": f.Duration, "size": f.Size,
		"thumb_url":  thumb,
		"created_at": f.CreatedAt, "published_at": f.PublishedAt.Int64,
		"media_at": f.MediaAt,
	}
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	page := atoiOr(r.URL.Query().Get("page"), 1)
	perPage := atoiOr(r.URL.Query().Get("per_page"), s.siteSettings().HomePageSize)
	w.Header().Set("Cache-Control", "no-store")
	files, total, err := s.DB.ListExhibitedFiles(page, perPage)
	if err != nil {
		s.Log.Error("首页查询失败", "err", err)
		fail(w, 500, "查询失败")
		return
	}
	items := make([]map[string]any, 0, len(files))
	for _, f := range files {
		items = append(items, s.filePublicJSON(f))
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "per_page": perPage})
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

var _ = context.Background
