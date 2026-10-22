package dbx

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// History is additive. Keep intake receipts after file deletion, without copying
// file contents, locations, credentials or keys into the audit trail.
const activitySchema = `
CREATE INDEX idx_ingest_source_latest ON ingest_events(source_key,id DESC);
CREATE INDEX idx_tasks_state_id ON tasks(state,id);
UPDATE ingest_events AS e SET body=json_set(e.body,
 '$.name',COALESCE(json_extract(e.body,'$.name'),f.name),
 '$.orig_name',COALESCE(json_extract(e.body,'$.orig_name'),f.orig_name),
 '$.album_name',COALESCE(json_extract(e.body,'$.album_name'),(SELECT name FROM albums WHERE id=f.album_id),''))
 FROM files f WHERE f.id=e.file_id;
INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at)
 SELECT 'existing:'||f.id,'accepted','升级前的文件，早期接收过程未记录',f.id,'','',
 json_object('source','existing','name',f.name,'orig_name',f.orig_name,'retained_album_id',COALESCE(f.album_id,0),'album_name',COALESCE(a.name,'')),f.created_at
 FROM files f LEFT JOIN albums a ON a.id=f.album_id
 WHERE f.hidden=0 AND NOT EXISTS(SELECT 1 FROM ingest_events e WHERE e.file_id=f.id AND e.source_key NOT LIKE 'verify:%');
`

