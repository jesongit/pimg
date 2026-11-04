package dbx

import "context"

// CompactRedundantDoneTasks bounds task history: for finished sync tasks keep
// only the newest keepPerFile rows per file and drop the rest together with
// their operation jobs, row revisions and empty sync operations. A runaway
// reconcile loop can otherwise add hundreds of thousands of rows, and every
// history query then aggregates them (10s+ page loads).
func (db *DB) CompactRedundantDoneTasks(ctx context.Context, keepPerFile int) (int64, error) {
	if keepPerFile < 1 {
		keepPerFile = 5
	}
	res, err := db.ExecContext(ctx, `
WITH ranked AS (
  SELECT id, ROW_NUMBER() OVER (PARTITION BY file_id, type ORDER BY id DESC) rn
  FROM tasks WHERE state='done' AND type IN ('archive_sync','cold_sync')
)
DELETE FROM tasks WHERE id IN (SELECT id FROM ranked WHERE rn > ?)`, keepPerFile)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if _, err = db.ExecContext(ctx, `DELETE FROM task_operation_jobs WHERE task_id NOT IN (SELECT id FROM tasks)`); err != nil {
		return removed, err
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM log_row_revisions WHERE record='task' AND record_id NOT IN (SELECT id FROM tasks)`); err != nil {
		return removed, err
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM task_operations WHERE kind='sync' AND id NOT IN (SELECT operation_id FROM task_operation_jobs)`); err != nil {
		return removed, err
	}
	return removed, nil
}
