package dbx

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func operationReceiptFixture(t *testing.T, d *DB, source, typ, fileID, name string, operationID, importID int64) {
	t.Helper()
	body := map[string]any{"orig_name": name, "source": "pimg"}
	if operationID != 0 {
		body["operation_id"] = operationID
	}
	if importID != 0 {
		body["telegram_source"] = fmt.Sprintf("local-import:%d:1", importID)
	}
	raw, _ := json.Marshal(body)
	if err := d.RecordIngestEvent(IngestEvent{SourceKey: source, Type: typ, FileID: fileID, Body: raw, Reason: "fixture detail"}); err != nil {
		t.Fatal(err)
	}
}

func importReceiptFixture(t *testing.T, d *DB, path string, states ...string) (int64, int64) {
	t.Helper()
	r, err := d.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule,state) VALUES(?,'[]',0,0,1,'now','done')`, path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.LastInsertId()
	parent, err := d.EnqueueTask(TaskLocalImportScan, "", fmt.Sprintf(`{"batch_id":%d}`, b))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(parent, "{}"); err != nil {
		t.Fatal(err)
	}
	for i, state := range states {
		if _, err = d.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,error) VALUES(?,?,1,'fixture',?,?)`, b, fmt.Sprintf("%s/item-%d.jpg", path, i), state, "import detail"); err != nil {
			t.Fatal(err)
		}
	}
	return b, parent
}

func TestGroupedLogReceiptsFollowInvocationAndPreserveFileDetails(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "receipt-upload", 3)
	f := operationFixtureFile(t, d, "receipt-original")
	id, _ := d.EnqueueTask(TaskDirectArchive, f.ID, "{}", o.ID)
	d.FinishTask(id, "{}")
	d.RecordTaskOperationItem(o.ID, AdminOwner, "duplicate", f.ID, "same-original.jpg", "skipped", "already exists")
	d.RecordTaskOperationItem(o.ID, AdminOwner, "rejected", "", "unsupported.xyz", "failed", "unsupported input")
	d.SealTaskOperation(o.ID, AdminOwner)
	operationReceiptFixture(t, d, "pimg:"+f.ID, "accepted", f.ID, f.OrigName, o.ID, 0)
	operationReceiptFixture(t, d, "upload:duplicate", "duplicate", f.ID, "same-original.jpg", o.ID, 0)
	operationReceiptFixture(t, d, "upload:rejected", "failed", "", "unsupported.xyz", o.ID, 0)
	b, parent := importReceiptFixture(t, d, "/folder-with-distinct-name", "done", "duplicate", "skipped", "failed")
	for i := 0; i < 4; i++ {
		operationReceiptFixture(t, d, fmt.Sprintf("import:%d", i), "accepted", "", fmt.Sprintf("import-file-%d.jpg", i), 0, b)
	}
	operationReceiptFixture(t, d, "legacy:standalone", "accepted", "", "legacy.jpg", 0, 0)
	rows, total, err := d.QueryGroupedLogReceipts(LogQuery{Limit: 50, Sort: "created_asc"})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatal(total, rows, err)
	}
	groups := map[int64]LogReceipt{}
	for _, row := range rows {
		if row.Group != nil {
			groups[row.Group.TaskID] = row
		}
	}
	upload := groups[o.TaskID]
	if upload.ID != -o.TaskID || upload.Group == nil || upload.Group.TotalFiles != 3 || upload.Group.FailedFiles != 1 || upload.Outcome != "failed" {
		t.Fatal("upload summary", upload)
	}
	imported := groups[parent]
	if imported.Group == nil || imported.Group.TotalFiles != 4 || imported.Group.ProcessedFiles != 4 || imported.Group.SucceededFiles != 1 || imported.Group.SkippedFiles != 2 || imported.Group.FailedFiles != 1 || imported.Group.BatchID != b {
		t.Fatal("import counters", imported)
	}
	if rows, n, err := d.QueryGroupedLogReceipts(LogQuery{FileID: f.ID}); err != nil || n != 2 || len(rows) != 2 || rows[0].Group != nil {
		t.Fatal("file detail was summarized", n, rows, err)
	}
	for _, query := range []LogQuery{{Search: "unsupported.xyz"}, {Type: "duplicate"}, {Search: "folder-with-distinct-name"}} {
		if rows, n, err := d.QueryGroupedLogReceipts(query); err != nil || n != 1 || len(rows) != 1 || rows[0].Group == nil {
			t.Fatal(query, n, rows, err)
		}
	}
	first, n, err := d.QueryGroupedLogReceipts(LogQuery{Limit: 2, Page: 1, Sort: "created_asc"})
	second, _, secondErr := d.QueryGroupedLogReceipts(LogQuery{Limit: 2, Page: 2, Sort: "created_asc"})
	if err != nil || secondErr != nil || n != 3 || len(first) != 2 || len(second) != 1 || first[0].RecordKey == second[0].RecordKey || first[1].RecordKey == second[0].RecordKey {
		t.Fatal(first, second, n, err, secondErr)
	}
}

