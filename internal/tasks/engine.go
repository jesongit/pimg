// Package tasks 是 SQLite 驱动的任务引擎：认领-执行-重试-崩溃恢复。
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/diagnostics"
	"pimg/internal/store"
)

type Engine struct {
	db      *dbx.DB
	svc     *store.Service
	workers int
	log     *slog.Logger

	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	wake           chan struct{}
	logCleanupWake chan struct{}
	activeMu       sync.Mutex
	active         map[int64]context.CancelFunc
}

func New(db *dbx.DB, svc *store.Service, workers int, log *slog.Logger) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	if workers <= 0 {
		workers = 2
	}
	return &Engine{db: db, svc: svc, workers: workers, log: log, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), logCleanupWake: make(chan struct{}, 1), active: map[int64]context.CancelFunc{}}
}

// Start 启动工作协程与清理循环。启动前先复位崩溃残留的 running 任务。
func (e *Engine) Start() error {
	if err := e.db.PrepareSystemSchedules(time.Now().Unix(), true); err != nil {
		return err
	}
	e.svc.StartTelegramRuntime(e.ctx)
	n, err := e.db.ResetRunningTasks()
	if err != nil {
		return err
	}
	if n > 0 {
		e.log.Info("复位中断的任务", "count", n)
	}
	if err := e.svc.QueueLocalImports(); err != nil {
		return err
	}
	if err := e.db.QueueEncryptedUnpublishes(); err != nil {
		return err
	}
	e.svc.CleanupLocalTemps()
	for i := 0; i < e.workers; i++ {
		e.wg.Add(1)
		go e.workerLoop()
	}
	e.wg.Add(1)
	go e.idleLoop()
	e.wg.Add(1)
	go e.systemScheduleLoop()
	e.wg.Add(1)
	go e.operationExpiryLoop()
	if e.svc.UserTelegramEnabled() {
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.svc.StartTelegramSync(e.ctx) }()
	}
	return nil
}

// Stop 优雅停止。
func (e *Engine) Stop() {
	e.cancel()
	e.svc.StopMediaLoads()
	e.wg.Wait()
}

// Wake 立即唤醒一个空闲 worker。
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) workerLoop() {
	defer e.wg.Done()
	for {
		if blocked, _ := e.db.IdleBlocksForeground(time.Now().Unix()); blocked {
			e.svc.InterruptIdle()
		}
		t, err := e.db.ClaimTask(time.Now().Unix(), 7200)
		if err != nil {
			if errors.Is(err, dbx.ErrNotFound) {
				select {
				case <-e.ctx.Done():
					return
				case <-e.wake:
				case <-time.After(2 * time.Second):
				}
				continue
			}
			e.log.Error("认领任务失败", "err", err)
			time.Sleep(3 * time.Second)
			continue
		}
		e.run(t)
	}
}

