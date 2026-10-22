package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const DefaultLogRetentionDays = 30
const MaxLogRetentionDays = 3650
const logRetentionKey = "site.log_retention_days"

func parseLogRetention(value string) (int, error) {
	if value == "" {
		return DefaultLogRetentionDays, nil
	}
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > MaxLogRetentionDays {
		return 0, fmt.Errorf("日志保留时间须为 1..%d 天，设置无效时不会清理", MaxLogRetentionDays)
	}
	return days, nil
}

func (db *DB) LogRetentionDays() (int, error) {
	value, err := db.GetSetting(logRetentionKey)
	if err != nil {
		return 0, err
	}
	return parseLogRetention(value)
}

type LogCleanupResult struct {
	RetentionDays int   `json:"retention_days"`
	Cutoff        int64 `json:"cutoff"`
	Operations    int64 `json:"operations"`
	Receipts      int64 `json:"receipts"`
	Tasks         int64 `json:"tasks"`
	Events        int64 `json:"events"`
	ImportBatches int64 `json:"import_batches"`
	More          bool  `json:"more"`
}

// These are operational dependencies, not disposable history. In particular a
// canceled archive_sync is a reconciliation tombstone while its file exists.
// Keeping the entire visible group prevents partial progress/history removal.
const logTaskProtected = `
 COALESCE(t.lease_until,0)>:now
 OR EXISTS(SELECT 1 FROM log_cleanup_references r WHERE r.id=t.id)
 OR EXISTS(SELECT 1 FROM log_cleanup_active_files p WHERE p.file_id=t.file_id)
 OR EXISTS(SELECT 1 FROM files f WHERE f.id=t.file_id AND
   (f.archive_state!='done' OR COALESCE(f.purge_state,'')!='' OR f.hidden=1 OR t.type='import' OR (t.type='archive_sync' AND t.state='canceled')))
 OR EXISTS(SELECT 1 FROM task_operation_jobs j JOIN log_cleanup_active_operations o ON o.id=j.operation_id WHERE j.task_id=t.id)
 OR EXISTS(SELECT 1 FROM task_operation_jobs j JOIN task_operations o ON o.id=j.operation_id WHERE j.task_id=t.id AND (o.input_state IN ('open','expired') OR o.updated_at>=:cutoff))
 OR EXISTS(SELECT 1 FROM task_operation_jobs j JOIN operation_item_states i ON i.operation_id=j.operation_id WHERE j.task_id=t.id AND i.state IN ('queued','pending','running','failed'))
 OR EXISTS(SELECT 1 FROM log_cleanup_protected_intakes p WHERE p.group_key=m.group_key)
 OR (t.type IN ('local_import_scan','local_import','local_import_idle') AND EXISTS(
   SELECT 1 FROM local_import_batches b WHERE b.id=CASE WHEN json_valid(t.payload) THEN CAST(json_extract(t.payload,'$.batch_id') AS INTEGER) END
   AND b.state NOT IN ('done','canceled')))
`

const logCleanupGroups = taskGroupsCTE + `,
 cleanup_members AS (
 SELECT id,group_key,batch_id FROM task_members UNION ALL
 SELECT t.id,'hidden:'||t.id,0 FROM tasks t WHERE t.type IN ('storage_probe','source_scan','source_cleanup')
 OR (t.type='tg_scan' AND COALESCE(CASE WHEN json_valid(t.payload) THEN json_type(t.payload,'$.automatic')='true' ELSE 0 END,0)=1
 AND t.state!='failed' AND COALESCE(t.last_error,'')='' AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.task_id=t.id)))
 SELECT m.group_key,MAX(m.batch_id) batch_id FROM cleanup_members m JOIN tasks t ON t.id=m.id
 GROUP BY m.group_key
 HAVING MIN(t.state IN ('done','canceled') OR (t.type='system_schedule' AND t.state='failed'))=1
 AND MAX(MAX(t.created_at,t.updated_at,COALESCE(t.done_at,0)))<:cutoff
 AND SUM(CASE WHEN ` + logTaskProtected + ` THEN 1 ELSE 0 END)=0
 ORDER BY MIN(t.id) LIMIT 101`

const logCleanupSources = `SELECT e.source_key FROM ingest_events e GROUP BY e.source_key
 HAVING MAX(e.created_at)<:cutoff
 AND (SELECT x.event_type FROM ingest_events x WHERE x.source_key=e.source_key ORDER BY x.id DESC LIMIT 1)!='failed'
 AND SUM(CASE WHEN EXISTS(SELECT 1 FROM task_operations o WHERE o.id=CASE WHEN json_valid(e.body) THEN CAST(json_extract(e.body,'$.operation_id') AS INTEGER) END AND o.request_key!='')
 OR EXISTS(SELECT 1 FROM local_import_batches b WHERE b.id=CASE WHEN json_valid(e.body) AND json_extract(e.body,'$.telegram_source') LIKE 'local-import:%' THEN CAST(substr(json_extract(e.body,'$.telegram_source'),14) AS INTEGER) WHEN e.source_key LIKE 'local-import:%' THEN CAST(substr(e.source_key,14) AS INTEGER) END) THEN 1 ELSE 0 END)=0
 AND SUM(CASE WHEN e.file_id!='' AND (
 EXISTS(SELECT 1 FROM files f WHERE f.id=e.file_id AND (f.updated_at>=:cutoff OR f.archive_state!='done' OR COALESCE(f.archive_error,'')!='' OR COALESCE(f.publish_error,'')!='' OR COALESCE(f.purge_state,'')!=''))
 OR EXISTS(SELECT 1 FROM log_cleanup_active_files p WHERE p.file_id=e.file_id)
 OR EXISTS(SELECT 1 FROM log_cleanup_recent_files p WHERE p.file_id=e.file_id)
 ) THEN 1 ELSE 0 END)=0
 ORDER BY MIN(e.id) LIMIT 501`

