package dbx

import (
	"context"
	"database/sql"
)

// A monotonically increasing revision distinguishes a new completion/retry from
// the result cleared earlier in the same second. Operational rows stay intact.
const logVisibilitySchema = `
CREATE TABLE log_visibility(
 id INTEGER PRIMARY KEY CHECK(id=1),
 revision INTEGER NOT NULL DEFAULT 0,
 cleared_revision INTEGER NOT NULL DEFAULT -1,
 cleared_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO log_visibility(id) VALUES(1);
CREATE TABLE log_row_revisions(
 record TEXT NOT NULL CHECK(record IN ('task','receipt')),
 record_id INTEGER NOT NULL,
 revision INTEGER NOT NULL,
 PRIMARY KEY(record,record_id)
);
INSERT INTO log_row_revisions(record,record_id,revision) SELECT 'task',id,0 FROM tasks;
INSERT INTO log_row_revisions(record,record_id,revision) SELECT 'receipt',id,0 FROM ingest_events;
CREATE TRIGGER log_task_insert AFTER INSERT ON tasks BEGIN
 UPDATE log_visibility SET revision=revision+1 WHERE id=1;
 INSERT INTO log_row_revisions VALUES('task',NEW.id,(SELECT revision FROM log_visibility WHERE id=1));
END;
CREATE TRIGGER log_task_update AFTER UPDATE ON tasks BEGIN
 UPDATE log_visibility SET revision=revision+1 WHERE id=1;
 INSERT INTO log_row_revisions VALUES('task',NEW.id,(SELECT revision FROM log_visibility WHERE id=1))
 ON CONFLICT(record,record_id) DO UPDATE SET revision=excluded.revision;
END;
CREATE TRIGGER log_task_delete AFTER DELETE ON tasks BEGIN
 DELETE FROM log_row_revisions WHERE record='task' AND record_id=OLD.id;
END;
CREATE TRIGGER log_receipt_insert AFTER INSERT ON ingest_events BEGIN
 UPDATE log_visibility SET revision=revision+1 WHERE id=1;
 INSERT INTO log_row_revisions VALUES('receipt',NEW.id,(SELECT revision FROM log_visibility WHERE id=1));
END;
CREATE TRIGGER log_receipt_update AFTER UPDATE ON ingest_events BEGIN
 UPDATE log_visibility SET revision=revision+1 WHERE id=1;
 INSERT INTO log_row_revisions VALUES('receipt',NEW.id,(SELECT revision FROM log_visibility WHERE id=1))
 ON CONFLICT(record,record_id) DO UPDATE SET revision=excluded.revision;
END;
CREATE TRIGGER log_receipt_delete AFTER DELETE ON ingest_events BEGIN
 DELETE FROM log_row_revisions WHERE record='receipt' AND record_id=OLD.id;
END;
`

type ClearLogsResult struct {
	Hidden           int64 `json:"hidden"`
	RemovedTasks     int64 `json:"removed_tasks"`
	RemovedEvents    int64 `json:"removed_events"`
	RetainedActive   int64 `json:"retained_active"`
	RetainedRecovery int64 `json:"retained_recovery"`
	More             bool  `json:"more"`
	ClearedAt        int64 `json:"cleared_at"`
	Revision         int64 `json:"revision"`
}

// Clear removes a bounded set of dispensable history and records the same
// transaction's visibility boundary. Failed, active and referenced work is
// preserved; a later update receives a larger revision and becomes visible.
func (db *DB) ClearLogs(ctx context.Context, at int64) (result ClearLogsResult, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE log_visibility SET revision=revision WHERE id=1`); err != nil {
		return result, err
	}
	if err = expireTaskOperations(tx, at); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, unifiedLogsCTE+` SELECT COUNT(*) FROM visible_logs WHERE is_active=0`).Scan(&result.Hidden); err != nil {
		return result, err
	}
	cleaned, err := cleanupLogsTx(ctx, tx, at, true)
	if err != nil {
		return result, err
	}
	result.RemovedTasks, result.RemovedEvents, result.More = cleaned.Tasks, cleaned.Events, cleaned.More
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM log_visibility WHERE id=1`).Scan(&result.Revision); err != nil {
		return result, err
	}
	result.ClearedAt = at
	if _, err = tx.ExecContext(ctx, `UPDATE log_visibility SET cleared_revision=?,cleared_at=? WHERE id=1`, result.Revision, at); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, unifiedLogsCTE+` SELECT COUNT(*) FROM visible_logs WHERE is_active=1`).Scan(&result.RetainedActive); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks t WHERE (t.state='failed' AND t.type!='system_schedule')
 OR EXISTS(SELECT 1 FROM file_conversions c WHERE c.task_id=t.id)
 OR EXISTS(SELECT 1 FROM cold_sync_files c WHERE c.task_id=t.id)
 OR EXISTS(SELECT 1 FROM album_topics a WHERE a.scan_task_id=t.id)
 OR EXISTS(SELECT 1 FROM system_schedules s WHERE s.run_task_id=t.id)
 OR (t.type='archive_sync' AND t.state='canceled' AND EXISTS(SELECT 1 FROM files f WHERE f.id=t.file_id))`).Scan(&result.RetainedRecovery); err != nil {
		return result, err
	}
	err = tx.Commit()
	return result, err
}

// Kept separate to let callers inspect a committed clear without exposing the
// global settings or any credentials through the log API.
func (db *DB) LogVisibilityBoundary() (revision, clearedAt int64, err error) {
	err = db.QueryRow(`SELECT cleared_revision,cleared_at FROM log_visibility WHERE id=1`).Scan(&revision, &clearedAt)
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}
