package dbx

import (
	"encoding/json"
)

// All filters are applied before counting and pagination. Sorting always has an
// ID tie-breaker so equal timestamps cannot repeat a record on the next page.
type LogQuery struct {
	FileID, View, State, Type, Search, Sort string
	From                                    int64
	Page, Limit                             int
	Grouped                                 bool
	GroupID                                 int64
}

func logOrder(sort, prefix string, tasks bool) string {
	switch sort {
	case "active":
		if tasks {
			return "CASE " + prefix + "state WHEN 'running' THEN 0 ELSE 1 END," + prefix + "created_at ASC," + prefix + "id ASC"
		}
	case "created_asc":
		return prefix + "created_at ASC," + prefix + "id ASC"
	case "updated_desc":
		return prefix + "updated_at DESC," + prefix + "id DESC"
	case "attempts_desc":
		if tasks {
			return prefix + "attempts DESC," + prefix + "id DESC"
		}
	}
	return prefix + "created_at DESC," + prefix + "id DESC"
}

type LogReceipt struct {
	IntakeReceipt
	UpdatedAt int64      `json:"updated_at"`
	Source    string     `json:"source"`
	OwnerID   string     `json:"owner_id"`
	Group     *TaskGroup `json:"group,omitempty"`
	RecordKey string     `json:"record_key,omitempty"`
}

// One row per intake identity. Verification and deduplication stages remain
// in that receipt's process rather than appearing as a second audit table.
const logReceiptsCTE = `WITH ` + logReceiptsBody

const logReceiptsBody = `latest AS (
 SELECT source_key,MAX(id) id,MIN(created_at) first_at FROM ingest_events WHERE source_key NOT LIKE 'verify:%' GROUP BY source_key
 ), verified AS (SELECT file_id,MAX(created_at) updated_at FROM ingest_events WHERE source_key LIKE 'verify:%' GROUP BY file_id), receipts AS (
 SELECT e.id,e.source_key,e.event_type,e.reason,e.file_id,e.tool_file_id,e.upload_id,e.body,l.first_at created_at,
 MAX(e.created_at,COALESCE(f.updated_at,e.created_at),COALESCE(v.updated_at,e.created_at)) updated_at,
 COALESCE(json_extract(e.body,'$.orig_name'),NULLIF(f.orig_name,''),json_extract(e.body,'$.name'),f.name,'') name,
 COALESCE(a.name,json_extract(e.body,'$.album_name'),'') album_name,
 CASE WHEN e.event_type='failed' THEN 'failed' WHEN e.event_type='duplicate' THEN 'duplicate' WHEN e.event_type='retired_ignored' THEN 'ignored'
 WHEN f.id IS NULL THEN 'deleted' WHEN f.archive_state='failed' OR COALESCE(f.archive_error,'')!='' OR COALESCE(f.publish_error,'')!='' THEN 'failed'
 WHEN f.published=1 THEN 'published' WHEN f.archive_state='done' THEN 'archived' ELSE 'processing' END outcome,
 COALESCE(NULLIF(f.archive_error,''),NULLIF(f.publish_error,''),CASE WHEN e.event_type='failed' THEN e.reason END,'') error,
 f.id IS NOT NULL present,
 CASE WHEN json_extract(e.body,'$.telegram_source') LIKE 'local-import:%' THEN 'local_import' ELSE COALESCE(json_extract(e.body,'$.source'),CASE WHEN e.tool_file_id!='' THEN 'uploader' ELSE 'pimg' END) END source,COALESCE(f.owner_id,'') owner_id
 FROM latest l JOIN ingest_events e ON e.id=l.id LEFT JOIN files f ON f.id=e.file_id LEFT JOIN albums a ON a.id=f.album_id LEFT JOIN verified v ON v.file_id=e.file_id)
 `

