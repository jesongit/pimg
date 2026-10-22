package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const taskOperationSchema = `CREATE TABLE task_operations(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,scope TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX task_operation_scope ON task_operations(kind,scope,id);
CREATE TABLE task_operation_jobs(task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,operation_id INTEGER NOT NULL REFERENCES task_operations(id));
CREATE INDEX task_operation_members ON task_operation_jobs(operation_id,task_id);
INSERT INTO task_operations(kind,scope,created_at) SELECT 'sync','archives',MIN(created_at) FROM tasks WHERE type='archive_sync' AND state IN ('pending','running','failed') HAVING COUNT(*)>0;
INSERT INTO task_operation_jobs(task_id,operation_id) SELECT t.id,o.id FROM tasks t JOIN task_operations o ON o.kind='sync' AND o.scope='archives' WHERE t.type='archive_sync' AND t.state IN ('pending','running','failed');
INSERT INTO task_operations(kind,scope,created_at) SELECT 'archive','owner:'||f.owner_id,MIN(t.created_at) FROM tasks t JOIN files f ON f.id=t.file_id WHERE t.type IN ('archive','direct_archive','import') AND t.state IN ('pending','running') GROUP BY f.owner_id;
INSERT INTO task_operation_jobs(task_id,operation_id) SELECT t.id,o.id FROM tasks t JOIN files f ON f.id=t.file_id JOIN task_operations o ON o.kind='archive' AND o.scope='owner:'||f.owner_id WHERE t.type IN ('archive','direct_archive','import') AND t.state IN ('pending','running');`

