package server

import (
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/store"
	"strconv"
	"strings"
	"time"
)

func (s *Server) accountAuth(next http.HandlerFunc) http.HandlerFunc {
	next = s.fastConditional(next)
	return func(w http.ResponseWriter, r *http.Request) {
		owner := ""
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			sum := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))))
			if token, e := s.DB.GetTokenByHash(hex.EncodeToString(sum[:])); e == nil && (token.Scope == "account" || token.Scope == "admin" && token.OwnerID == dbx.AdminOwner) {
				owner = token.OwnerID
				s.DB.TouchToken(token.TokenHash)
			}
		} else if cookie, e := r.Cookie(sessionCookie); e == nil {
			sum := sha256.Sum256([]byte(cookie.Value))
			if session, e := s.DB.GetSession(hex.EncodeToString(sum[:])); e == nil && session.ExpiresAt > time.Now().Unix() {
				owner = session.OwnerID
			}
		}
		if owner == "" || !s.DB.AccountActive(owner) {
			fail(w, 401, "未登录或账号已停用")
			return
		}
		if owner == dbx.AdminOwner && !s.allowAdminSetup(w, r) {
			return
		}
		if expected := r.Header.Get("X-Pimg-Owner"); expected != "" && expected != owner {
			fail(w, 401, "登录账号已改变，请重新进入图库")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Authorization") == "" && r.Header.Get("Origin") != "" && !sameOrigin(r.Header.Get("Origin"), r.Host) {
			fail(w, 403, "跨站请求被拒绝")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, r.WithContext(dbx.WithOwner(r.Context(), owner)))
	}
}

func (s *Server) ownFile(next http.HandlerFunc) http.HandlerFunc {
	return s.accountAuth(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.DB.OwnedFile(dbx.ContextOwner(r.Context()), r.PathValue("id")); err != nil {
			fail(w, 404, "文件不存在")
			return
		}
		s.serveForeground(next, w, r)
	})
}