func TestLogCleanupIntakeReceiptsAreWholeRetentionUnits(t *testing.T) {
	d := telegramTestDB(t)
	closed := beginOperationFixture(t, d, "upload", "large-old-receipt-group", 601)
	for i := 0; i < 601; i++ {
		key := fmt.Sprintf("old-input-%03d", i)
		if err := d.RecordTaskOperationItem(closed.ID, AdminOwner, key, "", key+".jpg", "done", ""); err != nil {
			t.Fatal(err)
		}
		operationReceiptFixture(t, d, key, "accepted", "", key+".jpg", closed.ID, 0)
	}
	if err := d.SealTaskOperation(closed.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	b, _ := importReceiptFixture(t, d, "/old-import", "done", "done")
	for i := 0; i < 2; i++ {
		operationReceiptFixture(t, d, fmt.Sprintf("old-import-%d", i), "accepted", "", "old-import.jpg", 0, b)
	}
	recent := beginOperationFixture(t, d, "upload", "protected-recent-receipt", 1)
	d.RecordTaskOperationItem(recent.ID, AdminOwner, "recent", "", "recent.jpg", "done", "")
	d.SealTaskOperation(recent.ID, AdminOwner)
	operationReceiptFixture(t, d, "recent-receipt", "accepted", "", "recent.jpg", recent.ID, 0)
	failed := beginOperationFixture(t, d, "upload", "protected-failed-receipt", 1)
	d.RecordTaskOperationItem(failed.ID, AdminOwner, "failed", "", "failed.jpg", "done", "")
	d.SealTaskOperation(failed.ID, AdminOwner)
	operationReceiptFixture(t, d, "failed-receipt", "failed", "", "failed.jpg", failed.ID, 0)
	old := now() - 40*86400
	d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=?`, old, old, old)
	d.Exec(`UPDATE task_operations SET created_at=?,updated_at=?`, old, old)
	d.Exec(`UPDATE task_operation_items SET created_at=?,updated_at=?`, old, old)
	d.Exec(`UPDATE local_import_batches SET created_at=?`, old)
	d.Exec(`UPDATE ingest_events SET created_at=?`, old)
	d.Exec(`UPDATE ingest_events SET created_at=? WHERE source_key='recent-receipt'`, now())
	result, err := d.CleanupLogs(context.Background(), now())
	if err != nil || result.Operations != 2 || result.Tasks != 2 || result.Receipts != 603 || result.Events != 603 || result.ImportBatches != 1 || result.More {
		t.Fatal(result, err)
	}
	rows, n, err := d.QueryGroupedLogReceipts(LogQuery{})
	if err != nil || n != 2 || len(rows) != 2 {
		t.Fatal("leftover per-file receipts escaped summary", n, rows, err)
	}
	for _, row := range rows {
		if row.Group == nil || row.Group.OperationID != recent.ID && row.Group.OperationID != failed.ID {
			t.Fatal("protected receipt lost operation", row)
		}
	}
	var leftovers int
	d.QueryRow(`SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?`, closed.ID).Scan(&leftovers)
	if leftovers != 0 {
		t.Fatal("cleaned operation kept child items", leftovers)
	}
}

func TestAutomaticEmptyTelegramScansOnlyLogFailuresAndExpire(t *testing.T) {
	d := telegramTestDB(t)
	old := now() - 40*86400
	quiet, _ := d.EnqueueTask(TaskTelegramScan, "tg-album:quiet", `{"album_id":1,"automatic":true}`)
	failed, _ := d.EnqueueTask(TaskTelegramScan, "tg-album:failed", `{"album_id":2,"automatic":true}`)
	d.Exec(`UPDATE tasks SET state='done',created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, quiet)
	d.Exec(`UPDATE tasks SET state='failed',last_error='provider failure',created_at=?,updated_at=? WHERE id=?`, old, old, failed)
	rows, n, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "all"})
	if err != nil || n != 1 || len(rows) != 1 || rows[0].ID != failed {
		t.Fatal(rows, n, err)
	}
	result, err := d.CleanupLogs(context.Background(), now())
	if err != nil || result.Tasks != 1 || result.Operations != 0 {
		t.Fatal(result, err)
	}
	if !hasTask(t, d, failed) || hasTask(t, d, quiet) {
		t.Fatal("hidden/failed scan retention is wrong")
	}
}