func (db *DB) QueryLogReceipts(q LogQuery) ([]LogReceipt, int, error) {
	const base = logReceiptsCTE
	where := ` WHERE 1=1`
	args := []any{}
	if q.FileID != "" {
		where += ` AND r.file_id=?`
		args = append(args, q.FileID)
	}
	if q.State != "" {
		where += ` AND r.outcome=?`
		args = append(args, q.State)
	}
	if q.Type == "verified" {
		where += ` AND EXISTS(SELECT 1 FROM ingest_events v WHERE v.event_type='verified' AND (v.source_key=r.source_key OR (r.file_id!='' AND v.source_key='verify:'||r.file_id)))`
	} else if q.Type != "" {
		where += ` AND r.event_type=?`
		args = append(args, q.Type)
	}
	if q.From > 0 {
		where += ` AND r.created_at>=?`
		args = append(args, q.From)
	}
	if q.Search != "" {
		where += ` AND (instr(lower('R'||CAST(r.id AS TEXT)||' '||r.name||' '||r.album_name||' '||r.file_id||' '||r.tool_file_id||' '||r.upload_id||' '||r.reason||' '||r.error),lower(?))>0 OR EXISTS(SELECT 1 FROM ingest_events v WHERE v.source_key=r.source_key AND instr(lower(v.tool_file_id||' '||v.upload_id||' '||v.reason),lower(?))>0))`
		args = append(args, q.Search, q.Search)
	}
	var total int
	if err := db.ReadQueryRow(base+`SELECT COUNT(*) FROM receipts r`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, limit := q.Page, q.Limit
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := db.ReadQuery(base+`SELECT r.id,r.source_key,r.event_type,r.reason,r.file_id,r.tool_file_id,r.upload_id,r.body,r.created_at,r.name,r.album_name,r.outcome,r.error,r.present,r.updated_at,r.source,r.owner_id FROM receipts r`+where+` ORDER BY `+logOrder(q.Sort, "r.", false)+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []LogReceipt{}
	for rows.Next() {
		var e LogReceipt
		var body string
		if err = rows.Scan(&e.ID, &e.SourceKey, &e.Type, &e.Reason, &e.FileID, &e.ToolFileID, &e.UploadID, &body, &e.CreatedAt, &e.Name, &e.AlbumName, &e.Outcome, &e.Error, &e.Exists, &e.UpdatedAt, &e.Source, &e.OwnerID); err != nil {
			return nil, 0, err
		}
		e.Body = json.RawMessage(body)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// The default intake view follows the same invocation boundary as Tasks. Old
// unbound receipts keep their original rows, while a file filter deliberately
// opens the detailed per-source history rather than hiding it in a summary.
func (db *DB) QueryGroupedLogReceipts(q LogQuery) ([]LogReceipt, int, error) {
	if q.FileID != "" {
		return db.QueryLogReceipts(q)
	}
	if err := db.expireDueTaskOperations(); err != nil {
		return nil, 0, err
	}
	base := taskGroupsCTE + `,` + logReceiptsBody + `,
 intake_groups AS MATERIALIZED (
 SELECT * FROM grouped_tasks WHERE type IN ('upload_group','local_import_batch')),
 receipt_candidates AS MATERIALIZED (
 SELECT r.*,CASE
 WHEN CAST(COALESCE(json_extract(r.body,'$.operation_id'),0) AS INTEGER)>0 THEN 'operation:'||CAST(json_extract(r.body,'$.operation_id') AS INTEGER)
 WHEN json_extract(r.body,'$.telegram_source') LIKE 'local-import:%' THEN 'import:'||CAST(substr(json_extract(r.body,'$.telegram_source'),14) AS INTEGER)
 WHEN r.source_key LIKE 'local-import:%' THEN 'import:'||CAST(substr(r.source_key,14) AS INTEGER)
 ELSE '' END candidate_key FROM receipts r),
 receipt_bindings AS MATERIALIZED (
 SELECT r.*,CASE WHEN EXISTS(SELECT 1 FROM intake_groups g WHERE g.group_key=r.candidate_key) THEN r.candidate_key ELSE '' END record_key
 FROM receipt_candidates r),
 bound_receipt_progress AS MATERIALIZED (
 SELECT record_key,MAX(updated_at) updated_at,
 GROUP_CONCAT(name||' '||file_id||' '||tool_file_id||' '||upload_id||' '||reason||' '||error,' ') search_text
 FROM receipt_bindings WHERE record_key!='' GROUP BY record_key),
 import_item_search AS MATERIALIZED (
 SELECT batch_id,GROUP_CONCAT(path||' '||error,' ') search_text FROM local_import_items GROUP BY batch_id),
 combined_receipts AS (
 SELECT r.id,r.source_key,r.event_type,r.reason,r.file_id,r.tool_file_id,r.upload_id,r.body,r.created_at,r.name,r.album_name,r.outcome,r.error,r.present,r.updated_at,r.source,r.owner_id,
 '' group_json,'receipt:'||r.id record_key,
 r.name||' '||r.file_id||' '||r.tool_file_id||' '||r.upload_id||' '||r.reason||' '||r.error search_text
 FROM receipt_bindings r WHERE r.record_key=''
 UNION ALL
 SELECT -g.id,g.group_key,CASE WHEN g.state='failed' THEN 'failed' ELSE 'accepted' END,g.last_error,'','','','{}',g.created_at,'','',
 CASE WHEN g.state IN ('running','pending') OR json_extract(g.group_json,'$.stage')='ready' THEN 'processing'
 WHEN g.state='failed' THEN 'failed' WHEN g.state='canceled' THEN 'ignored' ELSE 'archived' END,g.last_error,0,
 MAX(g.updated_at,COALESCE(p.updated_at,0)),CASE WHEN g.type='upload_group' THEN 'pimg' ELSE 'local_import' END,g.retained_owner,
 g.group_json,g.group_key,g.search_text||' '||COALESCE(p.search_text,'')||' '||COALESCE(i.search_text,'')
 FROM intake_groups g LEFT JOIN bound_receipt_progress p ON p.record_key=g.group_key
 LEFT JOIN import_item_search i ON i.batch_id=CASE WHEN json_valid(g.group_json) THEN json_extract(g.group_json,'$.batch_id') END)
 `
	where := ` WHERE 1=1`
	args := []any{}
	if q.State != "" {
		where += ` AND r.outcome=?`
		args = append(args, q.State)
	}
	if q.Type == "verified" {
		where += ` AND (EXISTS(SELECT 1 FROM ingest_events v WHERE v.event_type='verified' AND (v.source_key=r.source_key OR (r.file_id!='' AND v.source_key='verify:'||r.file_id)))
 OR EXISTS(SELECT 1 FROM receipt_bindings b JOIN ingest_events v ON v.source_key=b.source_key OR (b.file_id!='' AND v.source_key='verify:'||b.file_id) WHERE b.record_key=r.record_key AND v.event_type='verified'))`
	} else if q.Type != "" {
		where += ` AND (r.event_type=? OR EXISTS(SELECT 1 FROM receipt_bindings b WHERE b.record_key=r.record_key AND b.event_type=?))`
		args = append(args, q.Type, q.Type)
	}
	if q.From > 0 {
		where += ` AND r.created_at>=?`
		args = append(args, q.From)
	}
	if q.Search != "" {
		where += ` AND instr(lower('R'||CAST(r.id AS TEXT)||' '||r.record_key||' '||r.name||' '||r.album_name||' '||r.error||' '||r.search_text),lower(?))>0`
		args = append(args, q.Search)
	}
	var total int
	if err := db.ReadQueryRow(base+`SELECT COUNT(*) FROM combined_receipts r`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
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
	args = append(args, limit, (page-1)*limit)
	rows, err := db.ReadQuery(base+`SELECT r.id,r.source_key,r.event_type,r.reason,r.file_id,r.tool_file_id,r.upload_id,r.body,r.created_at,r.name,r.album_name,r.outcome,r.error,r.present,r.updated_at,r.source,r.owner_id,r.group_json,r.record_key FROM combined_receipts r`+where+` ORDER BY `+logOrder(q.Sort, "r.", false)+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []LogReceipt{}
	for rows.Next() {
		var e LogReceipt
		var body, group string
		if err = rows.Scan(&e.ID, &e.SourceKey, &e.Type, &e.Reason, &e.FileID, &e.ToolFileID, &e.UploadID, &body, &e.CreatedAt, &e.Name, &e.AlbumName, &e.Outcome, &e.Error, &e.Exists, &e.UpdatedAt, &e.Source, &e.OwnerID, &group, &e.RecordKey); err != nil {
			return nil, 0, err
		}
		e.Body = json.RawMessage(body)
		if e.Group, err = decodeTaskGroup(group); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}
