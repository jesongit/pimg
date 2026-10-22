package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Separate data directories must never delete each other's diagnostic history,
// even when their deployments share a runtime directory.
func (s *Server) performanceDatabasePath() string {
	root, _ := filepath.Abs(s.Cfg.Paths.DataDir)
	root = filepath.ToSlash(root)
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(s.Cfg.Paths.RuntimeRoot(), "diagnostics", fmt.Sprintf("performance-%x.sqlite", sum[:6]))
}

func diagnosticRoute(r *http.Request) string {
	method := r.Method
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		method = "OTHER"
	}
	if r.Pattern == "" {
		return method + " unmatched"
	}
	return r.Pattern
}
func (s *Server) handleDiagnosticMetrics(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "15m"
	}
	windows := map[string]time.Duration{"5m": 5 * time.Minute, "15m": 15 * time.Minute, "60m": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}
	duration, ok := windows[window]
	if !ok {
		fail(w, 400, "window 只支持 5m、15m、60m、6h、24h、7d")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	data, err := s.DB.Performance().Query(ctx, duration)
	if err != nil {
		fail(w, 503, "暂时无法读取性能诊断数据")
		return
	}
	writeJSON(w, 200, data)
}
func (s *Server) performanceSample(ctx context.Context, resource func() map[string]any) map[string]any {
	v := resource()
	v["version"] = Version
	writer, reader := s.DB.Stats(), s.DB.ReadStats()
	v["database_writer"] = map[string]any{"in_use": writer.InUse, "idle": writer.Idle, "max_open": writer.MaxOpenConnections, "wait_count_total": writer.WaitCount, "wait_ms_total": writer.WaitDuration.Milliseconds()}
	v["database_reader"] = map[string]any{"in_use": reader.InUse, "idle": reader.Idle, "max_open": reader.MaxOpenConnections, "wait_count_total": reader.WaitCount, "wait_ms_total": reader.WaitDuration.Milliseconds()}
	if s.Eng != nil {
		workers, active := s.Eng.WorkerStats()
		v["task_workers_configured"] = workers
		v["active_task_attempts_including_idle"] = active
	}
	rows, err := s.DB.ReadQueryContext(ctx, `SELECT state,COUNT(*),COALESCE(MIN(CASE WHEN run_after<=unixepoch() THEN run_after END),0) FROM tasks WHERE state IN ('pending','running') GROUP BY state`)
	if err != nil {
		v["task_queue_available"] = false
		return v
	}
	queue := map[string]any{}
	for rows.Next() {
		var state string
		var count, oldest int64
		if err = rows.Scan(&state, &count, &oldest); err != nil {
			break
		}
		age := int64(0)
		if state == "pending" && oldest > 0 {
			age = time.Now().Unix() - oldest
			if age < 0 {
				age = 0
			}
		}
		queue[state] = map[string]any{"count": count, "oldest_eligible_wait_seconds": age}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	v["task_queue_available"] = err == nil
	if err == nil {
		v["task_queue"] = queue
	}
	return v
}
