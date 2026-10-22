package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// LatestFileOperations returns the latest user-triggered operation per file in
// one query, so list and detail views can follow asynchronous work consistently.
func (db *DB) LatestFileOperations(ids []string) (map[string]*Task, error) {
	items := make(map[string]*Task)
	if len(ids) == 0 {
		return items, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(`SELECT id,type,file_id,state,COALESCE(last_error,''),COALESCE(result,'') FROM tasks
		WHERE id IN (SELECT MAX(id) FROM tasks WHERE type IN ('publish','unpublish','verify','convert','transfer_owner','tg_move_file')
		AND file_id IN (`+strings.TrimRight(strings.Repeat("?,", len(ids)), ",")+`) GROUP BY file_id)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t := &Task{}
		if err := rows.Scan(&t.ID, &t.Type, &t.FileID, &t.State, &t.LastError, &t.Result); err != nil {
			return nil, err
		}
		items[t.FileID.String] = t
	}
	return items, rows.Err()
}

// 任务类型常量。
const (
	TaskArchive          = "archive"           // 加密并上传 TG（从本地明文临时文件）
	TaskDirectArchive    = "direct_archive"    // 首页直发：明文分片存 TG + 自动发布上首页
	TaskImport           = "import"            // 从 PNG 载体重建归档
	TaskTempMaterialize  = "temp_materialize"  // TG→解密→R2 临时桶
	TaskPublish          = "publish"           // TG→解密→R2 永久桶+校验
	TaskUnpublish        = "unpublish"         // 删永久对象+CDN 清理
	TaskVerify           = "verify"            // TG→解密→校验 sha256（不落盘）
	TaskPurgeFile        = "purge_file"        // 彻底删除：TG 消息+R2 对象+记录
	TaskCleanupTemp      = "cleanup_temp"      // 清理过期临时对象（循环触发）
	TaskTransferOwner    = "transfer_owner"    // Administrator initiated verified ownership transfer.
	TaskConvert          = "convert"           // Verified replacement followed by resumable source cleanup.
	TaskCleanupConverted = "cleanup_converted" // Retired source cleanup never blocks a verified replacement.
)

// EnqueueTask 创建任务（可带去重键：同 file+type+payloadKind 未完成则复用）。
func (db *DB) EnqueueTask(typ, fileID, payload string, operationIDs ...int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ts := now()
	res, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at)
		VALUES(?,?,?,'pending',?,?,?)`, typ, fileID, payload, ts, ts, ts)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachTaskOperation(tx, id, typ, fileID, payload, operationIDs...); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FindActiveTask 查找同文件同类型未完成任务。
func (db *DB) FindActiveTask(typ, fileID string) (*Task, error) {
	row := db.QueryRow(`SELECT id,type,file_id,payload,state,attempts,max_attempts,last_error,result,
		run_after,lease_until,created_at,updated_at,done_at FROM tasks
		WHERE type=? AND file_id=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, typ, fileID)
	return scanTaskRow(row)
}

func scanTaskRow(row interface{ Scan(...any) error }) (*Task, error) {
	t := &Task{}
	var fileID sql.NullString
	err := row.Scan(&t.ID, &t.Type, &fileID, &t.Payload, &t.State, &t.Attempts, &t.MaxAttempts,
		&t.LastError, &t.Result, &t.RunAfter, &t.LeaseUntil, &t.CreatedAt, &t.UpdatedAt, &t.DoneAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.FileID = fileID
	return t, nil
}

// ClaimTask 以租约方式认领一个到期任务。
func (db *DB) ClaimTask(nowSec int64, leaseSec int64) (*Task, error) {
	row := db.QueryRow(`UPDATE tasks SET state='running', lease_until=?, attempts=attempts+1, updated_at=?
		WHERE id=(SELECT candidate.id FROM tasks candidate
		WHERE candidate.state='pending' AND candidate.cancel_requested=0 AND candidate.run_after<=?
		AND candidate.type NOT IN (`+idleTaskTypesSQL+`)
		AND candidate.type NOT IN ('operation_intake','system_schedule')
		AND NOT EXISTS (SELECT 1 FROM tasks active WHERE active.state='running'
		  AND candidate.file_id IS NOT NULL AND candidate.file_id!='' AND active.file_id=candidate.file_id)
		ORDER BY candidate.run_after,candidate.id LIMIT 1)
		RETURNING id,type,file_id,payload,state,attempts,max_attempts,last_error,result,
		run_after,lease_until,created_at,updated_at,done_at`,
		nowSec+leaseSec, nowSec, nowSec)
	return scanTaskRow(row)
}

// FinishTask 任务完成。
func (db *DB) FinishTask(id int64, result string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'done' END, result=?, last_error=NULL, done_at=?, updated_at=?, lease_until=NULL WHERE id=?`,
		result, now(), now(), id)
	if err != nil {
		return err
	}
	if err = snapshotTaskOutputs(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// EnqueuePurgeTask takes ownership of ordinary file work in one transaction.
// Running leases remain until workers stop; purge is serialized behind them.
func (db *DB) EnqueuePurgeTask(fileID string, operationIDs ...int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := enqueuePurgeTask(tx, fileID, operationIDs...)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func enqueuePurgeTask(tx *sql.Tx, fileID string, operationIDs ...int64) (int64, error) {
	var moving bool
	if err := tx.QueryRow(telegramMoveActiveSQL, fileID, fileID).Scan(&moving); err != nil {
		return 0, err
	}
	if moving {
		return 0, ErrTelegramMoving
	}
	var conversionCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?)`, fileID, fileID).Scan(&conversionCount); err != nil {
		return 0, err
	}
	if conversionCount != 0 {
		return 0, ErrConverting
	}
	if _, err := tx.Exec(`UPDATE tasks SET cancel_requested=1,state=CASE WHEN state='running' THEN state ELSE 'canceled' END,updated_at=unixepoch()
 WHERE file_id=? AND type!=? AND state IN('pending','running','failed')`, fileID, TaskPurgeFile); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM tasks WHERE file_id=? AND type=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, fileID, TaskPurgeFile).Scan(&id)
	if err == nil {
		if len(operationIDs) > 0 && operationIDs[0] != 0 {
			var existing int64
			if e := tx.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, id).Scan(&existing); e != nil || existing != operationIDs[0] {
				return 0, ErrTaskOperationConflict
			}
			if e := attachTaskOperation(tx, id, TaskPurgeFile, fileID, "{}", operationIDs...); e != nil {
				return 0, e
			}
		}
		if _, err = tx.Exec(`UPDATE tasks SET run_after=?,attempts=0,updated_at=? WHERE id=? AND state='pending'`, now(), now(), id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`UPDATE files SET purge_state='pending',updated_at=? WHERE id=? AND purge_state='failed'`, now(), fileID); err != nil {
			return 0, err
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE files SET purge_state='pending', updated_at=? WHERE id=? AND hidden=0`, now(), fileID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNotFound
	}
	ts := now()
	res, err = tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,'{}','pending',?,?,?)`, TaskPurgeFile, fileID, ts, ts, ts)
	if err != nil {
		return 0, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachTaskOperation(tx, id, TaskPurgeFile, fileID, "{}", operationIDs...); err != nil {
		return 0, err
	}
	return id, nil
}

// RetryFileArchive preserves import payloads and direct/encrypted semantics.
func (db *DB) RetryFileArchive(fileID string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	f, err := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, fileID))
	if err != nil {
		return 0, err
	}
	if f.PurgeState.String != "" {
		return 0, ErrDeleting
	}
	var id int64
	var typ, payload, state string
	err = tx.QueryRow(`SELECT id,type,payload,state FROM tasks WHERE file_id=? AND type IN (?,?,?) ORDER BY (type=?) DESC,id DESC LIMIT 1`, fileID, TaskArchive, TaskDirectArchive, TaskImport, TaskImport).Scan(&id, &typ, &payload, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if state == "running" {
		return id, tx.Commit()
	}
	if typ != TaskImport {
		typ, payload = TaskArchive, "{}"
		if f.Storage == "direct" {
			typ = TaskDirectArchive
		}
	}
	if id != 0 {
		_, err = tx.Exec(`UPDATE tasks SET type=?,payload=?,state='pending',cancel_requested=0,attempts=0,last_error=NULL,result=NULL,run_after=?,updated_at=?,lease_until=NULL WHERE id=?`, typ, payload, now(), now(), id)
	} else {
		var res sql.Result
		res, err = tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, typ, fileID, payload, now(), now(), now())
		if err == nil {
			id, err = res.LastInsertId()
		}
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE files SET archive_state='pending',archive_error='',updated_at=? WHERE id=?`, now(), fileID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FailTask 任务失败：决定重试或终态失败。
func (db *DB) FailTask(id int64, errStr string) error {
	t := &Task{}
	row := db.QueryRow(`SELECT attempts, max_attempts FROM tasks WHERE id=?`, id)
	if err := row.Scan(&t.Attempts, &t.MaxAttempts); err != nil {
		return err
	}
	if t.Attempts < t.MaxAttempts {
		// 指数退避：2^attempts 秒，上限 1 小时
		backoff := int64(1) << uint(min(t.Attempts, 12))
		if backoff > 3600 {
			backoff = 3600
		}
		_, err := db.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'pending' END, last_error=?, run_after=?, lease_until=NULL, updated_at=? WHERE id=?`,
			errStr, now()+backoff, now(), id)
		return err
	}
	_, err := db.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'failed' END, last_error=?, updated_at=?, lease_until=NULL WHERE id=?`,
		errStr, now(), id)
	return err
}

// ResetRunningTasks 启动时把滞留 running 的任务复位（进程崩溃恢复）。
func (db *DB) ResetRunningTasks() (int64, error) {
	res, err := db.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'pending' END, lease_until=NULL, run_after=?, updated_at=? WHERE state='running' AND type!='system_schedule'`, now(), now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListTasks 任务列表。
func (db *DB) ListTasks(state string, limit int) ([]*Task, error) {
	return db.ListFileTasks("", state, limit)
}
func (db *DB) ListFileTasks(fileID, state string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id,type,file_id,payload,state,attempts,max_attempts,last_error,result,
		run_after,lease_until,created_at,updated_at,done_at FROM tasks WHERE 1=1`
	args := []any{}
	if state != "" {
		q += ` AND state=?`
		args = append(args, state)
	}
	if fileID != "" {
		q += ` AND file_id=?`
		args = append(args, fileID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t := &Task{}
		var fileID sql.NullString
		if err := rows.Scan(&t.ID, &t.Type, &fileID, &t.Payload, &t.State, &t.Attempts, &t.MaxAttempts,
			&t.LastError, &t.Result, &t.RunAfter, &t.LeaseUntil, &t.CreatedAt, &t.UpdatedAt, &t.DoneAt); err != nil {
			return nil, err
		}
		t.FileID = fileID
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask 按 ID。
func (db *DB) GetTask(id int64) (*Task, error) {
	row := db.QueryRow(`SELECT id,type,file_id,payload,state,attempts,max_attempts,last_error,result,
		run_after,lease_until,created_at,updated_at,done_at FROM tasks WHERE id=?`, id)
	return scanTaskRow(row)
}

// RetryTask 把失败任务重新入队。
func (db *DB) RetryTask(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = retryTaskInTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TaskPayloadJSON 便捷取 payload。
func TaskPayloadJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = time.Now