func (s *Server) registerAccountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/accounts", s.auth(s.handleCreateAccount))
	mux.HandleFunc("GET /api/admin/accounts", s.auth(s.handleAccounts))
	mux.HandleFunc("PATCH /api/admin/accounts/{owner}", s.auth(s.handleAccountUpdate))
	mux.HandleFunc("DELETE /api/admin/accounts/{owner}", s.auth(s.handleAccountDelete))
	mux.HandleFunc("POST /api/admin/accounts/{owner}/password", s.auth(s.handleAccountPassword))
	mux.HandleFunc("PATCH /api/admin/accounts/{owner}/telegram", s.auth(s.handleAccountGroupTitle))
	mux.HandleFunc("POST /api/admin/accounts/telegram/refresh", s.auth(s.handleAccountNamesRefresh))
	mux.HandleFunc("POST /api/admin/accounts/{owner}/storage/retry", s.auth(s.handleAccountRetry))
	mux.HandleFunc("GET /api/account/me", s.accountAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"owner_id": dbx.ContextOwner(r.Context()), "upload_max_mb": s.Svc.MaxSizeMB(), "allowed_exts": s.Svc.AllowedExts()})
	}))
	mux.HandleFunc("GET /api/account/files", s.accountAuth(s.handleListFiles))
	mux.HandleFunc("GET /api/account/files/timeline", s.accountAuth(s.handleFileTimeline))
	mux.HandleFunc("GET /api/account/files/navigation", s.accountAuth(s.handleFileNavigation))
	mux.HandleFunc("GET /api/account/files/previews", s.accountAuth(s.handlePreviews))
	mux.HandleFunc("POST /api/account/files/conversion-check", s.accountAuth(s.handleConversionCheck))
	mux.HandleFunc("POST /api/account/files/similar-purge", s.accountAuth(s.handleSimilarPurge))
	mux.HandleFunc("POST /api/account/batch", s.accountAuth(s.handleAccountBatch))
	mux.HandleFunc("POST /api/account/files/{id}/convert", s.ownFile(s.handleConvertFile))
	mux.HandleFunc("DELETE /api/account/files/{id}/local/{instance}", s.ownFile(s.handleRemoveLocalMedia))
	mux.HandleFunc("POST /api/account/upload", s.accountAuth(func(w http.ResponseWriter, r *http.Request) { s.serveForeground(s.handleUpload, w, r) }))
	mux.HandleFunc("GET /api/account/albums", s.accountAuth(s.handleAccountAlbums))
	mux.HandleFunc("POST /api/account/albums", s.accountAuth(s.handleAccountAlbumCreate))
	mux.HandleFunc("PATCH /api/account/albums/{id}", s.accountAuth(s.handleAccountAlbumPatch))
	mux.HandleFunc("DELETE /api/account/albums/{id}", s.accountAuth(s.handleAccountAlbumDelete))
	mux.HandleFunc("PATCH /api/account/files/{id}", s.ownFile(s.handleAccountFilePatch))
	for _, route := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "", s.handleGetFile}, {"GET", "/content", s.handleContent}, {"GET", "/download", s.handleDownload}, {"GET", "/links", s.handleFileLinks},
		{"POST", "/publish", s.handlePublish}, {"POST", "/unpublish", s.handleUnpublish}, {"POST", "/temp", s.handleTempView}, {"POST", "/original", s.handleOriginalView}, {"POST", "/verify", s.handleVerify}, {"POST", "/retry", s.handleRetry}, {"DELETE", "", s.handleDeleteFile},
	} {
		mux.HandleFunc(route.method+" /api/account/files/{id}"+route.path, s.ownFile(route.handler))
	}
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := readJSONBody(r, &req, 4096); err != nil || len(req.Password) < 8 || len(req.Password) > 72 || !dbx.ValidUsername(strings.TrimSpace(req.Username)) {
		fail(w, 400, "账号名不能为空，密码至少 8 位")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "无法创建账号")
		return
	}
	owner := "user:" + ids.New()
	if err = s.DB.CreateAccount(owner, strings.TrimSpace(req.Username), string(hash)); err != nil {
		fail(w, 400, "账号名已存在或参数无效")
		return
	}
	if err = s.Svc.ReconcileAccountSpaces(); err != nil {
		fail(w, 500, "账号已创建，归档初始化待重试")
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 201, map[string]any{"id": owner, "username": req.Username})
}
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.ManagedAccounts()
	if err != nil {
		fail(w, 500, "读取账号失败")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSONBody(r, &req, 4096); err != nil || req.Confirm != "DELETE" {
		fail(w, 400, "请确认永久删除用户及其全部文件和归档")
		return
	}
	owner := r.PathValue("owner")
	id, err := s.DB.QueueAccountDelete(owner)
	if err != nil {
		if err == dbx.ErrNotFound {
			fail(w, 404, "用户不存在")
		} else {
			fail(w, 400, err.Error())
		}
		return
	}
	if err = s.DB.CancelDeletingAccountFileTasks(owner); err != nil {
		fail(w, 500, "删除请求已保存，等待后台清理任务")
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 202, map[string]any{"task_id": id})
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username *string `json:"username"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSONBody(r, &req, 4096); err != nil || req.Username == nil && req.Disabled == nil {
		fail(w, 400, "请填写要修改的用户名或账号状态")
		return
	}
	if req.Username != nil {
		*req.Username = strings.TrimSpace(*req.Username)
	}
	if err := s.DB.UpdateManagedAccount(r.PathValue("owner"), req.Username, req.Disabled, ""); err != nil {
		if err == dbx.ErrNotFound {
			fail(w, 404, "账号不存在")
		} else {
			fail(w, 400, err.Error())
		}
		return
	}
	if req.Disabled != nil && !*req.Disabled {
		if err := s.Svc.ReconcileAccountSpaces(); err != nil {
			fail(w, 500, "账号已启用，归档初始化待重试")
			return
		}
		if s.Eng != nil {
			s.Eng.Wake()
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSONBody(r, &req, 4096); err != nil || len(req.Password) < 8 || len(req.Password) > 72 {
		fail(w, 400, "密码至少 8 位；过长的密码请缩短后重试")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "无法设置密码")
		return
	}
	if err = s.DB.UpdateManagedAccount(r.PathValue("owner"), nil, nil, string(hash)); err != nil {
		if err == dbx.ErrNotFound {
			fail(w, 404, "账号不存在")
		} else {
			fail(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAccountGroupTitle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSONBody(r, &req, 2048); err != nil {
		fail(w, 400, "请填写群名")
		return
	}
	id, err := s.Svc.QueueAccountGroupTitle(r.PathValue("owner"), req.Name)
	if err != nil {
		if err == dbx.ErrNotFound {
			fail(w, 404, "账号归档群不存在")
		} else {
			fail(w, 400, err.Error())
		}
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 202, map[string]any{"task_id": id})
}

func (s *Server) handleAccountNamesRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.RefreshAccountGroupNames(r.Context()); err != nil {
		s.telegramError(w, err)
		return
	}
	s.handleAccounts(w, r)
}
func (s *Server) handleAccountRetry(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !s.DB.AccountActive(owner) {
		fail(w, 404, "账号不存在")
		return
	}
	_, err := s.DB.Exec(`UPDATE account_telegram SET state=CASE WHEN state='blocked' THEN 'failed' ELSE state END,retry_after=CASE WHEN last_error LIKE '%FLOOD_WAIT%' AND retry_after>unixepoch() THEN retry_after ELSE 0 END WHERE owner_id=?`, owner)
	if err == nil {
		_, err = s.DB.Exec(`UPDATE tasks SET state='pending',attempts=0,run_after=MAX(unixepoch(),COALESCE((SELECT retry_after FROM account_telegram WHERE owner_id=?),0)),last_error=NULL WHERE type=? AND file_id=? AND state='failed'`, owner, store.TaskAccountSpace, "tg-space:"+owner)
	}
	if err == nil {
		err = s.Svc.ReconcileAccountSpaces()
	}
	if err != nil {
		fail(w, 500, "无法重试归档初始化")
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 202, map[string]bool{"ok": true})
}
func (s *Server) handleAccountAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.DB.ListOwnerAlbums(dbx.ContextOwner(r.Context()))
	if err != nil {
		fail(w, 500, "读取相册失败")
		return
	}
	items := []map[string]any{}
	for _, a := range albums {
		if a.OwnerID == dbx.ContextOwner(r.Context()) {
			items = append(items, map[string]any{"id": a.ID, "name": a.Name, "count": a.Count, "is_unclassified": a.IsDefault})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) handleAccountAlbumCreate(w http.ResponseWriter, r *http.Request) {
	var req albumReq
	if err := readJSONBody(r, &req, 2048); err != nil || !validAccountAlbumName(req.Name) || req.TopicID != nil || req.OwnerID != "" {
		fail(w, 400, "请填写 1–128 字的相册名称，TG 话题由系统分配")
		return
	}
	id, err := s.createAccountAlbum(dbx.ContextOwner(r.Context()), strings.TrimSpace(req.Name))
	if err != nil {
		fail(w, 500, "创建相册失败")
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": req.Name})
}

func (s *Server) createAccountAlbum(owner, name string) (int64, error) {
	id, err := s.DB.InsertOwnerAlbum(owner, name)
	if err != nil {
		return 0, err
	}
	if s.Svc.UsesStorageDriver("telegram") {
		_, err = s.DB.EnqueueUniqueTask(dbx.TaskTelegramRenameAlbum, dbx.TelegramAlbumTaskKey(id), dbx.JSONValue(map[string]any{"album_id": id, "name": name}))
		if err != nil {
			return 0, err
		}
		if s.Eng != nil {
			s.Eng.Wake()
		}
	}
	return id, nil
}
func (s *Server) handleAccountFilePatch(w http.ResponseWriter, r *http.Request) {
	var req patchFileReq
	if err := readJSONBody(r, &req, 8192); err != nil {
		fail(w, 400, "修改参数无效")
		return
	}
	if req.ShowHome != nil {
		fail(w, 403, "无权更改站点首页")
		return
	}
	if req.AlbumID != nil {
		if *req.AlbumID <= 0 {
			id, err := s.DB.EnsureOwnerDefaultAlbum(dbx.ContextOwner(r.Context()))
			if err != nil {
				fail(w, 500, "默认相册不可用")
				return
			}
			req.AlbumID = &id
		}
		if _, err := s.DB.OwnedAlbum(dbx.ContextOwner(r.Context()), *req.AlbumID); err != nil {
			fail(w, 404, "相册不存在")
			return
		}
	}
	// Reuse the existing mutation pipeline after complete ownership validation.
	body := `{"name":` + strconv.Quote(req.Name)
	if req.AlbumID != nil {
		body += `,"album_id":` + strconv.FormatInt(*req.AlbumID, 10)
	}
	body += "}"
	r.Body = io.NopCloser(strings.NewReader(body))
	s.handlePatchFile(w, r)
}
