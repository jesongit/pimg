package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/diagnostics"
)

func containsIP(ranges []netip.Prefix, address netip.Addr) bool {
	for _, p := range ranges {
		if p.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}

// Forwarding headers only have meaning when the TCP peer is an explicitly
// trusted proxy. Strip trusted hops from the right, never trust a client prefix.
func (s *Server) diagnosticClientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	ip = ip.Unmap()
	if !containsIP(s.diagnosticProxies, ip) {
		return ip
	}
	raw := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	parts := strings.Split(raw, ",")
	if len(raw) > 2048 || len(parts) > 16 || raw == "" {
		return netip.Addr{}
	}
	addresses := make([]netip.Addr, len(parts))
	for i, part := range parts {
		a, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || a.Zone() != "" {
			return netip.Addr{}
		}
		addresses[i] = a.Unmap()
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		ip = addresses[i]
		if !containsIP(s.diagnosticProxies, ip) {
			return ip
		}
	}
	return ip
}

func (s *Server) diagnosticAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Authorization, Origin")
		// Browser pages on other origins cannot use LAN credentials implicitly.
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, 403, "跨站诊断请求被拒绝")
			return
		}
		allowed := false
		if auth := r.Header.Get("Authorization"); auth != "" {
			if strings.HasPrefix(auth, "Bearer ") {
				token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
				if token != "" && len(token) <= 512 {
					sum := sha256.Sum256([]byte(token))
					hash := hex.EncodeToString(sum[:])
					if t, err := s.DB.GetTokenByHash(hash); err == nil && t.OwnerID == dbx.AdminOwner && (t.Scope == "admin" || t.Scope == "diagnostics") {
						allowed = true
						s.DB.TouchToken(hash)
					}
				}
			}
		} else {
			allowed = containsIP(s.diagnosticIPs, s.diagnosticClientIP(r))
		}
		if !allowed {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pimg-diagnostics"`)
			fail(w, 401, "请提供有效的只读诊断令牌或管理员令牌")
			return
		}
		next(w, r)
	}
}

func (s *Server) diagnosticText(text string) string {
	// Also mask known deployment secrets that have no recognisable text shape.
	if s.diagnosticRedactor != nil {
		text = s.diagnosticRedactor.Replace(text)
	}
	return diagnostics.Clip(diagnostics.Redact(text), 4096)
}

func (s *Server) handleDiagnosticLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := diagnosticNumber(q.Get("limit"), 100, 200)
	if err != nil || limit < 1 {
		fail(w, 400, "limit 须为 1–200")
		return
	}
	after, err := diagnosticNumber(q.Get("after"), 0, 1<<63-1)
	if err != nil {
		fail(w, 400, "after 须为非负记录编号")
		return
	}
	var level slog.Level
	switch strings.ToLower(q.Get("level")) {
	case "", "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		fail(w, 400, "level 只支持 info、warn、error")
		return
	}
	if len(q.Get("q")) > 256 {
		fail(w, 400, "搜索内容最多 256 字节")
		return
	}
	snapshot := s.Diagnostics.Read(diagnostics.Query{After: uint64(after), Limit: int(limit), Level: level, Search: q.Get("q")})
	for i := range snapshot.Items {
		e := &snapshot.Items[i]
		e.Message = s.diagnosticText(e.Message)
		for k, v := range e.Fields {
			e.Fields[k] = s.diagnosticText(v)
		}
	}
	writeJSON(w, 200, snapshot)
}

func diagnosticNumber(raw string, fallback, max int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > max {
		return 0, strconv.ErrRange
	}
	return n, nil
}

// Query only fixed columns. Do not export payloads, results, configurations,
// source bodies, filesystem paths or crash files through a diagnosis token.
func (s *Server) handleDiagnosticActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, e1 := diagnosticNumber(q.Get("limit"), 100, 200)
	before, e2 := diagnosticNumber(q.Get("before"), 0, 1<<63-1)
	since, e3 := diagnosticNumber(q.Get("since"), 0, 253402300799)
	if e1 != nil || limit < 1 || e2 != nil || e3 != nil || len(q.Get("state")) > 64 || len(q.Get("file_id")) > 256 {
		fail(w, 400, "日志筛选参数无效，limit 须为 1–200")
		return
	}
	source := q.Get("source")
	if source == "" {
		source = "tasks"
	}
	var query string
	switch source {
	case "tasks":
		query = `SELECT id,type,state,COALESCE(file_id,''),COALESCE(last_error,''),created_at,updated_at,attempts,max_attempts FROM tasks WHERE (?=0 OR id<?) AND updated_at>=? AND (?='' OR state=?) AND (?='' OR file_id=?) ORDER BY id DESC LIMIT ?`
	case "events":
		query = `SELECT id,event_type,event_type,COALESCE(file_id,''),COALESCE(reason,''),created_at,created_at,0,0 FROM ingest_events WHERE (?=0 OR id<?) AND created_at>=? AND (?='' OR event_type=?) AND (?='' OR file_id=?) ORDER BY id DESC LIMIT ?`
	default:
		fail(w, 400, "source 只支持 tasks、events")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := s.DB.ReadQueryContext(ctx, query, before, before, since, q.Get("state"), q.Get("state"), q.Get("file_id"), q.Get("file_id"), limit+1)
	if err != nil {
		fail(w, 503, "暂时无法读取历史日志")
		return
	}
	defer rows.Close()
	type item struct {
		ID          int64  `json:"id"`
		Type        string `json:"type"`
		State       string `json:"state"`
		FileID      string `json:"file_id,omitempty"`
		Error       string `json:"last_error,omitempty"`
		Created     int64  `json:"created_at"`
		Updated     int64  `json:"updated_at"`
		Attempts    int    `json:"attempts"`
		MaxAttempts int    `json:"max_attempts"`
	}
	items := []item{}
	next := int64(0)
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.ID, &i.Type, &i.State, &i.FileID, &i.Error, &i.Created, &i.Updated, &i.Attempts, &i.MaxAttempts); err != nil {
			fail(w, 503, "读取历史日志失败")
			return
		}
		if len(items) == int(limit) {
			next = items[len(items)-1].ID
			break
		}
		i.Type = s.diagnosticText(i.Type)
		i.State = s.diagnosticText(i.State)
		i.FileID = s.diagnosticText(i.FileID)
		i.Error = s.diagnosticText(i.Error)
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		fail(w, 503, "读取历史日志失败")
		return
	}
	writeJSON(w, 200, map[string]any{"source": source, "items": items, "next_before": next})
}

func (s *Server) handleDiagnosticStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dbOK := s.DB.PingContext(ctx) == nil
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	snapshot := s.Diagnostics.Read(diagnostics.Query{Limit: 1})
	writer, reader := s.DB.Stats(), s.DB.ReadStats()
	writeJSON(w, 200, map[string]any{"version": Version, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "instance_id": snapshot.Instance, "started_at": snapshot.Started, "uptime_seconds": int64(time.Since(snapshot.Started).Seconds()), "database_ok": dbOK, "debug": s.DB.Performance().State(), "goroutines": runtime.NumGoroutine(), "heap_alloc_bytes": mem.HeapAlloc, "heap_sys_bytes": mem.HeapSys, "runtime_log_capacity": diagnostics.MaxEntries, "runtime_log_max_bytes": diagnostics.MaxBytes, "database_writer_wait_count": writer.WaitCount, "database_writer_wait_ms": writer.WaitDuration.Milliseconds(), "database_reader_wait_count": reader.WaitCount, "database_reader_wait_ms": reader.WaitDuration.Milliseconds()})
}
