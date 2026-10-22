package dbx

import (
	"context"
	"errors"
	"testing"
)

func TestUnifiedLogsMergeOperationsReceiptsAndSearchBeforePaging(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "unified-upload", 2)
	f := telegramTestFile("timeline-upload", 0)
	f.Storage, f.SHA256, f.OperationID, f.OperationItemKey = "direct", f.ID, o.ID, "first-input"
	if err := d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	d.RecordTaskOperationItem(o.ID, AdminOwner, "rejected", "", "unique-rejected.heic", "failed", "unique-rejection-error")
	operationReceiptFixture(t, d, "timeline:rejected", "failed", "", "unique-rejected.heic", o.ID, 0)
	d.SealTaskOperation(o.ID, AdminOwner)
	d.Exec(`UPDATE tasks SET state='done',done_at=unixepoch() WHERE file_id=?`, f.ID)
	legacy := telegramTestFile("timeline-legacy", 0)
	legacy.Storage, legacy.SHA256 = "direct", legacy.ID
	if err := d.InsertUploadBatch([]*File{legacy}); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE tasks SET state='done',done_at=unixepoch() WHERE file_id=?`, legacy.ID)
	b, parent := importReceiptFixture(t, d, "/timeline-import", "done")
	operationReceiptFixture(t, d, "timeline:import", "accepted", "", "imported.jpg", 0, b)
	operationReceiptFixture(t, d, "timeline:unbound", "failed", "", "unbound-failure.xyz", 0, 0)
	rows, n, err := d.QueryUnifiedLogs(LogQuery{Limit: 50})
	if err != nil || n != 4 || len(rows) != 4 {
		t.Fatal(n, rows, err)
	}
	tasks, receipts := 0, 0
	for _, row := range rows {
		if row.Task != nil {
			tasks++
		} else {
			receipts++
		}
	}
	if tasks != 3 || receipts != 1 {
		t.Fatal("intake duplicated in timeline", tasks, receipts)
	}
	for _, query := range []LogQuery{{Search: "unique-rejected.heic"}, {Search: "unique-rejection-error"}, {FileID: f.ID}, {GroupID: o.TaskID}, {GroupID: parent}} {
		found, count, err := d.QueryUnifiedLogs(query)
		if err != nil || count != 1 || len(found) != 1 || found[0].Task == nil {
			t.Fatal(query, count, found, err)
		}
	}
	first, n, err := d.QueryUnifiedLogs(LogQuery{Limit: 2, Page: 1})
	second, _, secondErr := d.QueryUnifiedLogs(LogQuery{Limit: 2, Page: 2})
	if err != nil || secondErr != nil || n != 4 || len(first) != 2 || len(second) != 2 {
		t.Fatal(first, second, n, err, secondErr)
	}
	seen := map[string]bool{}
	for _, row := range append(first, second...) {
		if seen[row.RecordKey] {
			t.Fatal("pagination repeated row", row.RecordKey)
		}
		seen[row.RecordKey] = true
	}
}

func TestClearLogsPreservesRecoveryAndShowsSameSecondCompletionAndRetry(t *testing.T) {
	d := telegramTestDB(t)
	if err := d.CreateAccount("user:clear-logs", "clear-owner", "hash"); err != nil {
		t.Fatal(err)
	}
	f := telegramTestFile("clear-logs-original", 0)
	f.OwnerID, f.SHA256 = "user:clear-logs", f.ID
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	d.Exec(`INSERT INTO input_groups(id,owner_id,sha256,size,encrypted) VALUES('clear-dedup',?,?,1,0)`, f.OwnerID, f.SHA256)
	d.SetSetting("test.connection.encrypted", "opaque-secret")
	finished, _ := d.EnqueueTask(TaskCleanupTemp, "", "{}")
	d.FinishTask(finished, "{}")
	failed, _ := d.EnqueueTask(TaskVerify, f.ID, "{}")
	d.Exec(`UPDATE tasks SET state='failed',last_error='recoverable provider error' WHERE id=?`, failed)
	pending, _ := d.EnqueueTask(TaskVerify, f.ID, `{"second":true}`)
	running, _ := d.EnqueueTask(TaskVerify, f.ID, `{"third":true}`)
	d.Exec(`UPDATE tasks SET state='running',lease_until=? WHERE id=?`, now()+3600, running)
	at := now()
	result, err := d.ClearLogs(context.Background(), at)
	if err != nil || result.Hidden < 2 || result.RemovedTasks != 1 || result.RetainedActive != 2 || result.RetainedRecovery < 1 {
		t.Fatal(result, err)
	}
	if !hasTask(t, d, failed) || !hasTask(t, d, pending) || !hasTask(t, d, running) || hasTask(t, d, finished) {
		t.Fatal("clear changed operational work")
	}
	rows, n, err := d.QueryUnifiedLogs(LogQuery{})
	if err != nil || n != 0 || len(rows) != 0 {
		t.Fatal("old recovery logs not hidden, or active work hidden", n, rows, err)
	}
	if err = d.FinishTask(pending, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, count, err := d.QueryUnifiedLogs(LogQuery{}); err != nil || count != 1 {
		t.Fatal("completion did not publish a new log revision", count, err)
	}
	d.Exec(`UPDATE tasks SET updated_at=?,done_at=? WHERE id=?`, at, at, pending)
	if err = d.RetryTask(failed); err != nil {
		t.Fatal(err)
	}
	d.FinishTask(failed, "{}")
	if _, count, err := d.QueryUnifiedLogs(LogQuery{}); err != nil || count != 2 {
		t.Fatal("retry completion did not publish a new log revision", count, err)
	}
	d.Exec(`UPDATE tasks SET updated_at=?,done_at=? WHERE id=?`, at, at, failed)
	rows, n, err = d.QueryUnifiedLogs(LogQuery{})
	if err != nil || n != 2 || len(rows) != 2 {
		t.Fatal("same-second completion/retry disappeared", n, rows, err)
	}
	operationReceiptFixture(t, d, "after-clear-new-input", "failed", "", "new-rejection.xyz", 0, 0)
	if _, n, err = d.QueryUnifiedLogs(LogQuery{Search: "new-rejection.xyz"}); err != nil || n != 1 {
		t.Fatal("new receipt after clear hidden", n, err)
	}
	var count int
	d.QueryRow(`SELECT COUNT(*) FROM input_groups WHERE id='clear-dedup'`).Scan(&count)
	if count != 1 {
		t.Fatal("clear erased dedup identity")
	}
	got, err := d.GetFile(f.ID)
	if err != nil || got.OwnerID != f.OwnerID || got.SHA256 != f.SHA256 {
		t.Fatal("clear changed another account's original", got, err)
	}
	if value, err := d.GetSetting("test.connection.encrypted"); err != nil || value != "opaque-secret" {
		t.Fatal("clear changed connection setting", err)
	}
}

func TestClearLogsFailureRollsBackDeletionAndVisibility(t *testing.T) {
	d := telegramTestDB(t)
	id, _ := d.EnqueueTask(TaskCleanupTemp, "", "{}")
	d.FinishTask(id, "{}")
	beforeRevision, beforeAt, _ := d.LogVisibilityBoundary()
	if _, err := d.Exec(`CREATE TRIGGER fail_log_clear BEFORE UPDATE OF cleared_revision ON log_visibility BEGIN SELECT RAISE(ABORT,'fixture clear failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClearLogs(context.Background(), now()); err == nil {
		t.Fatal("expected clear failure")
	}
	if !hasTask(t, d, id) {
		t.Fatal("failed clear committed physical deletion")
	}
	if revision, at, err := d.LogVisibilityBoundary(); err != nil || revision != beforeRevision || at != beforeAt {
		t.Fatal("failed clear committed visibility", revision, at, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.ClearLogs(ctx, now()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled clear did not fail closed", err)
	}
	var integrity string
	d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Fatal(integrity)
	}
}