// Attach a queue wave to one durable progress/cancellation boundary. Different
// repositories remain independent. A canceled wave never absorbs new work.
func attachTaskOperation(tx *sql.Tx, id int64, typ, fileID, payload string, operationIDs ...int64) error {
	if typ == TaskArchiveSync || typ == TaskColdSync {
		handled, err := attachIntakeSyncOperation(tx, id, typ, fileID, payload)
		if handled || err != nil {
			return err
		}
	}
	if len(operationIDs) > 0 && operationIDs[0] != 0 {
		return AttachTaskOperationItem(tx, id, typ, fileID, payload, operationIDs[0], fileID)
	}
	kind, scope := "sync", "archives"
	switch typ {
	case TaskArchive, TaskDirectArchive, TaskImport:
		var owner string
		err := tx.QueryRow(`SELECT owner_id FROM files WHERE id=?`, fileID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		kind, scope = "archive", "owner:"+owner
	case TaskArchiveSync, TaskColdSync:
	default:
		return nil
	}
	if typ == TaskColdSync {
		var p struct {
			RepositoryID int64 `json:"repository_id"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}
		scope = "repository:" + strconv.FormatInt(p.RepositoryID, 10)
	}
	var operation int64
	err := tx.QueryRow(`SELECT o.id FROM task_operations o WHERE kind=? AND scope=?
 AND EXISTS(SELECT 1 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=o.id AND t.state IN ('pending','running') AND t.cancel_requested=0)
 AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=o.id AND t.cancel_requested=1)
 ORDER BY o.id DESC LIMIT 1`, kind, scope).Scan(&operation)
	if errors.Is(err, sql.ErrNoRows) {
		r, e := tx.Exec(`INSERT INTO task_operations(kind,scope,created_at) VALUES(?,?,unixepoch())`, kind, scope)
		if e != nil {
			return e
		}
		operation, err = r.LastInsertId()
	}
	if err != nil {
		return err
	}
	if err = snapshotOperationContext(tx, operation, fileID); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id) VALUES(?,?) ON CONFLICT(task_id) DO NOTHING`, id, operation)
	return err
}

const TaskOperationIntake = "operation_intake"
const TaskOperationIntakeTTL = int64(6 * 60 * 60)

var ErrTaskOperationClosed = errors.New("这次操作已经停止或结束接收，请重新提交")
var ErrTaskOperationConflict = errors.New("操作标识已经用于其他请求，请刷新后重新提交")

// Existing archive/sync waves stay intact. New user operations have an explicit
// input boundary, independent of worker speed, HTTP chunks and concurrent clicks.
const explicitTaskOperationSchema = `
ALTER TABLE task_operations ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE task_operations ADD COLUMN request_key TEXT NOT NULL DEFAULT '';
ALTER TABLE task_operations ADD COLUMN input_state TEXT NOT NULL DEFAULT 'sealed' CHECK(input_state IN ('open','sealed','canceled','expired'));
ALTER TABLE task_operations ADD COLUMN expected_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_operations ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_operations ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_operations ADD COLUMN anchor_task_id INTEGER;
ALTER TABLE task_operation_jobs ADD COLUMN item_key TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX task_operation_request ON task_operations(owner_id,request_key) WHERE request_key!='';
CREATE INDEX task_operation_expiry ON task_operations(input_state,expires_at);
CREATE TABLE task_operation_items(
 operation_id INTEGER NOT NULL REFERENCES task_operations(id) ON DELETE CASCADE,
 item_key TEXT NOT NULL,
 file_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','done','skipped','failed','canceled')),
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(operation_id,item_key)
);
CREATE INDEX task_operation_item_file ON task_operation_items(operation_id,file_id);
CREATE INDEX task_operation_item_jobs ON task_operation_jobs(operation_id,item_key,task_id);
UPDATE task_operations SET updated_at=created_at;
`

type TaskOperation struct {
	ID            int64  `json:"id"`
	TaskID        int64  `json:"task_id"`
	OwnerID       string `json:"owner_id"`
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Scope         string `json:"scope"`
	InputState    string `json:"input_state"`
	ExpectedFiles int    `json:"expected_files"`
	ExpiresAt     int64  `json:"expires_at"`
}

const taskOperationColumns = `id,COALESCE(anchor_task_id,0),owner_id,request_key,kind,scope,input_state,expected_files,expires_at`

func scanTaskOperation(row *sql.Row) (*TaskOperation, error) {
	o := &TaskOperation{}
	err := row.Scan(&o.ID, &o.TaskID, &o.OwnerID, &o.Key, &o.Kind, &o.Scope, &o.InputState, &o.ExpectedFiles, &o.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

func (db *DB) TaskOperationByID(id int64, owner string) (*TaskOperation, error) {
	return scanTaskOperation(db.QueryRow(`SELECT `+taskOperationColumns+` FROM task_operations WHERE id=? AND owner_id=? AND request_key!=''`, id, owner))
}

// Begin is idempotent for exactly the same caller request. Reusing a key for a
// different kind/scope/size is rejected, including administrator operations.
func (db *DB) BeginTaskOperation(owner, kind, key, scope string, total int) (*TaskOperation, error) {
	if owner == "" || len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key || len(scope) > 256 || total < 1 || total > 1000000 {
		return nil, errors.New("操作标识或文件数量无效")
	}
	switch kind {
	case "upload", "purge", "publish", "unpublish", "move_album", "set_media_time", "convert", "show_home", "hide_home":
	default:
		return nil, errors.New("操作类型无效")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return nil, err
	}
	o, err := scanTaskOperation(tx.QueryRow(`SELECT `+taskOperationColumns+` FROM task_operations WHERE owner_id=? AND request_key=?`, owner, key))
	if err == nil {
		if o.Kind != kind || o.Scope != scope || o.ExpectedFiles != total {
			return nil, ErrTaskOperationConflict
		}
		if o.InputState != "open" || o.ExpiresAt <= now() {
			return nil, ErrTaskOperationClosed
		}
		if _, err = tx.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=?`, now(), now()+TaskOperationIntakeTTL, o.ID); err != nil {
			return nil, err
		}
		return o, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	ts := now()
	r, err := tx.Exec(`INSERT INTO task_operations(kind,scope,owner_id,request_key,input_state,expected_files,created_at,updated_at,expires_at) VALUES(?,?,?,?,'open',?,?,?,?)`, kind, scope, owner, key, total, ts, ts, ts+TaskOperationIntakeTTL)
	if err != nil {
		return nil, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err = snapshotOperationContext(tx, id, ""); err != nil {
		return nil, err
	}
	// A reserved, never-due worker row gives every operation a stable positive
	// task id even before its first file arrives. It is not an executable job.
	r, err = tx.Exec(`INSERT INTO tasks(type,payload,state,run_after,created_at,updated_at) VALUES(?,?,'pending',253402300799,?,?)`, TaskOperationIntake, TaskPayloadJSON(map[string]any{"operation_id": id}), ts, ts)
	if err != nil {
		return nil, err
	}
	taskID, err := r.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE task_operations SET anchor_task_id=? WHERE id=?`, taskID, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id) VALUES(?,?)`, taskID, id); err != nil {
		return nil, err
	}
	o, err = scanTaskOperation(tx.QueryRow(`SELECT `+taskOperationColumns+` FROM task_operations WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return o, tx.Commit()
}

func (db *DB) TouchTaskOperation(id int64, owner string) error {
	r, err := db.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=? AND owner_id=? AND input_state='open' AND expires_at>?`, now(), now()+TaskOperationIntakeTTL, id, owner, now())
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = ErrTaskOperationClosed
	}
	return err
}

func (db *DB) SealTaskOperation(id int64, owner string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE task_operations SET input_state='sealed',updated_at=? WHERE id=? AND owner_id=? AND input_state IN ('open','sealed')`, now(), id, owner)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTaskOperationClosed
	}
	if _, err = tx.Exec(`UPDATE task_operation_items SET state='failed',error='没有收到文件处理任务，请重新提交',updated_at=? WHERE operation_id=? AND state='queued'
 AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.operation_id=task_operation_items.operation_id AND j.item_key=task_operation_items.item_key)`, now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET
 state=CASE WHEN (SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?)<(SELECT expected_files FROM task_operations WHERE id=?) THEN 'failed' ELSE 'done' END,
 last_error=CASE WHEN (SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?)<(SELECT expected_files FROM task_operations WHERE id=?) THEN ((SELECT expected_files FROM task_operations WHERE id=?)-(SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?))||' 项文件未提交，请重新选择这些文件；已接收的文件会继续处理' END,
 done_at=?,updated_at=?,lease_until=NULL WHERE id=(SELECT anchor_task_id FROM task_operations WHERE id=?)`, id, id, id, id, id, id, now(), now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Expiration ends intake only. Work already accepted keeps its own leases,
// retry/cancellation state and continues after a browser disconnect or restart.
func (db *DB) ExpireTaskOperations(at int64) error {
	var due bool
	if err := db.ReadQueryRow(`SELECT EXISTS(SELECT 1 FROM task_operations WHERE input_state='open' AND expires_at<=?)`, at).Scan(&due); err != nil {
		return err
	}
	if !due {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = expireTaskOperations(tx, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) expireDueTaskOperations() error {
	var due bool
	if err := db.ReadQueryRow(`SELECT EXISTS(SELECT 1 FROM task_operations WHERE input_state='open' AND expires_at<=?)`, now()).Scan(&due); err != nil {
		return err
	}
	if due {
		return db.ExpireTaskOperations(now())
	}
	return nil
}

func expireTaskOperations(tx *sql.Tx, at int64) error {
	if _, err := tx.Exec(`UPDATE task_operation_items SET state='failed',error='文件接收已中断，请重新提交',updated_at=? WHERE state='queued' AND operation_id IN(SELECT id FROM task_operations WHERE input_state='open' AND expires_at<=?)
 AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.operation_id=task_operation_items.operation_id AND j.item_key=task_operation_items.item_key)`, at, at); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET state=CASE WHEN EXISTS(SELECT 1 FROM task_operations o WHERE o.anchor_task_id=tasks.id AND o.expected_files>(SELECT COUNT(*) FROM task_operation_items i WHERE i.operation_id=o.id)) THEN 'failed' ELSE 'done' END,
 last_error=CASE WHEN EXISTS(SELECT 1 FROM task_operations o WHERE o.anchor_task_id=tasks.id AND o.expected_files>(SELECT COUNT(*) FROM task_operation_items i WHERE i.operation_id=o.id)) THEN '文件接收已中断，已接收的文件会继续处理；未接收的文件请重新提交' ELSE NULL END,updated_at=?,done_at=?,lease_until=NULL
 WHERE id IN(SELECT anchor_task_id FROM task_operations WHERE input_state='open' AND expires_at<=?)`, at, at, at); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE task_operations SET input_state=CASE WHEN expected_files>(SELECT COUNT(*) FROM task_operation_items i WHERE i.operation_id=task_operations.id) THEN 'expired' ELSE 'sealed' END,updated_at=? WHERE input_state='open' AND expires_at<=?`, at, at)
	return err
}

func recordTaskOperationItem(tx *sql.Tx, operationID int64, itemKey, fileID, name, state, message string) error {
	if itemKey == "" || len(itemKey) > 256 || len(name) > 4096 || len(message) > 8192 {
		return errors.New("操作文件信息无效")
	}
	switch state {
	case "queued", "done", "skipped", "failed", "canceled":
	default:
		return errors.New("操作文件状态无效")
	}
	if fileID != "" {
		var existing string
		err := tx.QueryRow(`SELECT file_id FROM task_operation_items WHERE operation_id=? AND item_key=?`, operationID, itemKey).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing != "" && existing != fileID {
			return ErrTaskOperationConflict
		}
	}
	var overflow bool
	if err := tx.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM task_operation_items WHERE operation_id=? AND item_key=?) AND (SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?)>=(SELECT expected_files FROM task_operations WHERE id=?)`, operationID, itemKey, operationID, operationID).Scan(&overflow); err != nil {
		return err
	}
	if overflow {
		return errors.New("这次操作接收的文件已达到提交数量")
	}
	var recorded bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_operation_items WHERE operation_id=? AND item_key=?)`, operationID, itemKey).Scan(&recorded); err != nil {
		return err
	}
	if !recorded {
		if err := snapshotOperationContext(tx, operationID, fileID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO task_operation_items(operation_id,item_key,file_id,name,state,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(operation_id,item_key) DO UPDATE SET file_id=CASE WHEN task_operation_items.file_id='' THEN excluded.file_id ELSE task_operation_items.file_id END,
 name=CASE WHEN task_operation_items.name='' THEN excluded.name ELSE task_operation_items.name END,
 state=CASE WHEN excluded.state='queued' AND task_operation_items.state IN ('done','skipped') THEN task_operation_items.state ELSE excluded.state END,
 error=CASE WHEN excluded.state='queued' AND task_operation_items.state IN ('done','skipped') THEN task_operation_items.error ELSE excluded.error END,updated_at=excluded.updated_at`, operationID, itemKey, fileID, name, state, message, now(), now())
	return err
}

