package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pimg/internal/dbx"
)

const sessionCookie = "pimg_session"
const sessionTTL = 30 * 24 * time.Hour

// ---------- 会话与令牌 ----------

type authCtxKey struct{}

// resolveBearerToken shares token hashing, revocation and last-use tracking
// between admin authentication and token-only raw archive access.
func (s *Server) resolveBearerToken(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if id := strings.TrimSpace(tok); id != "" {
			sum := sha256.Sum256([]byte(id))
			if t, err := s.DB.GetTokenByHash(hex.EncodeToString(sum[:])); err == nil && t.Scope == "admin" && t.OwnerID == dbx.AdminOwner {
				s.DB.TouchToken(hex.EncodeToString(sum[:]))
				return true
			}
		}
		return false
	}
	return false
}

// resolveAuth 依次尝试 Bearer Token 与会话 Cookie。
func (s *Server) resolveAuth(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return s.resolveBearerToken(r)
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		sum := sha256.Sum256([]byte(c.Value))
		if sess, err := s.DB.GetSession(hex.EncodeToString(sum[:])); err == nil {
			if sess.ExpiresAt > time.Now().Unix() && sess.OwnerID == dbx.AdminOwner {
				return true
			}
		}
	}
	return false
}

// tokenAuth requires an existing API token in the Authorization header.
// Session cookies and URL parameters never authorize stored archive bytes.
func (s *Server) tokenAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if !s.resolveBearerToken(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pimg"`)
			fail(w, 401, "请使用有效的 API 令牌访问归档文件")
			return
		}
		if !s.allowAdminSetup(w, r) {
			return
		}
		s.serveForeground(next, w, r)
	}
}

// auth 管理接口鉴权中间件（会话或 Bearer Token）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	next = s.fastConditional(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.resolveAuth(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pimg"`)
			fail(w, 401, "未登录或凭证无效")
			return
		}
		if !s.allowAdminSetup(w, r) {
			return
		}
		// Cookie 会话下的写请求校验 Origin（CSRF 缓解；Token 认证无 Cookie 风险）
		if r.Method != http.MethodGet && r.Header.Get("Authorization") == "" {
			if origin := r.Header.Get("Origin"); origin != "" {
				host := r.Host
				if !sameOrigin(origin, host) {
					fail(w, 403, "跨站请求被拒绝")
					return
				}
			}
		}
		s.serveForeground(next, w, r)
	}
}

func (s *Server) allowAdminSetup(w http.ResponseWriter, r *http.Request) bool {
	required, err := s.DB.AdminSetupRequired()
	if err != nil {
		fail(w, 503, "暂时无法读取账号状态")
		return false
	}
	if required && r.URL.Path != "/api/auth/me" && r.URL.Path != "/api/auth/logout" && r.URL.Path != "/api/auth/account" && r.URL.Path != "/api/auth/password" {
		fail(w, 403, "请先设置管理员新密码")
		return false
	}
	return true
}

func (s *Server) serveForeground(next http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead || strings.HasPrefix(r.URL.Path, "/media/") || strings.HasSuffix(r.URL.Path, "/content") || strings.HasSuffix(r.URL.Path, "/download") || strings.HasSuffix(r.URL.Path, "/export") || strings.HasSuffix(r.URL.Path, "/previews") {
		finish := s.Svc.BeginForeground()
		defer finish()
	}
	next(w, r)
}

func sameOrigin(origin, host string) bool {
	o := strings.ToLower(origin)
	o = strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
	o = strings.Split(o, "/")[0]
	return o == strings.ToLower(host)
}

// ---------- 登录限流 ----------

type attempt struct {
	count int
	until time.Time
}

type loginGuard struct {
	mu  sync.Mutex
	ips map[string]*attempt
}

func newLoginGuard() *loginGuard {
	return &loginGuard{ips: map[string]*attempt{}}
}

func (g *loginGuard) allow(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.ips[ip]
	if a == nil || time.Now().After(a.until) {
		return true
	}
	return a.count < 5
}

func (g *loginGuard) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.ips[ip]
	if a == nil || time.Now().After(a.until) {
		a = &attempt{until: time.Now().Add(15 * time.Minute)}
		g.ips[ip] = a
	}
	a.count++
}