const logReceiptIntakeKey = `CASE WHEN json_valid(e.body) AND CAST(COALESCE(json_extract(e.body,'$.operation_id'),0) AS INTEGER)>0 THEN 'operation:'||CAST(json_extract(e.body,'$.operation_id') AS INTEGER)
 WHEN json_valid(e.body) AND json_extract(e.body,'$.telegram_source') LIKE 'local-import:%' THEN 'import:'||CAST(substr(json_extract(e.body,'$.telegram_source'),14) AS INTEGER)
 WHEN e.source_key LIKE 'local-import:%' THEN 'import:'||CAST(substr(e.source_key,14) AS INTEGER) ELSE '' END`

// One bounded transaction rechecks state and dependencies under the writer lock.
// It never touches files, source identities, archive objects or recovery state.
// A large backlog is continued by the background loop without durable tasks.
func (db *DB) CleanupLogs(ctx context.Context, now int64) (result LogCleanupResult, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result, err = cleanupLogsTx(ctx, tx, now, false)
	if err != nil {
		return result, err
	}
	err = tx.Commit()
	return result, err
}

func cleanupLogsTx(ctx context.Context, tx *sql.Tx, now int64, forceClosed bool) (result LogCleanupResult, err error) {
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET updated_at=updated_at WHERE 0`); err != nil {
		return result, err
	}
	if err = expireTaskOperations(tx, now); err != nil {
		return result, err
	}
	var value string
	err = tx.QueryRowContext(ctx, `SELECT v FROM settings WHERE k=?`, logRetentionKey).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	result.RetentionDays, err = parseLogRetention(value)
	if err != nil {
		return result, err
	}
	result.Cutoff = now - int64(result.RetentionDays)*86400
	if forceClosed {
		result.Cutoff = now + 1
	}
	args := []any{sql.Named("now", now), sql.Named("cutoff", result.Cutoff)}
	for _, query := range []string{
		`CREATE TEMP TABLE log_cleanup_groups(group_key TEXT PRIMARY KEY,batch_id INTEGER NOT NULL)`,
		`CREATE TEMP TABLE log_cleanup_jobs(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE log_cleanup_sources(source_key TEXT PRIMARY KEY,whole_group INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TEMP TABLE log_cleanup_references(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE log_cleanup_active_files(file_id TEXT PRIMARY KEY)`,
		`CREATE TEMP TABLE log_cleanup_recent_files(file_id TEXT PRIMARY KEY)`,
		`CREATE TEMP TABLE log_cleanup_active_operations(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE log_cleanup_protected_intakes(group_key TEXT PRIMARY KEY)`,
	} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return result, err
		}
	}
	// Materialize large recovery/reference sets once, rather than scanning a
	// repository or a whole operation again for every historical worker step.
	for _, query := range []string{
		`INSERT INTO log_cleanup_references SELECT task_id FROM file_conversions UNION SELECT task_id FROM cold_sync_files UNION SELECT scan_task_id FROM album_topics UNION SELECT run_task_id FROM system_schedules WHERE run_task_id>0`,
		`INSERT OR IGNORE INTO log_cleanup_active_files SELECT file_id FROM tasks WHERE state NOT IN ('done','canceled') AND COALESCE(file_id,'')!=''
 UNION SELECT source_id FROM file_conversions WHERE phase!='done' UNION SELECT target_id FROM file_conversions WHERE phase!='done'
 UNION SELECT file_id FROM tg_moves
 UNION SELECT j.value FROM local_import_items i JOIN local_import_batches b ON b.id=i.batch_id,
 json_each(CASE WHEN json_valid(i.file_ids) THEN i.file_ids ELSE '[]' END) j WHERE b.state NOT IN ('done','canceled') AND j.type='text'`,
		`INSERT OR IGNORE INTO log_cleanup_recent_files SELECT file_id FROM tasks WHERE COALESCE(file_id,'')!='' AND MAX(created_at,updated_at,COALESCE(done_at,0))>=:cutoff
 UNION SELECT file_id FROM ingest_events WHERE file_id!='' AND created_at>=:cutoff`,
		`INSERT INTO log_cleanup_active_operations SELECT j.operation_id FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id GROUP BY j.operation_id
 HAVING MIN(t.state IN ('done','canceled'))=0 OR MAX(MAX(t.created_at,t.updated_at,COALESCE(t.done_at,0)))>=:cutoff`,
		`INSERT OR IGNORE INTO log_cleanup_protected_intakes SELECT ` + logReceiptIntakeKey + ` FROM ingest_events e WHERE
 (e.created_at>=:cutoff
 OR (e.event_type='failed' AND e.id=(SELECT MAX(x.id) FROM ingest_events x WHERE x.source_key=e.source_key))
 OR (e.file_id!='' AND (EXISTS(SELECT 1 FROM files f WHERE f.id=e.file_id AND (f.updated_at>=:cutoff OR f.archive_state!='done' OR COALESCE(f.archive_error,'')!='' OR COALESCE(f.publish_error,'')!='' OR COALESCE(f.purge_state,'')!=''))
 OR EXISTS(SELECT 1 FROM log_cleanup_active_files p WHERE p.file_id=e.file_id)
 OR EXISTS(SELECT 1 FROM log_cleanup_recent_files p WHERE p.file_id=e.file_id))))`,
	} {
		queryArgs := []any{}
		if strings.Contains(query, ":cutoff") {
			queryArgs = append(queryArgs, sql.Named("cutoff", result.Cutoff))
		}
		if _, err = tx.ExecContext(ctx, query, queryArgs...); err != nil {
			return result, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO log_cleanup_groups `+logCleanupGroups, args...); err != nil {
		return result, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_cleanup_groups`).Scan(&count); err != nil {
		return result, err
	}
	result.More = count > 100
	if count > 100 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM log_cleanup_groups WHERE rowid NOT IN(SELECT rowid FROM log_cleanup_groups ORDER BY rowid LIMIT 100)`); err != nil {
			return result, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_cleanup_groups WHERE group_key NOT LIKE 'hidden:%'`).Scan(&result.Operations); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, taskGroupsCTE+` INSERT INTO log_cleanup_jobs SELECT m.id FROM task_members m JOIN log_cleanup_groups g ON g.group_key=m.group_key
 UNION ALL SELECT t.id FROM tasks t JOIN log_cleanup_groups g ON g.group_key='hidden:'||t.id`); err != nil {
		return result, err
	}
	// Capture bound receipts before deleting operation metadata. A large intake
	// is one retention unit; the ordinary 500-source limit must not split it and
	// make its remaining receipts reappear as hundreds of unbound rows.
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO log_cleanup_sources(source_key,whole_group) SELECT e.source_key,1 FROM ingest_events e JOIN log_cleanup_groups g ON g.group_key=(`+logReceiptIntakeKey+`)`); err != nil {
		return result, err
	}
	removed, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id IN(SELECT id FROM log_cleanup_jobs)`)
	if err != nil {
		return result, err
	}
	result.Tasks, err = removed.RowsAffected()
	if err != nil {
		return result, err
	}
	// local_import_items has a restrictive foreign key; remove its expired audit
	// details only for the same fully closed import groups selected above.
	if _, err = tx.ExecContext(ctx, `DELETE FROM local_import_items WHERE batch_id IN(SELECT batch_id FROM log_cleanup_groups WHERE batch_id>0)`); err != nil {
		return result, err
	}
	removed, err = tx.ExecContext(ctx, `DELETE FROM local_import_batches WHERE id IN(SELECT batch_id FROM log_cleanup_groups WHERE batch_id>0)`)
	if err != nil {
		return result, err
	}
	result.ImportBatches, err = removed.RowsAffected()
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM task_operations WHERE MAX(created_at,updated_at)<:cutoff AND input_state NOT IN ('open','expired') AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j WHERE j.operation_id=task_operations.id)`, sql.Named("cutoff", result.Cutoff)); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO log_cleanup_sources(source_key) `+logCleanupSources, sql.Named("cutoff", result.Cutoff)); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_cleanup_sources WHERE whole_group=0`).Scan(&count); err != nil {
		return result, err
	}
	result.More = result.More || count > 500
	if count > 500 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM log_cleanup_sources WHERE whole_group=0 AND rowid NOT IN(SELECT rowid FROM log_cleanup_sources WHERE whole_group=0 ORDER BY rowid LIMIT 500)`); err != nil {
			return result, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_cleanup_sources WHERE source_key NOT LIKE 'verify:%'`).Scan(&result.Receipts); err != nil {
		return result, err
	}
	removed, err = tx.ExecContext(ctx, `DELETE FROM ingest_events WHERE source_key IN(SELECT source_key FROM log_cleanup_sources)`)
	if err != nil {
		return result, err
	}
	result.Events, err = removed.RowsAffected()
	if err != nil {
		return result, err
	}
	for _, table := range []string{"log_cleanup_jobs", "log_cleanup_sources", "log_cleanup_groups", "log_cleanup_references", "log_cleanup_active_files", "log_cleanup_recent_files", "log_cleanup_active_operations", "log_cleanup_protected_intakes"} {
		if _, err = tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
			return result, err
		}
	}
	return result, nil
}
