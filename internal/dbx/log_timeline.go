package dbx

// A receipt belongs to its intake, rather than to a later unrelated publish or
// delete operation on the same file. Only genuinely unbound sources get a row.
const unifiedLogsCTE = taskGroupsCTE + `,` + logReceiptsBody + `,
 timeline_task_revisions AS MATERIALIZED (
 SELECT m.group_key,MAX(COALESCE(v.revision,0)) revision
 FROM task_members m LEFT JOIN log_row_revisions v ON v.record='task' AND v.record_id=m.id GROUP BY m.group_key),
 timeline_member_content AS MATERIALIZED (
 SELECT m.group_key,GROUP_CONCAT(COALESCE(NULLIF(f.orig_name,''),f.name,m.file_id,'')||' '||COALESCE(m.file_id,'')||' '||COALESCE(m.last_error,''),' ') search_text
 FROM task_members m LEFT JOIN files f ON f.id=m.file_id GROUP BY m.group_key),
 timeline_receipt_revisions AS MATERIALIZED (
 SELECT e.source_key,MAX(COALESCE(v.revision,0)) revision,MAX(e.created_at) event_at FROM ingest_events e
 LEFT JOIN log_row_revisions v ON v.record='receipt' AND v.record_id=e.id GROUP BY e.source_key),
 timeline_receipt_candidates AS MATERIALIZED (
 SELECT r.*,CASE
 WHEN CAST(COALESCE(json_extract(r.body,'$.operation_id'),0) AS INTEGER)>0 THEN 'operation:'||CAST(json_extract(r.body,'$.operation_id') AS INTEGER)
 WHEN json_extract(r.body,'$.telegram_source') LIKE 'local-import:%' THEN 'import:'||CAST(substr(json_extract(r.body,'$.telegram_source'),14) AS INTEGER)
 WHEN r.source_key LIKE 'local-import:%' THEN 'import:'||CAST(substr(r.source_key,14) AS INTEGER)
 ELSE COALESCE((SELECT m.group_key FROM task_members m WHERE m.file_id=r.file_id AND r.file_id!='' AND m.type IN ('archive','direct_archive','import') ORDER BY m.id LIMIT 1),
 (SELECT m.group_key FROM task_operation_items i JOIN task_operation_jobs j ON j.operation_id=i.operation_id JOIN task_members m ON m.id=j.task_id JOIN task_operations o ON o.id=j.operation_id
 WHERE i.file_id=r.file_id AND r.file_id!='' AND o.kind IN ('archive','upload','telegram_receive') ORDER BY m.id LIMIT 1),'') END candidate_key,
 COALESCE(v.revision,0) revision,COALESCE(v.event_at,r.created_at) event_at FROM receipts r LEFT JOIN timeline_receipt_revisions v ON v.source_key=r.source_key),
 timeline_receipt_bindings AS MATERIALIZED (
 SELECT r.*,CASE WHEN EXISTS(SELECT 1 FROM grouped_tasks g WHERE g.group_key=r.candidate_key) THEN r.candidate_key ELSE '' END bound_group
 FROM timeline_receipt_candidates r),
 timeline_intake_content AS MATERIALIZED (
 SELECT bound_group,MAX(revision) revision,MAX(event_at) updated_at,MAX(name) name,
 GROUP_CONCAT(name||' '||file_id||' '||tool_file_id||' '||upload_id||' '||reason||' '||error,' ') search_text
 FROM timeline_receipt_bindings WHERE bound_group!='' GROUP BY bound_group),
 timeline_rows AS (
 SELECT 'task' record,'task:'||g.id record_key,g.id,g.id task_id,g.type,g.state,COALESCE(g.file_id,'') file_id,
 COALESCE(NULLIF(f.orig_name,''),f.name,NULLIF(g.retained_name,''),CASE WHEN json_extract(g.group_json,'$.total_files')=1 THEN c.name END,
 CASE WHEN g.type='system_schedule' AND json_valid(g.payload) THEN json_extract(g.payload,'$.name') END,
 CASE WHEN g.type='account_delete' AND json_valid(g.payload) THEN json_extract(g.payload,'$.username') END,'') file_name,
 f.id IS NOT NULL AND f.hidden=0 file_exists,COALESCE(a.id,0) album_id,COALESCE(a.name,'') album_name,
 COALESCE(f.owner_id,a.owner_id,NULLIF(g.retained_owner,''),'') owner_id,g.last_error,g.result,g.attempts,g.max_attempts,g.run_after,g.cancel_requested,
 g.created_at,MAX(g.updated_at,COALESCE(c.updated_at,0)) updated_at,CASE WHEN g.state IN ('running','pending') THEN 0 ELSE COALESCE(g.done_at,0) END done_at,
 COALESCE(g.group_json,'') group_json,'' source,'' reason,MAX(v.revision,COALESCE(c.revision,0)) revision,
 g.state IN ('pending','running') OR COALESCE(json_extract(g.group_json,'$.stage'),'')='ready' is_active,
 g.search_text||' '||COALESCE(m.search_text,'')||' '||COALESCE(c.search_text,'')||' '||COALESCE(json_extract(g.group_json,'$.context'),'') search_text
 FROM grouped_tasks g JOIN timeline_task_revisions v ON v.group_key=g.group_key
 LEFT JOIN files f ON f.id=g.file_id LEFT JOIN albums a ON a.id=COALESCE(f.album_id,CASE WHEN substr(g.file_id,1,9)='tg-album:' THEN CAST(substr(g.file_id,10) AS INTEGER) END)
 LEFT JOIN timeline_member_content m ON m.group_key=g.group_key LEFT JOIN timeline_intake_content c ON c.bound_group=g.group_key
 UNION ALL
 SELECT 'receipt','receipt:'||r.id,r.id,0,r.event_type,r.outcome,r.file_id,r.name,r.present,0,r.album_name,r.owner_id,r.error,NULL,0,0,0,0,
 r.created_at,r.event_at,r.event_at,'',r.source,r.reason,r.revision,r.outcome='processing',
 r.name||' '||r.file_id||' '||r.tool_file_id||' '||r.upload_id||' '||r.reason||' '||r.error
 FROM timeline_receipt_bindings r WHERE r.bound_group=''),
 visible_logs AS (
 SELECT r.* FROM timeline_rows r JOIN log_visibility v ON v.id=1 WHERE r.is_active=1 OR r.revision>v.cleared_revision)
 `