func (e *Engine) run(t *dbx.Task) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(e.ctx, 6*time.Hour)
	defer cancel()
	ctx = store.WithTaskID(ctx, t.ID)
	ctx = diagnostics.WithTaskID(ctx, t.ID)
	span := e.db.Performance().Start(ctx, "task", t.Type)
	var observedError error
	defer func() { span.Finish(observedError) }()
	if e.db.Performance().Enabled() {
		eligible := t.RunAfter
		if eligible < t.CreatedAt {
			eligible = t.CreatedAt
		}
		wait := time.Since(time.Unix(eligible, 0))
		if wait < 0 {
			wait = 0
		}
		e.db.Performance().Observe("queue_wait", t.Type, wait, 0)
	}
	e.activeMu.Lock()
	e.active[t.ID] = cancel
	e.activeMu.Unlock()
	defer func() { e.activeMu.Lock(); delete(e.active, t.ID); e.activeMu.Unlock() }()
	if requested, err := e.db.TaskCancelRequested(t.ID); err != nil || requested {
		if requested {
			e.db.FinishTaskCanceled(t.ID)
		} else {
			e.db.FailTask(t.ID, err.Error())
		}
		return
	}
	idle := dbx.IdleTask(t.Type)
	if idle {
		child, finish, err := e.svc.BeginIdle(ctx)
		if err != nil {
			if requested, _ := e.db.TaskCancelRequested(t.ID); requested {
				e.db.FinishTaskCanceled(t.ID)
			} else {
				e.db.PostponeIdleTask(t.ID)
			}
			return
		}
		ctx = child
		defer finish()
	} else if !dbx.PassiveTask(t) {
		var finish func()
		if t.Type == dbx.TaskArchive || t.Type == dbx.TaskDirectArchive {
			finish = e.svc.BeginForegroundTransfer()
		} else {
			finish = e.svc.BeginForeground()
		}
		defer finish()
	}
	result, err := e.safeRun(ctx, t)
	observedError = err
	if errors.Is(err, store.ErrLocalImportPending) || errors.Is(err, store.ErrLocalImportWaiting) || errors.Is(err, store.ErrAccountDeleteWaiting) {
		observedError = nil
		delay := 0
		if errors.Is(err, store.ErrLocalImportWaiting) || errors.Is(err, store.ErrAccountDeleteWaiting) {
			delay = 2
		}
		_, saveErr := e.db.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'pending' END,attempts=0,run_after=unixepoch()+?,lease_until=NULL,updated_at=unixepoch() WHERE id=?`, delay, t.ID)
		if saveErr != nil {
			e.log.Error("保存导入进度失败", "error", saveErr)
		}
		return
	}
	if requested, _ := e.db.TaskCancelRequested(t.ID); requested {
		e.db.FinishTaskCanceled(t.ID)
		return
	}
	if errors.Is(err, store.ErrTelegramArchiveDisabled) {
		e.db.FinishTask(t.ID, `{"skipped":"telegram_disabled"}`)
		return
	}
	if err != nil && t.Type == store.TaskAccountGroupTitle {
		var p struct {
			OwnerID string `json:"owner_id"`
		}
		_ = json.Unmarshal([]byte(t.Payload), &p)
		after := time.Now().Add(time.Minute).Unix()
		if space, e := e.db.TelegramSpace(p.OwnerID); e == nil && space.TitleRetryAfter > after {
			after = space.TitleRetryAfter
		}
		if _, saveErr := e.db.Exec(`UPDATE tasks SET state='pending',attempts=MAX(0,attempts-1),last_error=?,run_after=?,lease_until=NULL WHERE id=?`, err.Error(), after, t.ID); saveErr != nil {
			e.log.Error("保存群名重试失败", "error", saveErr)
		}
		return
	}
	if err != nil && t.Type == store.TaskAccountSpace {
		var payload struct {
			OwnerID string `json:"owner_id"`
		}
		_ = json.Unmarshal([]byte(t.Payload), &payload)
		after := time.Now().Add(time.Minute).Unix()
		state := "pending"
		if space, e := e.db.TelegramSpace(payload.OwnerID); e == nil {
			if space.RetryAfter > after {
				after = space.RetryAfter
			}
			if space.State == "blocked" {
				state = "failed"
			}
		}
		if _, saveErr := e.db.Exec(`UPDATE tasks SET state=?,attempts=MAX(0,attempts-1),last_error=?,run_after=?,lease_until=NULL WHERE id=?`, state, err.Error(), after, t.ID); saveErr != nil {
			e.log.Error("保存归档初始化重试失败", "error", saveErr)
		}
		return
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			if idle {
				e.db.PostponeIdleTask(t.ID)
				return
			}
			// 进程停止：任务回到 pending 由下次启动/其他 worker 处理
			e.db.FailTask(t.ID, "interrupted: "+err.Error())
			return
		}
		e.log.Warn("任务失败", "id", t.ID, "type", t.Type, "attempts", t.Attempts,
			"max", t.MaxAttempts, "err", err.Error())
		if ferr := e.db.FailTask(t.ID, err.Error()); ferr != nil {
			e.log.Error("标记任务失败时出错", "err", ferr)
		}
		if t.Type == dbx.TaskLocalImport || t.Type == dbx.TaskLocalImportIdle {
			if task, getErr := e.db.GetTask(t.ID); getErr == nil && task.State == "failed" {
				var p struct {
					BatchID int64 `json:"batch_id"`
				}
				if json.Unmarshal([]byte(t.Payload), &p) == nil {
					_, _ = e.db.Exec(`UPDATE local_import_batches SET state='failed',error=? WHERE id=? AND state='running'`, err.Error(), p.BatchID)
				}
			}
		}
		return
	}
	e.log.Info("任务完成", "id", t.ID, "type", t.Type, "file", t.FileID.String,
		"took", time.Since(start).Round(time.Millisecond).String())
	e.db.FinishTask(t.ID, result)
	if t.Type == dbx.TaskArchive || t.Type == dbx.TaskDirectArchive {
		_, _ = e.db.Exec(`UPDATE tasks SET run_after=unixepoch() WHERE type IN ('local_import','local_import_idle') AND state='pending' AND run_after>unixepoch()`)
		e.Wake()
	}
}

func (e *Engine) WorkerStats() (int, int) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	return e.workers, len(e.active)
}

func (e *Engine) safeRun(ctx context.Context, t *dbx.Task) (result string, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic: %v", r)
			e.log.Error("任务 panic", "id", t.ID, "type", t.Type, "panic", fmt.Sprint(r))
		}
	}()
	fileID := t.FileID.String
	switch t.Type {
	case store.TaskAccountGroupTitle:
		var p struct {
			OwnerID string `json:"owner_id"`
			Name    string `json:"name"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if p.OwnerID == "" || p.Name == "" {
			return "", errors.New("群名任务参数无效")
		}
		return "", e.svc.RunAccountGroupTitle(ctx, p.OwnerID, p.Name)
	case store.TaskLocalLayout:
		return "", e.svc.MigrateLocalArchiveFile(ctx, fileID)
	case dbx.TaskAccountDelete:
		var payload struct {
			OwnerID string `json:"owner_id"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &payload); err != nil {
			return "", err
		}
		return "", e.svc.RunAccountDelete(ctx, payload.OwnerID)
	case store.TaskAccountSpace:
		if !e.svc.UsesStorageDriver("telegram") {
			return "", store.ErrTelegramArchiveDisabled
		}
		var p struct {
			OwnerID string `json:"owner_id"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if p.OwnerID == "" {
			return "", errors.New("归档空间任务缺少账号")
		}
		space, err := e.svc.EnsureTelegramSpace(ctx, p.OwnerID)
		if err != nil {
			return "", err
		}
		id, provisionErr := e.db.EnsureOwnerDefaultAlbum(p.OwnerID)
		if provisionErr != nil {
			return "", provisionErr
		}
		if _, provisionErr = e.svc.EnsureTelegramAlbum(ctx, id); provisionErr != nil {
			return "", provisionErr
		}
		if err = e.svc.EnsureTelegramArchiveFolder(ctx, space); err != nil {
			return "", err
		}
		return dbx.JSONValue(space), nil
	case dbx.TaskStorageProbe:
		return "", e.svc.RunStorageProbe(ctx)
	case dbx.TaskArchiveSync:
		return "", e.svc.RunArchiveSync(ctx, fileID)
	case dbx.TaskSourceScan, dbx.TaskSourceCleanup:
		// Retired tasks may still exist in an upgraded database. Finish them
		// without scanning or deleting any source, including malformed payloads.
		return `{"skipped":"source_import_rules_removed"}`, nil
	case dbx.TaskLocalImportScan, dbx.TaskLocalImport, dbx.TaskLocalImportIdle:
		var p struct {
			BatchID int64 `json:"batch_id"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if t.Type == dbx.TaskLocalImportScan {
			return "", e.svc.RunLocalImportScan(ctx, p.BatchID)
		}
		return "", e.svc.RunLocalImport(ctx, p.BatchID)

	case dbx.TaskColdSync, dbx.TaskColdDelete:
		var p struct {
			RepositoryID int64    `json:"repository_id"`
			References   []string `json:"references"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if p.RepositoryID <= 0 {
			return "", errors.New("副仓任务参数无效")
		}
		if t.Type == dbx.TaskColdDelete {
			return "", e.svc.RunColdDelete(ctx, fileID, p.RepositoryID, p.References)
		}
		return "", e.svc.RunColdSync(ctx, fileID, p.RepositoryID)
	case dbx.TaskTelegramScan:
		return "", e.svc.RunTelegramScan(ctx, t.Payload)
	case dbx.TaskTelegramReceive:
		return "", e.svc.RunTelegramReceive(ctx, t.Payload)
	case dbx.TaskTelegramDeleteAlbum:
		var p struct {
			AlbumID int64 `json:"album_id"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		return "", e.svc.RunDeleteLinkedAlbum(ctx, p.AlbumID)
	case dbx.TaskTelegramMoveFile:
		var p struct {
			AlbumID int64 `json:"album_id"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		return "", e.svc.MoveTelegramFile(ctx, fileID, p.AlbumID)
	case dbx.TaskTelegramRenameAlbum:
		var p struct {
			AlbumID int64  `json:"album_id"`
			Name    string `json:"name"`
		}
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		err := e.svc.RunRenameLinkedAlbum(ctx, p.AlbumID, p.Name)
		if errors.Is(err, store.ErrTelegramAlbumSyncObsolete) {
			return `{"skipped":"album_removed_or_deleting"}`, nil
		}
		return "", err
	case dbx.TaskTransferOwner:
		id, err := e.svc.RunOwnerTransfer(ctx, fileID)
		return dbx.TaskPayloadJSON(map[string]any{"new_file_id": id, "cleanup_pending": true}), err
	case dbx.TaskConvert:
		id, err := e.svc.RunConversion(ctx, fileID)
		return dbx.TaskPayloadJSON(map[string]any{"new_file_id": id, "cleanup_pending": true}), err
	case dbx.TaskCleanupConverted:
		return "", e.svc.RunConvertedCleanup(ctx, fileID)
	case dbx.TaskArchive:
		return "", e.svc.RunArchive(ctx, fileID)
	case dbx.TaskDirectArchive:
		return "", e.svc.RunDirectArchive(ctx, fileID)
	case dbx.TaskImport:
		hdr := &carrier.Header{}
		if err := json.Unmarshal([]byte(t.Payload), hdr); err != nil {
			return "", fmt.Errorf("导入 payload 损坏: %w", err)
		}
		return "", e.svc.RunImport(ctx, fileID, hdr)
	case dbx.TaskTempMaterialize:
		p := store.TempPayload{}
		json.Unmarshal([]byte(t.Payload), &p)
		if p.Kind == "" {
			p.Kind = "original"
		}
		return "", e.svc.RunTempMaterialize(ctx, fileID, p.Kind)
	case dbx.TaskPublish:
		var p struct {
			AutoHome bool `json:"auto_home"`
		}
		json.Unmarshal([]byte(t.Payload), &p)
		return "", e.svc.RunPublish(ctx, fileID, p.AutoHome)
	case dbx.TaskUnpublish:
		return "", e.svc.RunUnpublish(ctx, fileID)
	case dbx.TaskVerify:
		res, err := e.svc.Verify(ctx, fileID)
		if err != nil && res == "" {
			return "", err
		}
		// 校验完成：结论（ok 与否）记录在 result，任务本身成功
		return res, nil
	case dbx.TaskPurgeFile:
		return "", e.svc.RunPurgeFile(ctx, fileID)
	case dbx.TaskCleanupTemp:
		_, err := e.svc.CleanupExpiredTemp(ctx)
		return "", err
	default:
		return "", fmt.Errorf("未知任务类型 %s", t.Type)
	}
}

// CancelTask leaves a running reference in place until its operation returns.
// A request racing with registration is caught by run's database preflight.
func (e *Engine) CancelTask(id int64) error {
	if err := e.db.RequestTaskCancel(id); err != nil {
		return err
	}
	e.InterruptTasks([]int64{id})
	e.Wake()
	return nil
}

// Database cancellation is committed by the caller before interrupting I/O.
func (e *Engine) InterruptTasks(ids []int64) {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	for _, id := range ids {
		if cancel := e.active[id]; cancel != nil {
			cancel()
		}
	}
}

func (e *Engine) idleLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var reconciled time.Time
	for {
		if e.ctx.Err() != nil {
			return
		}
		if err := e.db.ReconcileColdSync(); err != nil {
			e.log.Warn("归档任务检查失败", "err", err)
		}
		if time.Since(reconciled) > 30*time.Second {
			if err := e.svc.ReconcileStorage(); err != nil {
				e.log.Warn("存储任务入队失败", "err", err)
			}
			reconciled = time.Now()
		}
		t, err := e.db.ClaimIdleTask(time.Now().Unix(), int64((6*time.Hour)/time.Second))
		if err == nil {
			e.run(t)
			continue
		}
		if !errors.Is(err, dbx.ErrNotFound) {
			e.log.Warn("空闲任务认领失败", "err", err)
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
