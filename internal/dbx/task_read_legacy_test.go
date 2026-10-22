// Released v1.0.22 read model, frozen solely for semantic/performance regression.
package dbx

const legacyOperationStatesCTE = `operation_job_attempts AS MATERIALIZED (
 SELECT j.operation_id,j.item_key,t.*,
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

const legacyTaskMembersCTE = `WITH ` + legacyOperationStatesCTE + `,
 operation_progress AS MATERIALIZED (
 SELECT operation_id,COUNT(*) total,SUM(state IN ('done','skipped','failed','canceled')) processed,
 SUM(state='done') succeeded,SUM(state='failed') failed,SUM(state='skipped') skipped,SUM(state='canceled') canceled,
 MAX(CASE WHEN state='failed' THEN error END) error,MAX(name) name,
 GROUP_CONCAT(name||' '||file_id||' '||error,' ') search_text
 FROM operation_item_states GROUP BY operation_id),
 import_parents AS MATERIALIZED (
 SELECT t.*,CASE WHEN json_valid(payload) THEN CAST(json_extract(payload,'$.batch_id') AS INTEGER) END batch_id
 FROM tasks t WHERE type IN ('local_import_scan','local_import','local_import_idle')),
 import_files AS MATERIALIZED (
 SELECT j.value file_id,MIN(i.batch_id) batch_id FROM local_import_items i,
 json_each(CASE WHEN json_valid(i.file_ids) THEN i.file_ids ELSE '[]' END) j
 WHERE i.state!='duplicate' AND j.type='text' GROUP BY j.value),
 import_bounds AS MATERIALIZED (
 SELECT b.id,b.state,b.error,b.created_at,b.root,b.schedule,b.context,
 MAX(b.created_at,COALESCE((SELECT MAX(updated_at) FROM import_parents p WHERE p.batch_id=b.id),b.created_at),
 COALESCE((SELECT MAX(t.updated_at) FROM tasks t JOIN import_files f ON f.file_id=t.file_id
 WHERE f.batch_id=b.id AND t.type IN ('archive','direct_archive')),b.created_at)) cutoff
 FROM local_import_batches b),
 import_progress AS MATERIALIZED (
 SELECT i.batch_id,COUNT(*) total,
 SUM(CASE WHEN i.state IN ('done','duplicate','skipped','failed') THEN 1
 WHEN i.state='archiving' AND i.file_ids!='[]' AND NOT EXISTS(
 SELECT 1 FROM json_each(CASE WHEN json_valid(i.file_ids) THEN i.file_ids ELSE '[]' END) j
 LEFT JOIN files f ON f.id=j.value WHERE f.id IS NULL OR f.archive_state!='done') THEN 1 ELSE 0 END) processed,
 SUM(i.state='failed') failed,SUM(i.state IN ('duplicate','skipped')) skipped,
 SUM(CASE WHEN i.state='done' THEN 1 WHEN i.state='archiving' AND i.file_ids!='[]' AND NOT EXISTS(
 SELECT 1 FROM json_each(CASE WHEN json_valid(i.file_ids) THEN i.file_ids ELSE '[]' END) j
 LEFT JOIN files f ON f.id=j.value WHERE f.id IS NULL OR f.archive_state!='done') THEN 1 ELSE 0 END) succeeded,
 MAX(CASE WHEN i.state='failed' THEN NULLIF(i.error,'') END) error,
 GROUP_CONCAT(CASE WHEN i.state='failed' THEN i.path||' '||i.error END,' ') search_text
 FROM local_import_items i GROUP BY i.batch_id),
 member_batches AS MATERIALIZED (
 SELECT t.*,CASE WHEN p.batch_id IS NOT NULL THEN p.batch_id
 WHEN t.type IN ('archive','direct_archive') AND t.created_at>=b.created_at
 AND (b.state NOT IN ('done','done_errors','canceled','failed') OR t.created_at<=b.cutoff) THEN b.id ELSE 0 END batch_id
 FROM tasks t LEFT JOIN import_parents p ON p.id=t.id
 LEFT JOIN import_files f ON f.file_id=t.file_id LEFT JOIN import_bounds b ON b.id=f.batch_id),
 task_members AS MATERIALIZED (
 SELECT m.*,CASE WHEN batch_id>0 THEN NULL ELSE o.operation_id END group_operation_id,
 CASE WHEN batch_id>0 THEN 'import:'||batch_id
 WHEN o.operation_id IS NOT NULL THEN CASE WHEN op.request_key!='' THEN 'operation:' ELSE op.kind||':' END||o.operation_id
 WHEN type IN ('archive','direct_archive') AND COALESCE(file_id,'')!='' THEN 'archive:'||file_id
 ELSE 'task:'||m.id END group_key FROM member_batches m LEFT JOIN task_operation_jobs o ON o.task_id=m.id LEFT JOIN task_operations op ON op.id=o.operation_id
 WHERE m.type NOT IN ('storage_probe','source_scan','source_cleanup')
 AND NOT(m.type='system_schedule' AND m.state='running')
 AND NOT(m.type='tg_scan' AND COALESCE(CASE WHEN json_valid(m.payload) THEN json_type(m.payload,'$.automatic')='true' ELSE 0 END,0)=1
 AND m.state!='failed' AND COALESCE(m.last_error,'')='' AND o.operation_id IS NULL)),
 ranked_members AS MATERIALIZED (
 SELECT m.*,ROW_NUMBER() OVER(PARTITION BY group_key,type,COALESCE(file_id,''),COALESCE(payload,'') ORDER BY id DESC) relevance
 FROM task_members m)`

const legacyTaskGroupsCTE = legacyTaskMembersCTE + `,
 grouped_file_states AS MATERIALIZED (
 SELECT group_key,file_id,CASE WHEN SUM(state='running')>0 THEN 'running' WHEN SUM(state='pending')>0 THEN 'pending'
 WHEN SUM(state='failed')>0 THEN 'failed' WHEN SUM(state='canceled')>0 THEN 'canceled' ELSE 'done' END state
 FROM ranked_members WHERE relevance=1 AND COALESCE(file_id,'')!='' AND type!='operation_intake' GROUP BY group_key,file_id),
 grouped_file_progress AS MATERIALIZED (
 SELECT group_key,COUNT(*) total,SUM(state IN ('done','failed','canceled')) processed,
 SUM(state='done') succeeded,SUM(state='failed') failed,SUM(state='canceled') canceled
 FROM grouped_file_states GROUP BY group_key),
 grouped_tasks AS (
 SELECT m.group_key,COALESCE(o.anchor_task_id,MIN(m.id)) id,
 CASE WHEN m.batch_id>0 THEN 'local_import_batch' WHEN o.id IS NOT NULL THEN o.kind||'_group' WHEN m.group_key LIKE 'sync:%' THEN 'sync_group' WHEN m.group_key LIKE 'archive:%' THEN 'archive_group' ELSE MAX(m.type) END type,
 CASE WHEN m.batch_id>0 OR o.id IS NOT NULL OR m.group_key LIKE 'sync:%' OR COUNT(DISTINCT m.file_id)>1 THEN NULL ELSE MAX(m.file_id) END file_id,MAX(m.payload) payload,
 CASE WHEN SUM(m.relevance=1 AND m.type!='operation_intake' AND m.state='running')>0 THEN 'running'
 WHEN SUM(m.relevance=1 AND m.type!='operation_intake' AND m.state='pending')>0 OR o.input_state='open' OR b.state IN ('paused','running','scanning') THEN 'pending'
 WHEN b.state='canceled' OR o.input_state='canceled' THEN 'canceled'
 WHEN SUM(m.relevance=1 AND m.state='failed')>0 OR COALESCE(op.failed,0)>0 OR COALESCE(p.failed,0)>0 OR b.state IN ('failed','done_errors') THEN 'failed'
 WHEN SUM(m.relevance=1 AND m.state='canceled')>0 OR COALESCE(op.canceled,0)>0 THEN 'canceled' ELSE 'done' END state,
 MAX(m.attempts) attempts,MAX(m.max_attempts) max_attempts,
 COALESCE(NULLIF(b.error,''),NULLIF(p.error,''),MAX(CASE WHEN m.relevance=1 AND m.state='failed' THEN m.last_error END),NULLIF(op.error,''),'') last_error,
 CASE WHEN COUNT(*)=1 AND m.batch_id=0 THEN MAX(m.result) END result,
 COALESCE(MIN(CASE WHEN m.type!='operation_intake' THEN m.run_after END),0) run_after,MAX(m.lease_until) lease_until,MIN(m.created_at) created_at,MAX(MAX(m.updated_at),COALESCE(o.updated_at,0)) updated_at,MAX(m.done_at) done_at,
 CASE WHEN b.state='canceled' OR o.input_state='canceled' THEN 1 ELSE COALESCE(MIN(CASE WHEN m.relevance=1 AND m.state IN ('running','pending') THEN m.cancel_requested END),0) END cancel_requested,
 CASE WHEN COUNT(*)>1 OR m.batch_id>0 OR o.id IS NOT NULL OR m.group_key LIKE 'archive:%' OR m.group_key LIKE 'sync:%' THEN
 json_object('kind',CASE WHEN m.batch_id>0 THEN 'import' WHEN o.id IS NOT NULL THEN o.kind WHEN m.group_key LIKE 'sync:%' THEN 'sync' ELSE 'archive' END,'batch_id',COALESCE(m.batch_id,0),
 'task_id',COALESCE(o.anchor_task_id,MIN(m.id)),
 'operation_id',CASE WHEN o.request_key!='' THEN o.id ELSE 0 END,'input_state',COALESCE(o.input_state,''),
 'members',COUNT(*),'steps',SUM(m.relevance=1 AND m.type!='operation_intake'),'completed_steps',SUM(m.relevance=1 AND m.type!='operation_intake' AND m.state='done'),
 'failed_steps',SUM(m.relevance=1 AND m.type!='operation_intake' AND m.state='failed'),
 'total_files',CASE WHEN m.batch_id>0 THEN COALESCE(p.total,0) WHEN o.request_key!='' THEN MAX(o.expected_files,COALESCE(op.total,0)) ELSE COALESCE(op.total,fp.total,0) END,
 'processed_files',CASE WHEN m.batch_id>0 THEN COALESCE(p.processed,0) ELSE COALESCE(op.processed,fp.processed,0)+CASE WHEN o.request_key!='' AND o.input_state IN ('sealed','expired','canceled') THEN MAX(0,o.expected_files-COALESCE(op.total,0)) ELSE 0 END END,
 'unsubmitted_files',CASE WHEN o.request_key!='' THEN MAX(0,o.expected_files-COALESCE(op.total,0)) ELSE 0 END,
 'succeeded_files',CASE WHEN m.batch_id>0 THEN COALESCE(p.succeeded,0) ELSE COALESCE(op.succeeded,fp.succeeded,0) END,
 'failed_files',CASE WHEN m.batch_id>0 THEN COALESCE(p.failed,0) ELSE COALESCE(op.failed,fp.failed,0)+CASE WHEN o.request_key!='' AND o.input_state IN ('sealed','expired') THEN MAX(0,o.expected_files-COALESCE(op.total,0)) ELSE 0 END END,
 'skipped_files',CASE WHEN m.batch_id>0 THEN COALESCE(p.skipped,0) ELSE COALESCE(op.skipped,0) END,
 'canceled_files',CASE WHEN m.batch_id>0 AND b.state='canceled' THEN MAX(0,COALESCE(p.total,0)-COALESCE(p.processed,0)) ELSE COALESCE(op.canceled,fp.canceled,0)+CASE WHEN o.request_key!='' AND o.input_state='canceled' THEN MAX(0,o.expected_files-COALESCE(op.total,0)) ELSE 0 END END,
 'context',json(COALESCE(b.context,o.context,'{}')),'stage',COALESCE(b.state,CASE WHEN o.request_key!='' THEN o.input_state END,''),
 'source',COALESCE(b.root,CASE o.kind WHEN 'archive' THEN '上传归档' WHEN 'upload' THEN '上传' WHEN 'purge' THEN '批量删除' WHEN 'publish' THEN '批量永久发布' WHEN 'unpublish' THEN '批量撤销发布' WHEN 'move_album' THEN '批量移动相册' WHEN 'set_media_time' THEN '批量修改日期' WHEN 'convert' THEN '文件加密转换' WHEN 'maintenance' THEN '本地归档整理' WHEN 'telegram_receive' THEN 'Telegram 文件接收' WHEN 'sync' THEN '归档存储同步' ELSE '' END,'')) END group_json,
 CASE WHEN op.total=1 THEN COALESCE(op.name,'') ELSE '' END retained_name,COALESCE(o.owner_id,'') retained_owner,COALESCE(op.search_text,'')||' '||COALESCE(p.search_text,'') search_text
 FROM ranked_members m LEFT JOIN import_bounds b ON b.id=m.batch_id
 LEFT JOIN import_progress p ON p.batch_id=m.batch_id LEFT JOIN task_operations o ON o.id=m.group_operation_id
 LEFT JOIN operation_progress op ON op.operation_id=o.id LEFT JOIN grouped_file_progress fp ON fp.group_key=m.group_key GROUP BY m.group_key)