type UnifiedLogEntry struct {
	Record    string
	RecordKey string
	Task      *ActivityTask
	Receipt   *LogReceipt
}

// Search, scope and pagination operate on the merged stream, preventing one
// operation from occupying both a task row and hundreds of receipt rows.
func (db *DB) QueryUnifiedLogs(q LogQuery) ([]UnifiedLogEntry, int, error) {
	if err := db.expireDueTaskOperations(); err != nil {
		return nil, 0, err
	}
	where := ` WHERE 1=1`
	base := taskReadCTE(unifiedLogsCTE, q.Search != "", false)
	if q.GroupID == 0 && q.FileID == "" {
		base = historyReadCTE(base)
	}
	args := []any{}
	if q.GroupID == 0 && q.FileID == "" {
		where += ` AND r.is_active=0`
	}
	if q.GroupID > 0 {
		where += ` AND r.task_id=?`
		args = append(args, q.GroupID)
	}
	if q.FileID != "" {
		where += ` AND (r.file_id=? OR (r.record='task' AND EXISTS(SELECT 1 FROM task_members m WHERE m.group_key=(SELECT group_key FROM grouped_tasks WHERE id=r.task_id) AND m.file_id=?))
 OR (r.record='task' AND EXISTS(SELECT 1 FROM task_operation_items i JOIN task_operation_jobs j ON j.operation_id=i.operation_id JOIN task_members m ON m.id=j.task_id WHERE i.file_id=? AND m.group_key=(SELECT group_key FROM grouped_tasks WHERE id=r.task_id))))`
		args = append(args, q.FileID, q.FileID, q.FileID)
	}
	if q.Search != "" {
		where += ` AND instr(lower(r.record_key||' '||r.type||' '||r.file_name||' '||r.file_id||' '||r.album_name||' '||r.last_error||' '||r.search_text),lower(?))>0`
		args = append(args, q.Search)
	}
	warning := `CASE WHEN r.type='system_schedule' AND json_valid(r.result) THEN COALESCE(CAST(json_extract(r.result,'$.unavailable') AS INTEGER),0)>0 ELSE 0 END`
	switch q.State {
	case "done":
		where += ` AND r.state IN ('done','archived','published','duplicate','deleted') AND NOT (` + warning + `)`
	case "failed":
		where += ` AND r.state='failed'`
	case "issues":
		where += ` AND (r.state='failed' OR (` + warning + `))`
	case "canceled":
		where += ` AND r.state IN ('canceled','ignored')`
	}
	var total int

	page, limit := q.Page, q.Limit
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
	rows, err := db.ReadQuery(base+`SELECT COUNT(*) OVER(),r.record,r.record_key,r.id,r.type,r.state,r.file_id,r.file_name,r.file_exists,r.album_id,r.album_name,r.owner_id,r.last_error,r.result,r.attempts,r.max_attempts,r.run_after,r.cancel_requested,r.created_at,r.updated_at,r.done_at,r.group_json,r.source,r.reason
 FROM visible_logs r`+where+` ORDER BY r.updated_at DESC,r.record DESC,r.id DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UnifiedLogEntry{}
	for rows.Next() {
		t := &Task{}
		a := &ActivityTask{Task: t}
		entry := UnifiedLogEntry{}
		var group, source, reason string
		if err = rows.Scan(&total, &entry.Record, &entry.RecordKey, &t.ID, &t.Type, &t.State, &t.FileID, &a.FileName, &a.FileExists, &a.AlbumID, &a.AlbumName, &a.OwnerID, &t.LastError, &t.Result, &t.Attempts, &t.MaxAttempts, &t.RunAfter, &a.CancelRequested, &t.CreatedAt, &t.UpdatedAt, &t.DoneAt, &group, &source, &reason); err != nil {
			return nil, 0, err
		}
		if a.Group, err = decodeTaskGroup(group); err != nil {
			return nil, 0, err
		}
		if entry.Record == "task" {
			entry.Task = a
		} else {
			entry.Receipt = &LogReceipt{IntakeReceipt: IntakeReceipt{IngestEvent: IngestEvent{ID: t.ID, Type: t.Type, Reason: reason, FileID: t.FileID.String, CreatedAt: t.CreatedAt}, Name: a.FileName, AlbumName: a.AlbumName, Outcome: t.State, Error: t.LastError.String, Exists: a.FileExists}, UpdatedAt: t.UpdatedAt, OwnerID: a.OwnerID, Source: source, RecordKey: entry.RecordKey}
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()
	if len(out) == 0 {
		if err := db.ReadQueryRow(base+`SELECT COUNT(*) FROM visible_logs r`+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}
