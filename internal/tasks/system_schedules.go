package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"pimg/internal/dbx"
)

func (e *Engine) operationExpiryLoop() {
	e.operationExpiryLoopEvery(time.Minute)
}

func (e *Engine) operationExpiryLoopEvery(interval time.Duration) {
	defer e.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if err := e.db.ExpireTaskOperations(time.Now().Unix()); err != nil {
				e.log.Warn("结束中断的接收操作失败", "err", err)
			}
		}
	}
}

func (e *Engine) systemScheduleLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		case <-e.logCleanupWake:
		}
		if e.ctx.Err() != nil {
			return
		}
		at := time.Now().Unix()
		if err := e.db.PrepareSystemSchedules(at, false); err != nil {
			e.log.Warn("恢复系统计划失败", "err", err)
			continue
		}
		for i := 0; i < 4; i++ {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err != nil {
				e.log.Error("生成系统计划租约失败", "err", err)
				break
			}
			s, err := e.db.ClaimSystemSchedule(at, hex.EncodeToString(raw[:]), 90)
			if errors.Is(err, dbx.ErrNotFound) {
				break
			}
			if err != nil {
				e.log.Warn("认领系统计划失败", "err", err)
				break
			}
			e.wg.Add(1)
			go e.runSystemSchedule(*s)
		}
	}
}

func (e *Engine) runSystemSchedule(s dbx.SystemSchedule) {
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(e.ctx, 6*time.Hour)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.db.HeartbeatSystemSchedule(s.ID, s.LeaseToken, s.TaskID, time.Now().Unix(), 90); err != nil {
					e.log.Warn("系统计划租约失效", "id", s.ID, "err", err)
					cancel()
					return
				}
			}
		}
	}()
	var result map[string]any
	var runErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("系统维护异常: %v", recovered)
			}
		}()
		result, runErr = e.executeSystemSchedule(ctx, s.ID)
	}()
	cancel()
	<-heartbeatDone
	if runErr == nil && e.ctx.Err() != nil {
		runErr = e.ctx.Err()
	}
	if err := e.db.FinishSystemSchedule(s.ID, s.LeaseToken, s.TaskID, time.Now().Unix(), result, runErr); err != nil {
		e.log.Warn("记录系统计划结果失败", "id", s.ID, "err", err)
	}
}

func (e *Engine) executeSystemSchedule(ctx context.Context, id string) (map[string]any, error) {
	switch id {
	case "log_cleanup":
		tasks, events, operations, passes := int64(0), int64(0), int64(0), 0
		for {
			if err := ctx.Err(); err != nil {
				return map[string]any{"changed": tasks + events, "tasks": tasks, "events": events, "operations": operations, "passes": passes}, err
			}
			pass, stop := context.WithTimeout(ctx, 30*time.Second)
			result, err := e.db.CleanupLogs(pass, time.Now().Unix())
			stop()
			tasks += result.Tasks
			events += result.Events
			operations += result.Operations
			passes++
			summary := map[string]any{"summary": fmt.Sprintf("已清理 %d 条操作日志和 %d 条接收记录", operations, events), "changed": tasks + events, "tasks": tasks, "events": events, "operations": operations, "passes": passes}
			if err != nil || !result.More {
				return summary, err
			}
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
				return summary, ctx.Err()
			case <-timer.C:
			}
		}
	case "storage_probe":
		previous := map[string]*bool{}
		for _, s := range e.svc.StorageInstances() {
			previous[s.ID] = s.Health.Available
		}
		if err := e.svc.RunStorageProbe(ctx); err != nil {
			return nil, err
		}
		checked, unavailable, changed := 0, 0, 0
		checks := []map[string]any{}
		for _, s := range e.svc.StorageInstances() {
			if !s.Enabled {
				continue
			}
			checked++
			checks = append(checks, map[string]any{"name": s.Name, "instance_id": s.ID, "available": s.Health.Available, "latency_ms": s.Health.LatencyMS, "error": s.Health.Error})
			if s.Health.Available != nil && !*s.Health.Available {
				unavailable++
			}
			if s.Health.Available != nil {
				before := previous[s.ID]
				if before == nil && !*s.Health.Available || before != nil && *before != *s.Health.Available {
					changed++
				}
			}
		}
		result := map[string]any{"summary": fmt.Sprintf("%d 个存储可用，%d 个不可用", checked-unavailable, unavailable), "checks": checks, "changed": changed, "checked": checked, "unavailable": unavailable}
		if unavailable > 0 {
			result["warning"] = fmt.Sprintf("%d 个存储当前不可用", unavailable)
		}
		return result, nil
	case "temp_cleanup":
		n, err := e.svc.CleanupExpiredTemp(ctx)
		e.db.CleanSessions()
		return map[string]any{"summary": fmt.Sprintf("已清理 %d 个到期临时副本", n), "changed": n, "removed": n}, err
	case "media_cleanup":
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := e.svc.CleanupMediaCacheResult()
		return map[string]any{"summary": fmt.Sprintf("已清理 %d 个到期媒体缓存", n), "changed": n, "removed": n}, err
	}
	return nil, errors.New("未知系统计划")
}
