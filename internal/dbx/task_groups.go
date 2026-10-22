package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Grouping is a read model. The durable worker jobs, retry leases and audit
// history stay independent; a gallery operation is one user-visible task.
// Carry the numeric operation ID through the read model so joins can use the
// existing primary key. Import membership takes priority over an attached wave.
const taskMembersCTE = `WITH ` + operationItemStatesCTE + `,
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

const taskGroupsCTE = taskMembersCTE + `,
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

type TaskGroup struct {
	CanRetry         *bool  `json:"can_retry,omitempty"`
	CanCancel        *bool  `json:"can_cancel,omitempty"`
	Kind             string `json:"kind"`
	TaskID           int64  `json:"task_id"`
	BatchID          int64  `json:"batch_id"`
	OperationID      int64  `json:"operation_id"`
	InputState       string `json:"input_state"`
	Members          int    `json:"members"`
	Steps            int    `json:"steps"`
	CompletedSteps   int    `json:"completed_steps"`
	FailedSteps      int    `json:"failed_steps"`
	TotalFiles       int    `json:"total_files"`
	ProcessedFiles   int    `json:"processed_files"`
	SucceededFiles   int    `json:"succeeded_files"`
	FailedFiles      int    `json:"failed_files"`
	SkippedFiles     int    `json:"skipped_files"`
	CanceledFiles    int    `json:"canceled_files"`
	UnsubmittedFiles int    `json:"unsubmitted_files"`
	Stage            string `json:"stage"`
	Source           string `json:"source"`
}

func (db *DB) GroupedTaskMembers(id int64, page, limit int) ([]ActivityTask, int, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var key string
	if err := db.ReadQueryRow(taskGroupsCTE+` SELECT group_key FROM task_members WHERE id=(SELECT id FROM grouped_tasks WHERE id=?)`, id).Scan(&key); err != nil {
		return nil, 0, err
	}
	var total int
	if err := db.ReadQueryRow(taskGroupsCTE+` SELECT COUNT(*) FROM task_members WHERE group_key=?`, key).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.ReadQuery(taskGroupsCTE+` SELECT id FROM task_members WHERE group_key=? ORDER BY CASE state WHEN 'failed' THEN 0 WHEN 'running' THEN 1 WHEN 'pending' THEN 2 WHEN 'canceled' THEN 3 ELSE 4 END,id LIMIT ? OFFSET ?`, key, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	var ids []int64
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	result := make([]ActivityTask, 0, len(ids))
	for _, n := range ids {
		t, e := db.GetTask(n)
		if e != nil {
			return nil, 0, e
		}
		requested, e := db.TaskCancelRequested(n)
		if e != nil {
			return nil, 0, e
		}
		item := ActivityTask{Task: t, CancelRequested: requested}
		if t.FileID.Valid {
			if f, e := db.GetFile(t.FileID.String); e == nil {
				item.FileName = f.OrigName
				item.OwnerID = f.OwnerID
				item.FileExists = !f.Hidden
			}
		}
		if item.FileName == "" {
			if e := db.ReadQueryRow(`SELECT i.name,o.owner_id FROM task_operation_jobs j JOIN task_operation_items i ON i.operation_id=j.operation_id AND i.item_key=j.item_key JOIN task_operations o ON o.id=j.operation_id WHERE j.task_id=?`, n).Scan(&item.FileName, &item.OwnerID); e != nil && !errors.Is(e, sql.ErrNoRows) {
				return nil, 0, e
			}
		}
		result = append(result, item)
	}
	return result, total, nil
}

func (db *DB) GetTaskGroup(id int64) (*ActivityTask, error) {
	items, _, err := db.QueryActivityTasks(LogQuery{Grouped: true, GroupID: id, View: "all", Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

func decodeTaskGroup(value string) (*TaskGroup, error) {
	if value == "" {
		return nil, nil
	}
	var group TaskGroup
	if err := json.Unmarshal([]byte(value), &group); err != nil {
		return nil, err
	}
	if group.Members < 1 {
		return nil, errors.New("任务分组损坏")
	}
	return &group, nil
}

// One transaction stops every queued child before a worker can claim it. Running
// leases remain until their operation acknowledges cancellation.
func (db *DB) CancelTaskGroup(id int64, preserveArchive bool) ([]int64, int, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	// Seal intake under the same write lock as child cancellation, before a
	// subsequent HTTP chunk can attach another job to this operation.
	if _, err = tx.Exec(`UPDATE task_operations SET input_state='canceled',updated_at=? WHERE request_key!='' AND anchor_task_id=?`, now(), id); err != nil {
		return nil, 0, err
	}
	condition := ""
	if preserveArchive {
		condition = " AND type NOT IN ('archive','direct_archive','import')"
	}
	rows, err := tx.Query(taskGroupsCTE+` UPDATE tasks SET cancel_requested=1,
 state=CASE WHEN state='running' THEN state ELSE 'canceled' END,updated_at=unixepoch()
 WHERE id IN(SELECT id FROM task_members WHERE group_key=(SELECT group_key FROM task_members WHERE id=?))
 AND state IN('pending','running','failed')`+condition+` RETURNING id,state`, id)
	if err != nil {
		return nil, 0, err
	}
	var running []int64
	count := 0
	for rows.Next() {
		var n int64
		var state string
		if err = rows.Scan(&n, &state); err != nil {
			rows.Close()
			return nil, 0, err
		}
		count++
		if state == "running" {
			running = append(running, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if _, err = tx.Exec(`UPDATE task_operation_items SET state='canceled',updated_at=? WHERE operation_id=(SELECT id FROM task_operations WHERE anchor_task_id=? AND request_key!='') AND state='queued' AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.operation_id=task_operation_items.operation_id AND j.item_key=task_operation_items.item_key)`, now(), id); err != nil {
		return nil, 0, err
	}
	return running, count, tx.Commit()
}