type TaskCounts struct {
	Running int `json:"running"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

func (db *DB) ActivityTaskCounts(fileID string) (TaskCounts, error) {
	var c TaskCounts
	if fileID == "" {
		if err := db.expireDueTaskOperations(); err != nil {
			return c, err
		}
		// A navigation badge needs state only. Avoid building every historical
		// operation's JSON, file counts, errors and concatenated search contents.
		err := db.ReadQueryRow(taskMembersCTE+`, candidate_groups AS MATERIALIZED (
 SELECT DISTINCT m.group_key FROM task_members m
 LEFT JOIN task_operations o ON o.id=m.group_operation_id LEFT JOIN import_bounds b ON b.id=m.batch_id
 WHERE m.state IN ('pending','running') OR o.input_state='open' OR b.state IN ('paused','running','scanning')),
 active_members AS MATERIALIZED (
 SELECT m.*,ROW_NUMBER() OVER(PARTITION BY group_key,type,COALESCE(file_id,''),COALESCE(payload,'') ORDER BY id DESC) relevance
 FROM task_members m WHERE group_key IN (SELECT group_key FROM candidate_groups)), active_groups AS (
 SELECT m.group_key,
 MAX(m.relevance=1 AND m.type!='operation_intake' AND m.state='running') running,
 MAX((m.relevance=1 AND m.type!='operation_intake' AND m.state='pending') OR COALESCE(o.input_state='open',0) OR COALESCE(b.state IN ('paused','running','scanning'),0)) pending
 FROM active_members m LEFT JOIN import_bounds b ON b.id=m.batch_id
 LEFT JOIN task_operations o ON o.id=m.group_operation_id
 GROUP BY m.group_key)
 SELECT COALESCE(SUM(running),0),COALESCE(SUM(running=0 AND pending=1),0) FROM active_groups`).Scan(&c.Running, &c.Pending)
		return c, err
	}
	err := db.ReadQueryRow(`SELECT COALESCE(SUM(state='running'),0),COALESCE(SUM(state='pending'),0),COALESCE(SUM(state='failed'),0) FROM tasks WHERE state IN ('running','pending','failed') AND (?='' OR file_id=?)`, fileID, fileID).Scan(&c.Running, &c.Pending, &c.Failed)
	return c, err
}

type ActivityTask struct {
	*Task
	FileName, AlbumName string
	OwnerID             string
	AlbumID             int64
	CancelRequested     bool
	FileExists          bool
	Group               *TaskGroup
}

// Active work is filtered before pagination; a large completed history cannot
// hide old queued jobs. The legacy task endpoint can still request all states.
func (db *DB) ActivityTasks(fileID, view, state string, page, limit int) ([]ActivityTask, int, error) {
	return db.QueryActivityTasks(LogQuery{FileID: fileID, View: view, State: state, Page: page, Limit: limit})
}

func (db *DB) QueryActivityTasks(q LogQuery) ([]ActivityTask, int, error) {
	fileID, view, state, page, limit := q.FileID, q.View, q.State, q.Page, q.Limit
	where := ` WHERE (?='' OR t.file_id=?)`
	prefix, table, extra := "", "tasks", "''"
	retainedName, retainedOwner, searchMembers := "''", "''", "''"
	if q.Grouped {
		if err := db.expireDueTaskOperations(); err != nil {
			return nil, 0, err
		}
		prefix, table, extra = taskReadCTE(taskGroupsCTE, q.Search != "", q.State == "active" || q.View == "current"), "grouped_tasks", "COALESCE(t.group_json,'')"
		retainedName, retainedOwner, searchMembers = "NULLIF(t.retained_name,'')", "NULLIF(t.retained_owner,'')", "t.search_text"
	} else {
		retainedName = `(SELECT NULLIF(i.name,'') FROM task_operation_jobs j JOIN task_operation_items i ON i.operation_id=j.operation_id AND i.item_key=j.item_key WHERE j.task_id=t.id)`
		retainedOwner = `(SELECT NULLIF(o.owner_id,'') FROM task_operation_jobs j JOIN task_operations o ON o.id=j.operation_id WHERE j.task_id=t.id)`
	}
	args := []any{fileID, fileID}
	if q.GroupID > 0 {
		where += ` AND t.id=?`
		args = append(args, q.GroupID)
	}
	switch view {
	case "current":
		where += ` AND t.state IN ('running','pending')`
	case "attention":
		where += ` AND t.state='failed'`
	case "history":
		where += ` AND t.state IN ('done','canceled')`
	case "receipt":
		// Keep the first archive and the latest task of each operation type.
		where += ` AND (t.id IN (SELECT MAX(id) FROM tasks WHERE file_id=? GROUP BY type) OR t.id=(SELECT MIN(id) FROM tasks WHERE file_id=? AND type IN ('archive','direct_archive','import')))`
		args = append(args, fileID, fileID)
	}
	switch state {
	case "active":
		where += ` AND t.state IN ('running','pending')`
	case "finished":
		where += ` AND t.state IN ('done','canceled')`
	case "terminal":
		where += ` AND t.state IN ('done','canceled','failed')`
	case "retrying":
		where += ` AND t.state='pending' AND t.attempts>0`
	case "stopping":
		where += ` AND t.state IN ('running','pending') AND t.cancel_requested=1`
	default:
		if state != "" {
			where += ` AND t.state=?`
			args = append(args, state)
		}
	}
	if q.Type != "" {
		category := taskCategorySQL(q.Type)
		if category != "" {
			where += ` AND t.type IN (` + category + `)`
		} else {
			where += ` AND t.type=?`
			args = append(args, q.Type)
		}
	}
	if q.From > 0 {
		where += ` AND t.created_at>=?`
		args = append(args, q.From)
	}
	if q.Search != "" {
		where += ` AND instr(lower('#'||CAST(t.id AS TEXT)||' '||COALESCE(t.file_id,'')||' '||COALESCE(NULLIF(f.orig_name,''),f.name,` + retainedName + `,'')||' '||COALESCE(a.name,'')||' '||COALESCE(t.last_error,'')||' '||` + searchMembers + `),lower(?))>0`
		args = append(args, q.Search)
	}
	joins := ` FROM ` + table + ` t LEFT JOIN files f ON f.id=t.file_id LEFT JOIN albums a ON a.id=COALESCE(f.album_id,CASE WHEN substr(t.file_id,1,9)='tg-album:' THEN CAST(substr(t.file_id,10) AS INTEGER) END)`
	var total int

	order := ` ORDER BY t.id DESC`
	if view == "current" && q.Sort == "" {
		order = ` ORDER BY CASE t.state WHEN 'running' THEN 0 ELSE 1 END,t.id`
	}
	if q.Sort != "" {
		order = ` ORDER BY ` + logOrder(q.Sort, "t.", true)
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
	pageArgs := append(append([]any{}, args...), limit, (page-1)*limit)
	rows, err := db.ReadQuery(prefix+`SELECT COUNT(*) OVER(),t.id,t.type,t.file_id,t.payload,t.state,t.attempts,t.max_attempts,t.last_error,t.result,t.run_after,t.lease_until,t.created_at,t.updated_at,t.done_at,t.cancel_requested,COALESCE(NULLIF(f.orig_name,''),f.name,`+retainedName+`,CASE WHEN t.type='account_delete' AND json_valid(t.payload) THEN json_extract(t.payload,'$.username') END,''),COALESCE(a.name,''),COALESCE(a.id,0),COALESCE(f.owner_id,a.owner_id,`+retainedOwner+`,''),f.id IS NOT NULL AND f.hidden=0,`+extra+`
`+joins+where+order+` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ActivityTask{}
	for rows.Next() {
		t := &Task{}
		a := ActivityTask{Task: t}
		var group string
		if err = rows.Scan(&total, &t.ID, &t.Type, &t.FileID, &t.Payload, &t.State, &t.Attempts, &t.MaxAttempts, &t.LastError, &t.Result, &t.RunAfter, &t.LeaseUntil, &t.CreatedAt, &t.UpdatedAt, &t.DoneAt, &a.CancelRequested, &a.FileName, &a.AlbumName, &a.AlbumID, &a.OwnerID, &a.FileExists, &group); err != nil {
			return nil, 0, err
		}
		if a.Group, err = decodeTaskGroup(group); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	if len(out) == 0 {
		if err := db.ReadQueryRow(prefix+`SELECT COUNT(*)`+joins+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

type IntakeReceipt struct {
	IngestEvent
	Name      string `json:"name"`
	AlbumName string `json:"album_name"`
	Outcome   string `json:"outcome"`
	Error     string `json:"error"`
	Exists    bool   `json:"exists"`
}

func (db *DB) IntakeReceipts(fileID, outcome string, before int64) ([]IntakeReceipt, error) {
	if before <= 0 {
		before = 1 << 62
	}
	// Select a whole source receipt, then apply the cursor. Applying the cursor
	// first would make older stages of that receipt reappear on later pages.
	rows, err := db.ReadQuery(`WITH latest AS (SELECT MAX(id) id FROM ingest_events WHERE source_key NOT LIKE 'verify:%' GROUP BY source_key), receipts AS (
 SELECT e.id,e.source_key,e.event_type,e.reason,e.file_id,e.tool_file_id,e.upload_id,e.body,e.created_at,
 COALESCE(json_extract(e.body,'$.orig_name'),NULLIF(f.orig_name,''),json_extract(e.body,'$.name'),f.name,'') name,
 COALESCE(a.name,json_extract(e.body,'$.album_name'),'') album_name,
 CASE WHEN e.event_type='failed' THEN 'failed' WHEN e.event_type='duplicate' THEN 'duplicate' WHEN e.event_type='retired_ignored' THEN 'ignored'
 WHEN f.id IS NULL THEN 'deleted' WHEN f.archive_state='failed' OR COALESCE(f.archive_error,'')!='' OR COALESCE(f.publish_error,'')!='' THEN 'failed'
 WHEN f.published=1 THEN 'published' WHEN f.archive_state='done' THEN 'archived' ELSE 'processing' END outcome,
 COALESCE(NULLIF(f.archive_error,''),NULLIF(f.publish_error,''),CASE WHEN e.event_type='failed' THEN e.reason END,'') error,
 f.id IS NOT NULL present
 FROM latest l JOIN ingest_events e ON e.id=l.id LEFT JOIN files f ON f.id=e.file_id LEFT JOIN albums a ON a.id=f.album_id)
 SELECT * FROM receipts WHERE id<? AND (?='' OR file_id=?) AND (?='' OR outcome=?) ORDER BY id DESC LIMIT 51`, before, fileID, fileID, outcome, outcome)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntakeReceipt{}
	for rows.Next() {
		var e IntakeReceipt
		var body string
		if err = rows.Scan(&e.ID, &e.SourceKey, &e.Type, &e.Reason, &e.FileID, &e.ToolFileID, &e.UploadID, &body, &e.CreatedAt, &e.Name, &e.AlbumName, &e.Outcome, &e.Error, &e.Exists); err != nil {
			return nil, err
		}
		e.Body = json.RawMessage(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) ReceiptEvents(id int64) ([]IngestEvent, error) {
	var source, fileID string
	if err := db.ReadQueryRow(`SELECT source_key,file_id FROM ingest_events WHERE id=?`, id).Scan(&source, &fileID); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := db.ReadQuery(`SELECT id,source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at FROM ingest_events WHERE source_key=? OR (?!='' AND source_key='verify:'||?) ORDER BY id LIMIT 100`, source, fileID, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngestEvent{}
	for rows.Next() {
		var e IngestEvent
		var b string
		if err = rows.Scan(&e.ID, &e.SourceKey, &e.Type, &e.Reason, &e.FileID, &e.ToolFileID, &e.UploadID, &b, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Body = json.RawMessage(b)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Keep a human-readable identity when the file is later purged. Enrichment
// runs in the intake transaction, never during polling or original downloads.
func enrichIntakeEvent(tx *sql.Tx, e *IngestEvent) error {
	var body map[string]any
	if json.Unmarshal(e.Body, &body) != nil || body == nil {
		body = map[string]any{}
	}
	if e.FileID != "" {
		var name, original, album string
		err := tx.QueryRow(`SELECT f.name,f.orig_name,COALESCE(a.name,'') FROM files f LEFT JOIN albums a ON a.id=f.album_id WHERE f.id=?`, e.FileID).Scan(&name, &original, &album)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if _, ok := body["name"]; !ok {
			body["name"] = name
		}
		if _, ok := body["orig_name"]; !ok {
			body["orig_name"] = original
		}
		if _, ok := body["album_name"]; !ok {
			body["album_name"] = album
		}
	}
	if v, _ := body["telegram_source"].(string); strings.TrimSpace(v) != "" {
		body["source"] = "telegram"
	}
	b, err := json.Marshal(body)
	e.Body = b
	return err
}