func (g *loginGuard) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.ips, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 认证接口 ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		fail(w, 403, "跨站请求被拒绝")
		return
	}
	ip := clientIP(r)
	if !s.guard.allow(ip) {
		fail(w, 429, "尝试次数过多，请 15 分钟后再试")
		return
	}
	var req loginReq
	if err := readAuthBody(w, r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.factorMu.Lock()
	defer s.factorMu.Unlock()
	// Other completed requests may have exhausted the budget while this body
	// was being read. Recheck under the same lock as credential verification.
	if !s.guard.allow(ip) {
		fail(w, 429, "尝试次数过多，请 15 分钟后再试")
		return
	}
	user, hash, err := s.DB.GetAdmin()
	if err != nil {
		fail(w, 500, "管理员未初始化")
		return
	}
	owner := dbx.AdminOwner
	if req.Username != user || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		a, e := s.DB.AccountByUsername(req.Username)
		if e != nil || a.Disabled || bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(req.Password)) != nil {
			s.guard.fail(ip)
			fail(w, 401, "用户名或密码错误")
			return
		}
		owner, user = a.ID, a.Username
	}
	factor, err := s.DB.TwoFactor(owner)
	if err != nil {
		fail(w, 503, "无法读取两步验证状态")
		return
	}
	if factor != nil && factor.Enabled {
		if !s.guard.allow("factor-owner:"+owner) || !s.guard.allow("factor-ip:"+ip) {
			fail(w, 429, "验证次数过多，请 15 分钟后再试")
			return
		}
		if req.Code == "" {
			writeJSON(w, 200, map[string]any{"two_factor_required": true})
			return
		}
		if !s.verifyFactor(owner, req.Code, factor) {
			s.guard.fail(ip)
			s.factorFailure(r, owner)
			fail(w, 400, "验证码无效或已使用，请等待新动态码，或使用未使用的恢复码")
			return
		}
	}
	s.guard.reset(ip)
	s.guard.reset("factor-owner:" + owner)
	s.guard.reset("factor-ip:" + ip)
	tok := randomToken()
	sum := sha256.Sum256([]byte(tok))
	expires := time.Now().Add(sessionTTL).Unix()
	if err := s.DB.InsertOwnerSession(owner, hex.EncodeToString(sum[:]), expires, r.UserAgent(), ip); err != nil {
		fail(w, 500, "创建会话失败")
		return
	}
	public, _ := url.Parse(s.Svc.PublicBaseURL())
	// A configured HTTPS domain must not make direct HTTP IP access lose its
	// session. Forwarded HTTPS can only add Secure; it never overrides real TLS.
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") ||
		(public != nil && public.Scheme == "https" && strings.EqualFold(strings.TrimSuffix(r.Host, ":443"), strings.TrimSuffix(public.Host, ":443")))
	if s.Cfg.Server.CookieSecure != nil {
		secure = *s.Cfg.Server.CookieSecure
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	setup, _ := s.DB.AdminSetupRequired()
	writeJSON(w, 200, map[string]any{"ok": true, "username": user, "owner_id": owner, "role": map[bool]string{true: "admin", false: "user"}[owner == dbx.AdminOwner], "setup_required": owner == dbx.AdminOwner && setup})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		sum := sha256.Sum256([]byte(c.Value))
		s.DB.DeleteSession(hex.EncodeToString(sum[:]))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	owner := dbx.ContextOwner(r.Context())
	if owner != dbx.AdminOwner {
		var username string
		if err := s.DB.QueryRow(`SELECT username FROM accounts WHERE id=? AND disabled=0`, owner).Scan(&username); err != nil {
			fail(w, 401, "账号不可用")
			return
		}
		writeJSON(w, 200, map[string]any{"username": username, "owner_id": owner, "role": "user", "cache_scope": queryCacheScope(r), "version": Version})
		return
	}
	user, _, err := s.DB.GetAdmin()
	if err != nil {
		fail(w, 500, "管理员未初始化")
		return
	}
	setup, err := s.DB.AdminSetupRequired()
	if err != nil {
		fail(w, 503, "暂时无法读取账号状态")
		return
	}
	writeJSON(w, 200, map[string]any{"username": user, "owner_id": dbx.AdminOwner, "role": "admin", "cache_scope": queryCacheScope(r), "version": Version, "setup_required": setup})
}

