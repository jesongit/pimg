package dbx

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func agedTask(t *testing.T, d *DB, typ, file, state string, at int64) int64 {
	t.Helper()
	id, err := d.EnqueueTask(typ, file, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state=?,created_at=?,updated_at=?,done_at=? WHERE id=?`, state, at, at, at, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func hasTask(t *testing.T, d *DB, id int64) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestLogCleanupDefaultCutoffAndRecoverableStates(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 31*86400
	remove := []int64{agedTask(t, d, TaskCleanupTemp, "", "done", old), agedTask(t, d, TaskCleanupTemp, "", "canceled", old)}
	keep := []int64{}
	for _, state := range []string{"pending", "running", "failed"} {
		keep = append(keep, agedTask(t, d, TaskCleanupTemp, "", state, old))
	}
	keep = append(keep, agedTask(t, d, TaskCleanupTemp, "", "done", now-29*86400), agedTask(t, d, TaskCleanupTemp, "", "done", now-30*86400))
	id := agedTask(t, d, TaskCleanupTemp, "", "canceled", old)
	keep = append(keep, id)
	d.Exec(`UPDATE tasks SET updated_at=? WHERE id=?`, now, id)
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.RetentionDays != 30 || r.Tasks != 2 || r.Operations != 2 {
		t.Fatal(r, err)
	}
	for _, id := range remove {
		if hasTask(t, d, id) {
			t.Fatal("expired terminal job retained", id)
		}
	}
	for _, id := range keep {
		if !hasTask(t, d, id) {
			t.Fatal("recoverable or recent job removed", id)
		}
	}
	if err = d.SetSetting(logRetentionKey, "0"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CleanupLogs(context.Background(), now); err == nil {
		t.Fatal("invalid saved retention must fail closed")
	}
	for _, id := range keep {
		if !hasTask(t, d, id) {
			t.Fatal("failed cleanup mutated jobs", id)
		}
	}
}

func TestLogCleanupPreservesDependenciesAndCancellationTombstones(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	for _, id := range []string{"canceled-sync", "conversion-source", "conversion-target", "legacy-import", "moving", "archive-pending"} {
		f := telegramTestFile(id, 0)
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
	}
	var keep []int64
	id := agedTask(t, d, TaskArchiveSync, "canceled-sync", "canceled", old)
	keep = append(keep, id)
	d.Exec(`UPDATE tasks SET cancel_requested=1 WHERE id=?`, id)
	if _, err := d.EnqueueUniqueTask(TaskArchiveSync, "canceled-sync", "{}"); err != nil {
		t.Fatal(err)
	}
	id = agedTask(t, d, TaskConvert, "conversion-source", "done", old)
	keep = append(keep, id)
	if _, err := d.Exec(`INSERT INTO file_conversions(source_id,target_id,target_encrypted,phase,task_id,created_at,updated_at) VALUES('conversion-source','conversion-target',0,'done',?,?,?)`, id, old, old); err != nil {
		t.Fatal(err)
	}
	id = agedTask(t, d, TaskTelegramScan, "tg-album:1234", "done", old)
	keep = append(keep, id)
	if _, err := d.Exec(`INSERT INTO album_topics(album_id,account_id,chat_id,name,create_name,scan_task_id,created_at,updated_at) VALUES(1234,1,1,'scan','scan',?,?,?)`, id, old, old); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, agedTask(t, d, TaskImport, "legacy-import", "done", old))
	id = agedTask(t, d, TaskArchive, "archive-pending", "canceled", old)
	keep = append(keep, id)
	if _, err := d.Exec(`UPDATE files SET archive_state='pending' WHERE id='archive-pending'`); err != nil {
		t.Fatal(err)
	}
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.Tasks != 0 {
		t.Fatal(r, err)
	}
	for _, id := range keep {
		if !hasTask(t, d, id) {
			t.Fatal("dependency removed", id)
		}
	}
	before := keep[0]
	got, err := d.EnqueueUniqueTask(TaskArchiveSync, "canceled-sync", "{}")
	if err != nil || got != before {
		t.Fatal("canceled sync resurrected", got, before, err)
	}
	var integrity string
	d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Fatal(integrity)
	}
}

func TestLogCleanupWholeImportGroupsAndIntakeHistory(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	createBatch := func(state string) (int64, int64) {
		t.Helper()
		r, err := d.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule,state,created_at) VALUES('/old','[]',0,0,1,'now',?,?)`, state, old)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := r.LastInsertId()
		id, err := d.EnqueueTask(TaskLocalImportScan, "", fmt.Sprintf(`{"batch_id":%d}`, b))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET state='done',created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, id); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state) VALUES(?,'old.jpg',1,'v','done')`, b); err != nil {
			t.Fatal(err)
		}
		return b, id
	}
	closed, closedJob := createBatch("done")
	_, pausedJob := createBatch("paused")
	_, failedJob := createBatch("done_errors")
	_, readyJob := createBatch("ready")
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.ImportBatches != 1 || r.Tasks != 1 || hasTask(t, d, closedJob) {
		t.Fatal(r, err)
	}
	for _, id := range []int64{pausedJob, failedJob, readyJob} {
		if !hasTask(t, d, id) {
			t.Fatal("batch recovery removed", id)
		}
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM local_import_items WHERE batch_id=?`, closed).Scan(&n)
	if n != 0 {
		t.Fatal("expired batch audit items retained", n)
	}
}