func (db *DB) RecordTaskOperationItem(id int64, owner, itemKey, fileID, name, state, message string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var input string
	var expiry int64
	if err = tx.QueryRow(`SELECT input_state,expires_at FROM task_operations WHERE id=? AND owner_id=? AND request_key!=''`, id, owner).Scan(&input, &expiry); err != nil {
		return err
	}
	if input != "open" || expiry <= now() {
		return ErrTaskOperationClosed
	}
	if fileID != "" && owner != AdminOwner {
		var authorized bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM files WHERE id=? AND owner_id=?) OR EXISTS(SELECT 1 FROM task_operation_items WHERE operation_id=? AND item_key=? AND file_id=?)`, fileID, owner, id, itemKey, fileID).Scan(&authorized); err != nil {
			return err
		}
		if !authorized {
			return ErrNotFound
		}
	}
	if err = recordTaskOperationItem(tx, id, itemKey, fileID, name, state, message); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=?`, now(), now()+TaskOperationIntakeTTL, id); err != nil {
		return err
	}
	return tx.Commit()
}

// This hook belongs inside the enqueue transaction. Closing/canceling intake and
// accepting another chunk cannot interleave and escape the cancellation boundary.
func AttachTaskOperationItem(tx *sql.Tx, taskID int64, typ, fileID, payload string, operationID int64, itemKey string) error {
	var owner, input string
	var expiry int64
	if err := tx.QueryRow(`SELECT owner_id,input_state,expires_at FROM task_operations WHERE id=? AND request_key!=''`, operationID).Scan(&owner, &input, &expiry); err != nil {
		return err
	}
	if input != "open" || expiry <= now() {
		return ErrTaskOperationClosed
	}
	var name, fileOwner string
	if fileID != "" {
		err := tx.QueryRow(`SELECT COALESCE(NULLIF(orig_name,''),name),owner_id FROM files WHERE id=?`, fileID).Scan(&name, &fileOwner)
		if err != nil {
			return err
		}
		if owner != AdminOwner && owner != fileOwner {
			return ErrNotFound
		}
	}
	if itemKey == "" {
		itemKey = fileID
	}
	var existing int64
	err := tx.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, taskID).Scan(&existing)
	if err == nil && existing != operationID {
		return ErrTaskOperationConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = recordTaskOperationItem(tx, operationID, itemKey, fileID, name, "queued", ""); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?) ON CONFLICT(task_id) DO NOTHING`, taskID, operationID, itemKey); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=?`, now(), now()+TaskOperationIntakeTTL, operationID)
	return err
}