// A domain-separated, non-authenticating identifier for browser snapshots.
// It is returned only after session validation and never reveals the DB token hash.
func queryCacheScope(r *http.Request) string {
	if r.Header.Get("Authorization") != "" {
		return ""
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("pimg-query-snapshot-v1\x00" + c.Value))
	return hex.EncodeToString(sum[:])
}

type passwordReq struct {
	Username *string `json:"username"`
	Old      string  `json:"old_password"`
	New      string  `json:"new_password"`
	Code     string  `json:"code"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if err := readAuthBody(w, r, &req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.factorMu.Lock()
	defer s.factorMu.Unlock()
	if (req.New != "" && len(req.New) < 8) || len(req.New) > 72 || r.URL.Path == "/api/auth/password" && req.New == "" {
		fail(w, 400, "新密码须为 8–72 字节")
		return
	}
	user, hash, err := s.DB.GetAdmin()
	if err != nil {
		fail(w, 500, "管理员未初始化")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Old)) != nil {
		fail(w, 400, "当前密码错误")
		return
	}
	factor, err := s.DB.TwoFactor(dbx.AdminOwner)
	if err != nil {
		fail(w, 503, "无法读取两步验证状态")
		return
	}
	if factor != nil && factor.Enabled {
		if !s.guard.allow("factor-owner:"+dbx.AdminOwner) || !s.guard.allow("factor-ip:"+clientIP(r)) {
			fail(w, 429, "验证次数过多，请 15 分钟后再试")
			return
		}
		if !s.verifyFactor(dbx.AdminOwner, req.Code, factor) {
			s.factorFailure(r, dbx.AdminOwner)
			fail(w, 400, "请填写有效且未使用的动态码或恢复码")
			return
		}
	}
	setup, err := s.DB.AdminSetupRequired()
	if err != nil {
		fail(w, 503, "暂时无法读取账号状态")
		return
	}
	if setup && (req.New == "" || req.New == req.Old) {
		fail(w, 400, "首次登录请设置不同于默认密码的新密码，至少 8 位")
		return
	}
	if req.Username != nil {
		user = strings.TrimSpace(*req.Username)
		if !dbx.ValidUsername(user) {
			fail(w, 400, "用户名须为 1–64 个字符")
			return
		}
	}
	nh := hash
	if req.New != "" {
		b, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 500, "无法更新密码")
			return
		}
		nh = string(b)
	}
	keepSession := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		sum := sha256.Sum256([]byte(c.Value))
		keepSession = hex.EncodeToString(sum[:])
	}
	if err := s.DB.ChangeAdminCredentials(user, hash, nh, keepSession); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "username": user})
}

// ---------- API Token ----------

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.DB.ListTokens()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, t := range toks {
		out = append(out, map[string]any{
			"scope": t.Scope, "topic_ids": t.TopicIDs,
			"id": t.ID, "name": t.Name, "prefix": t.Prefix,
			"created_at": t.CreatedAt, "last_used_at": t.LastUsedAt.Int64,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

type tokenReq struct {
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	TopicIDs []int  `json:"topic_ids"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req tokenReq
	if err := readJSONBody(r, &req, 4096); err != nil || req.Name == "" {
		fail(w, 400, "请填写令牌名称")
		return
	}
	if req.Scope == "" {
		req.Scope = "admin"
	}
	if req.Scope != "admin" && req.Scope != "diagnostics" {
		fail(w, 400, "不支持的令牌权限")
		return
	}
	if req.Scope == "diagnostics" && len(req.TopicIDs) > 0 {
		fail(w, 400, "只读诊断令牌不接受话题授权")
		return
	}

	tok := "pimg_" + randomToken()
	sum := sha256.Sum256([]byte(tok))
	t := &dbx.APIToken{
		Scope: req.Scope, OwnerID: "admin:1", TopicIDs: req.TopicIDs,
		Name: req.Name, TokenHash: hex.EncodeToString(sum[:]),
		Prefix: tok[:12], // 前缀可显示，完整令牌只出现一次
	}
	t.ID = "tk_" + randomToken()[:10]
	if err := s.DB.InsertToken(t); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "name": t.Name, "token": tok,
		"scope": t.Scope, "topic_ids": t.TopicIDs,
		"notice": "令牌只显示这一次，请立即保存"})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.RevokeToken(r.PathValue("id")); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var _ = sql.NullInt64{}