func TestClearLogsOpenOperationSameSecondSealBecomesNewHistory(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "clear-before-seal", 1)
	f := operationFixtureFile(t, d, "clear-open-original")
	id, err := d.EnqueueTask(TaskDirectArchive, f.ID, "{}", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.FinishTask(id, "{}")
	at := now()
	if _, err = d.ClearLogs(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if _, n, err := d.QueryUnifiedLogs(LogQuery{}); err != nil || n != 0 {
		t.Fatal("open intake leaked into history", n, err)
	}
	if rows, n, err := d.QueryUnifiedLogs(LogQuery{GroupID: o.TaskID}); err != nil || n != 1 || rows[0].Task.State != "pending" {
		t.Fatal("explicit active operation became inaccessible", rows, n, err)
	}
	if err = d.SealTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	rows, n, err := d.QueryUnifiedLogs(LogQuery{})
	if err != nil || n != 1 || rows[0].Task.State != "done" || rows[0].Task.Group.OperationID != o.ID {
		t.Fatal("seal itself did not publish a new log revision", rows, n, err)
	}
	d.Exec(`UPDATE tasks SET updated_at=?,done_at=? WHERE id=?`, at, at, o.TaskID)
	rows, n, err = d.QueryUnifiedLogs(LogQuery{})
	if err != nil || n != 1 || rows[0].Task.State != "done" || rows[0].Task.Group.OperationID != o.ID {
		t.Fatal("same-second seal did not become new history", rows, n, err)
	}
}

func TestUnifiedLogsKeepIntakeTimeIndependentFromLaterFileOperations(t *testing.T) {
	d := telegramTestDB(t)
	old := now() - 2*86400
	later := old + 86400
	upload := beginOperationFixture(t, d, "upload", "stable-upload-time", 1)
	f := telegramTestFile("stable-upload-original", 0)
	f.Storage, f.SHA256, f.OperationID, f.OperationItemKey = "direct", f.ID, upload.ID, "source-file"
	if err := d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE tasks SET state='done' WHERE file_id=?`, f.ID)
	d.SealTaskOperation(upload.ID, AdminOwner)
	d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=? WHERE id IN(SELECT task_id FROM task_operation_jobs WHERE operation_id=?)`, old, old, old, upload.ID)
	d.Exec(`UPDATE task_operations SET created_at=?,updated_at=? WHERE id=?`, old, old, upload.ID)
	d.Exec(`UPDATE ingest_events SET created_at=? WHERE file_id=?`, old, f.ID)
	d.Exec(`UPDATE files SET updated_at=? WHERE id=?`, old, f.ID)
	before, count, err := d.QueryUnifiedLogs(LogQuery{GroupID: upload.TaskID})
	if err != nil || count != 1 || before[0].Task.UpdatedAt != old {
		t.Fatal("fixture upload timestamp", before, count, err)
	}
	publish := beginOperationFixture(t, d, "publish", "independent-publish", 1)
	job, err := d.EnqueueTask(TaskPublish, f.ID, "{}", publish.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.FinishTask(job, "{}")
	d.SealTaskOperation(publish.ID, AdminOwner)
	d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=? WHERE id IN(SELECT task_id FROM task_operation_jobs WHERE operation_id=?)`, later, later, later, publish.ID)
	d.Exec(`UPDATE task_operations SET created_at=?,updated_at=? WHERE id=?`, later, later, publish.ID)
	// Publish, move and rename all update the file. Those changes belong to
	// their own operation and must not move an old intake to a newer day.
	d.Exec(`UPDATE files SET published=1,name='renamed-after-import',updated_at=? WHERE id=?`, later, f.ID)
	after, count, err := d.QueryUnifiedLogs(LogQuery{GroupID: upload.TaskID})
	if err != nil || count != 1 || after[0].Task.UpdatedAt != old || after[0].Task.DoneAt.Int64 != old {
		t.Fatal("later file change refreshed old intake", after, count, err)
	}
	all, count, err := d.QueryUnifiedLogs(LogQuery{})
	if err != nil || count != 2 || all[0].Task.ID != publish.TaskID || all[0].Task.UpdatedAt != later || all[1].Task.ID != upload.TaskID {
		t.Fatal("independent publish was not ordered separately", all, count, err)
	}
	// A standalone receipt can still report the current file state; its event
	// timestamp is independent. Keep the old record API's existing semantics.
	standalone := operationFixtureFile(t, d, "standalone-receipt-time")
	d.Exec(`UPDATE ingest_events SET created_at=? WHERE file_id=?`, old, standalone.ID)
	d.Exec(`UPDATE files SET published=1,updated_at=? WHERE id=?`, later, standalone.ID)
	rows, count, err := d.QueryUnifiedLogs(LogQuery{FileID: standalone.ID})
	if err != nil || count != 1 || rows[0].Receipt == nil || rows[0].Receipt.UpdatedAt != old || rows[0].Receipt.Outcome != "published" {
		t.Fatal("standalone receipt used mutable file time", rows, count, err)
	}
	legacy, count, err := d.QueryLogReceipts(LogQuery{FileID: standalone.ID})
	if err != nil || count != 1 || legacy[0].UpdatedAt != later {
		t.Fatal("legacy receipt contract changed", legacy, count, err)
	}
}