`

const legacyUnifiedLogsCTE = legacyTaskGroupsCTE + `,` + logReceiptsBody + `,
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

func (db *DB) legacyActivityTasks(q LogQuery) ([]ActivityTask, int, error) {
	fileID, view, state, page, limit := q.FileID, q.View, q.State, q.Page, q.Limit
	where := ` WHERE (?='' OR t.file_id=?)`
	prefix, table, extra := "", "tasks", "''"
	retainedName, retainedOwner, searchMembers := "''", "''", "''"
	if q.Grouped {
		if err := db.expireDueTaskOperations(); err != nil {
			return nil, 0, err
		}
		prefix, table, extra = legacyTaskGroupsCTE, "grouped_tasks", "COALESCE(t.group_json,'')"
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

func (db *DB) legacyUnifiedLogs(q LogQuery) ([]UnifiedLogEntry, int, error) {
	if err := db.expireDueTaskOperations(); err != nil {
		return nil, 0, err
	}
	where := ` WHERE 1=1`
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
	rows, err := db.ReadQuery(legacyUnifiedLogsCTE+`SELECT COUNT(*) OVER(),r.record,r.record_key,r.id,r.type,r.state,r.file_id,r.file_name,r.file_exists,r.album_id,r.album_name,r.owner_id,r.last_error,r.result,r.attempts,r.max_attempts,r.run_after,r.cancel_requested,r.created_at,r.updated_at,r.done_at,r.group_json,r.source,r.reason
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
		if err := db.ReadQueryRow(legacyUnifiedLogsCTE+`SELECT COUNT(*) FROM visible_logs r`+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}