// Retry never reopens intake or replays successful items. The caller retries
// individual worker jobs through RetryTask, preserving each business guard.
func (db *DB) RetryTaskOperation(id int64, owner string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var anchor int64
	var input string
	if err = tx.QueryRow(`SELECT anchor_task_id,input_state FROM task_operations WHERE id=? AND owner_id=? AND request_key!=''`, id, owner).Scan(&anchor, &input); err != nil {
		return err
	}
	if input == "open" {
		return fmt.Errorf("操作仍在接收文件，请结束接收后重试")
	}
	if _, err = tx.Exec(`UPDATE task_operations SET input_state='sealed',updated_at=? WHERE id=?`, now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET state=CASE WHEN (SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?)<(SELECT expected_files FROM task_operations WHERE id=?) THEN 'failed' ELSE 'done' END,
 cancel_requested=0,last_error=CASE WHEN (SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?)<(SELECT expected_files FROM task_operations WHERE id=?) THEN ((SELECT expected_files FROM task_operations WHERE id=?)-(SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?))||' 项文件未提交，请重新选择这些文件' END,done_at=?,updated_at=?,lease_until=NULL WHERE id=?`, id, id, id, id, id, id, now(), now(), anchor); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE task_operation_items SET state='queued',error='',updated_at=? WHERE operation_id=? AND state IN ('failed','canceled') AND EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.operation_id=task_operation_items.operation_id AND j.item_key=task_operation_items.item_key)`, now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Per-file state is derived from the newest worker attempt, never from the
// arrival order of HTTP responses. Retained names survive successful deletion.
const operationItemStatesCTE = `operation_job_attempts AS MATERIALIZED (
 SELECT j.operation_id,j.item_key,t.state,t.last_error,t.updated_at,
 ROW_NUMBER() OVER(PARTITION BY j.operation_id,j.item_key,t.type,COALESCE(t.file_id,''),t.payload ORDER BY t.id DESC) relevance
 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.item_key!=''),
 operation_item_jobs AS MATERIALIZED (
 SELECT operation_id,item_key,COUNT(*) jobs,SUM(state='running') running,SUM(state='pending') pending,
 SUM(state='failed') failed,SUM(state='canceled') canceled,
 MAX(CASE WHEN state='failed' THEN last_error END) error,MAX(updated_at) updated_at
 FROM operation_job_attempts WHERE relevance=1 GROUP BY operation_id,item_key),
 operation_item_states AS MATERIALIZED (
 SELECT i.operation_id,i.item_key,i.file_id,i.name,
 CASE WHEN j.running>0 THEN 'running' WHEN j.pending>0 THEN 'pending'
 WHEN j.failed>0 THEN 'failed' WHEN j.canceled>0 THEN 'canceled' WHEN j.jobs>0 THEN 'done' ELSE i.state END state,
 COALESCE(NULLIF(j.error,''),i.error,'') error,MAX(i.updated_at,COALESCE(j.updated_at,0)) updated_at
 FROM task_operation_items i LEFT JOIN operation_item_jobs j ON j.operation_id=i.operation_id AND j.item_key=i.item_key)`

type TaskOperationItem struct {
	FileSuccessor *FileSuccessor `json:"file_successor,omitempty"`
	OwnerID       string         `json:"owner_id,omitempty"`
	Key           string         `json:"key"`
	FileID        string         `json:"file_id"`
	FileName      string         `json:"file_name"`
	State         string         `json:"state"`
	Error         string         `json:"error"`
	FileExists    bool           `json:"file_exists"`
}

func (db *DB) GetTaskOperation(id int64, owner string) (*ActivityTask, error) {
	o, err := db.TaskOperationByID(id, owner)
	if err != nil {
		return nil, err
	}
	return db.GetTaskGroup(o.TaskID)
}

func (db *DB) TaskOperationItems(id int64, owner, state string, page, limit int) ([]TaskOperationItem, int, error) {
	if state == "all" {
		state = ""
	}
	if _, err := db.TaskOperationByID(id, owner); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if state != "" && state != "queued" && state != "pending" && state != "running" && state != "done" && state != "skipped" && state != "failed" && state != "canceled" {
		return nil, 0, errors.New("文件处理状态无效")
	}
	where := ` WHERE i.operation_id=? AND (?='' OR i.state=?)`
	args := []any{id, state, state}
	var total int
	if err := db.QueryRow(`WITH `+operationItemStatesCTE+` SELECT COUNT(*) FROM operation_item_states i`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := db.Query(`WITH `+operationItemStatesCTE+` SELECT i.item_key,i.file_id,i.name,i.state,i.error,EXISTS(SELECT 1 FROM files f WHERE f.id=i.file_id AND f.hidden=0),COALESCE((SELECT owner_id FROM files WHERE id=i.file_id),(SELECT owner_id FROM task_operations WHERE id=i.operation_id),'') FROM operation_item_states i`+where+` ORDER BY (i.state='failed') DESC,i.item_key LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []TaskOperationItem{}
	for rows.Next() {
		var item TaskOperationItem
		if err = rows.Scan(&item.Key, &item.FileID, &item.FileName, &item.State, &item.Error, &item.FileExists, &item.OwnerID); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func taskCategorySQL(category string) string {
	switch category {
	case "intake":
		return "'upload_group','local_import_batch','local_import_scan','local_import','local_import_idle','import'"
	case "archival":
		return "'archive_group','sync_group','convert_group','archive','direct_archive','archive_sync','cold_sync','convert','verify'"
	case "publication":
		return "'publish_group','unpublish_group','publish','unpublish','temp_materialize'"
	case "cleanup":
		return "'purge_group','purge_file','cleanup_temp','cleanup_converted','cold_delete','account_delete'"
	case "telegram":
		return "'move_album_group','telegram_receive_group','tg_scan','tg_receive','tg_delete_album','tg_move_file','transfer_owner','tg_rename_album','tg_create_album','account_space','account_group_title'"
	case "maintenance":
		return "'maintenance_group','local_layout','storage_probe'"
	}
	return ""
}