func TestLogCleanupReceiptsKeepFailuresRecentVerificationAndSourceIdentity(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	f := telegramTestFile("receipt-log-cleanup", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM ingest_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE files SET created_at=?,updated_at=? WHERE id=?`, old, old, f.ID); err != nil {
		t.Fatal(err)
	}
	add := func(source, typ, file string, at int64) {
		t.Helper()
		if _, err := d.Exec(`INSERT INTO ingest_events(source_key,event_type,file_id,body,created_at) VALUES(?,?,?,'{}',?)`, source, typ, file, at); err != nil {
			t.Fatal(err)
		}
	}
	add("old-accepted", "accepted", f.ID, old)
	add("old-accepted", "verified", f.ID, old)
	add("old-failure", "failed", "", old)
	add("recent", "accepted", "", now)
	if _, err := d.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES('stable-source',0,?)`, f.ID); err != nil {
		t.Fatal(err)
	}
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.Receipts != 1 || r.Events != 2 {
		t.Fatal(r, err)
	}
	if _, err = d.GetFile(f.ID); err != nil {
		t.Fatal("original record deleted", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM telegram_source_parts WHERE source_key='stable-source'`).Scan(&n)
	if n != 1 {
		t.Fatal("dedup source identity lost")
	}
	add("verified-file", "accepted", f.ID, old)
	add("verify:"+f.ID, "verified", f.ID, now)
	r, err = d.CleanupLogs(context.Background(), now)
	if err != nil || r.Events != 0 {
		t.Fatal("recent verification or failure removed", r, err)
	}
}

func TestLogCleanupBoundedPassesAndWholeArchiveWave(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	for i := 0; i < 105; i++ {
		agedTask(t, d, TaskCleanupTemp, "", "done", old)
	}
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || !r.More || r.Tasks != 100 {
		t.Fatal(r, err)
	}
	r, err = d.CleanupLogs(context.Background(), now)
	if err != nil || r.More || r.Tasks != 5 {
		t.Fatal(r, err)
	}
	for i := 0; i < 125; i++ {
		f := telegramTestFile(fmt.Sprintf("wave-log-%d", i), 0)
		if err = d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		agedTask(t, d, TaskArchive, f.ID, "pending", old)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='done'; UPDATE task_operations SET created_at=?`, old); err != nil {
		t.Fatal(err)
	}
	r, err = d.CleanupLogs(context.Background(), now)
	if err != nil || r.Tasks != 125 || r.Operations != 1 || r.More {
		t.Fatal("wave was split", r, err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM task_operations`).Scan(&n)
	if n != 0 {
		t.Fatal("empty operation retained", n)
	}
}

func TestLogCleanupKeepsWholeActiveWaveAndColdSyncReference(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	var ids []int64
	for _, name := range []string{"active-group-a", "active-group-b"} {
		f := telegramTestFile(name, 0)
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, agedTask(t, d, TaskArchive, name, "pending", old))
	}
	d.Exec(`UPDATE tasks SET state='done' WHERE id=?`, ids[0])
	for _, state := range []string{"pending", "running", "failed"} {
		d.Exec(`UPDATE tasks SET state=? WHERE id=?`, state, ids[1])
		r, err := d.CleanupLogs(context.Background(), now)
		if err != nil || r.Tasks != 0 || !hasTask(t, d, ids[0]) || !hasTask(t, d, ids[1]) {
			t.Fatal("active wave partially cleaned", r, err)
		}
	}
	repo, err := d.AddColdRepository("recovery", "local", "{}")
	if err != nil {
		t.Fatal(err)
	}
	referenced := agedTask(t, d, TaskCleanupTemp, "active-group-a", "done", old)
	if _, err = d.Exec(`INSERT INTO cold_sync_files(repository_id,file_id,wanted_revision,task_id) VALUES(?,'active-group-a',1,?)`, repo, referenced); err != nil {
		t.Fatal(err)
	}
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.Tasks != 0 || !hasTask(t, d, referenced) {
		t.Fatal("cold synchronization recovery reference removed", r, err)
	}
}

func TestLogCleanupReceiptBacklogAndCanceledRequest(t *testing.T) {
	d := telegramTestDB(t)
	now := time.Now().Unix()
	old := now - 40*86400
	for i := 0; i < 505; i++ {
		if _, err := d.Exec(`INSERT INTO ingest_events(source_key,event_type,body,created_at) VALUES(?,'accepted','{}',?)`, fmt.Sprintf("old-log-%d", i), old); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.CleanupLogs(ctx, now); err == nil {
		t.Fatal("canceled request proceeded")
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM ingest_events`).Scan(&n)
	if n != 505 {
		t.Fatal("canceled cleanup deleted history", n)
	}
	r, err := d.CleanupLogs(context.Background(), now)
	if err != nil || r.Events != 500 || r.Receipts != 500 || !r.More {
		t.Fatal(r, err)
	}
	r, err = d.CleanupLogs(context.Background(), now)
	if err != nil || r.Events != 5 || r.More {
		t.Fatal(r, err)
	}
}
