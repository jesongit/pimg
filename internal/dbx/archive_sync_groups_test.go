package dbx

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func syncImportFixture(t *testing.T, d *DB, files ...*File) (int64, int64, []int64) {
	t.Helper()
	r, err := d.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule,state) VALUES('/fixture','[]',0,0,1,'now','running')`)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := r.LastInsertId()
	parent, err := d.EnqueueTask(TaskLocalImport, "", TaskPayloadJSON(map[string]any{"batch_id": batch}))
	if err != nil {
		t.Fatal(err)
	}
	var archives []int64
	for _, f := range files {
		if err = d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,file_ids) VALUES(?,?,1,'v','archiving',json_array(?))`, batch, f.ID, f.ID); err != nil {
			t.Fatal(err)
		}
		archive, err := d.EnqueueTask(TaskArchive, f.ID, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		archives = append(archives, archive)
	}
	return batch, parent, archives
}

func TestImportFirstArchiveCompletesIndependentlyOfReplicaSync(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("import-split-progress", 0)
	f.ArchiveState = "pending"
	batch, parent, archives := syncImportFixture(t, d, f)
	before, err := d.GetTaskGroup(parent)
	if err != nil || before.Group.TotalFiles != 1 || before.Group.ProcessedFiles != 0 || before.State != "pending" {
		t.Fatal("mere acceptance counted as archived", before, err)
	}
	if err = d.SetArchiveState(f.ID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(archives[0], ""); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE local_import_items SET state='done' WHERE batch_id=?`, batch); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE local_import_batches SET state='done' WHERE id=?`, batch); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(parent, ""); err != nil {
		t.Fatal(err)
	}
	syncID, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, syncState := range []string{"pending", "running", "failed", "done"} {
		if _, err = d.Exec(`UPDATE tasks SET state=? WHERE id=?`, syncState, syncID); err != nil {
			t.Fatal(err)
		}
		got, err := d.GetTaskGroup(parent)
		if err != nil || got.State != "done" || got.Group.Kind != "import" || got.Group.TotalFiles != 1 || got.Group.ProcessedFiles != 1 || got.Group.Members != 2 {
			t.Fatal("replica status delayed completed import", syncState, got, err)
		}
		independent, err := d.GetTaskGroup(syncID)
		if err != nil || independent.Type != "sync_group" || independent.State != syncState || independent.Group.TotalFiles != 1 || independent.Group.Members != 1 {
			t.Fatal("replica progress was not independently visible", syncState, independent, err)
		}
	}
	groups, n, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "all"})
	if err != nil || n != 2 || len(groups) != 2 {
		t.Fatal("expected exactly import and replica sync operations", n, groups, err)
	}
}

func TestImportCancelDoesNotCancelIndependentReplicaSync(t *testing.T) {
	d := telegramTestDB(t)
	a, b := telegramTestFile("cancel-import-archived", 0), telegramTestFile("cancel-import-pending", 0)
	b.ArchiveState = "pending"
	batch, parent, _ := syncImportFixture(t, d, a, b)
	syncID, err := d.EnqueueUniqueTask(TaskArchiveSync, a.ID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE local_import_batches SET state='canceled' WHERE id=?`, batch); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.CancelTaskGroup(parent, true); err != nil {
		t.Fatal(err)
	}
	job, _ := d.GetTask(syncID)
	if job.State != "pending" {
		t.Fatal("canceling intake canceled independent replication", job)
	}
	var archivePending int
	if err = d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive' AND state='pending'`).Scan(&archivePending); err != nil || archivePending != 2 {
		t.Fatal("already accepted first copies were canceled", archivePending, err)
	}
}

func TestUploadReplicaSyncHasStableSeparateWaveAcrossFastWorkersAndStores(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "upload", "album:0;encrypted:false", "upload-and-replicate", 2)
	files := []*File{telegramTestFile("upload-sync-a", 0), telegramTestFile("upload-sync-b", 0)}
	var syncRoot, syncOperation int64
	for i, f := range files {
		f.ArchiveState, f.Storage, f.SHA256 = "pending", "direct", f.ID
		f.OperationID, f.OperationItemKey = op.ID, f.ID
		if err := d.InsertUploadBatch([]*File{f}); err != nil {
			t.Fatal(err)
		}
		if err := d.SetArchiveState(f.ID, "done", ""); err != nil {
			t.Fatal(err)
		}
		archive, err := d.FindActiveTask(TaskDirectArchive, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.FinishTask(archive.ID, ""); err != nil {
			t.Fatal(err)
		}
		syncID, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := followupOperation(t, d, syncID)
		if i == 0 {
			syncRoot, syncOperation = syncID, got
		} else if got != syncOperation {
			t.Fatal("fast completion split one upload's synchronization", got, syncOperation)
		}
		if got == op.ID {
			t.Fatal("replication remained inside upload operation")
		}
		if err = d.FinishTask(syncID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SealTaskOperation(op.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		repo, err := d.AddColdRepository(fmt.Sprintf("replica-%d", i), "local", TaskPayloadJSON(map[string]any{"directory": fmt.Sprintf("replica-%d", i)}))
		if err != nil {
			t.Fatal(err)
		}
		if err = d.SetColdRepositoryEnabled(repo, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	rows, n, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "all"})
	if err != nil || n != 2 || len(rows) != 2 {
		t.Fatal("stores appeared as independent per-file/warehouse tasks", n, rows, err)
	}
	upload, err := d.GetTaskOperation(op.ID, AdminOwner)
	if err != nil || upload.State != "done" || upload.Group.ProcessedFiles != 2 || upload.Group.TotalFiles != 2 || upload.Group.Steps != 2 {
		t.Fatal("secondary stores delayed upload completion", upload, err)
	}
	syncGroup, err := d.GetTaskGroup(syncRoot)
	if err != nil || syncGroup.State != "pending" || syncGroup.Group.Kind != "sync" || syncGroup.Group.TotalFiles != 2 || syncGroup.Group.ProcessedFiles != 0 || syncGroup.Group.Steps != 6 {
		t.Fatal("replica progress did not cover all stores independently", syncGroup, err)
	}
}

func TestCanceledIntakeSyncWaveRetainsLaterFileTombstones(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "upload", "album:0;encrypted:false", "cancel-replication", 2)
	var syncOperation, firstSync int64
	for i, id := range []string{"cancel-sync-a", "cancel-sync-b"} {
		f := telegramTestFile(id, 0)
		f.Storage, f.SHA256, f.OperationID, f.OperationItemKey = "direct", id, op.ID, id
		if err := d.InsertUploadBatch([]*File{f}); err != nil {
			t.Fatal(err)
		}
		syncID, err := d.EnqueueUniqueTask(TaskArchiveSync, id, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := followupOperation(t, d, syncID)
		if i == 0 {
			firstSync, syncOperation = syncID, got
			if _, _, err = d.CancelTaskGroup(firstSync, false); err != nil {
				t.Fatal(err)
			}
		} else {
			job, _ := d.GetTask(syncID)
			if got != syncOperation || job.State != "canceled" {
				t.Fatal("later file escaped canceled replication boundary", got, syncOperation, job)
			}
			again, err := d.EnqueueUniqueTask(TaskArchiveSync, id, `{}`)
			if err != nil || again != syncID {
				t.Fatal("reconciliation resurrected canceled sync", again, syncID, err)
			}
		}
	}
	upload, err := d.GetTaskOperation(op.ID, AdminOwner)
	if err != nil || upload.CancelRequested {
		t.Fatal("sync cancellation altered upload boundary", upload, err)
	}
}

func TestLaterMetadataSyncDoesNotReopenOriginalIntakeSyncOperation(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "upload", "album:0;encrypted:false", "subsequent-sync", 1)
	f := telegramTestFile("subsequent-file", 0)
	f.OperationID, f.OperationItemKey, f.SHA256 = op.ID, f.ID, f.ID
	if err := d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	first, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(first, ""); err != nil {
		t.Fatal(err)
	}
	second, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`)
	if err != nil || second == first {
		t.Fatal(second, first, err)
	}
	a, _ := followupOperation(t, d, first)
	b, _ := followupOperation(t, d, second)
	if a == b {
		t.Fatal("later resynchronization reopened old intake wave", a, b)
	}
}

func TestCanceledSourceSyncTombstoneSurvivesLogRetention(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "upload", "album:0;encrypted:false", "retained-sync-boundary", 1)
	f := telegramTestFile("retained-sync-file", 0)
	f.Storage, f.SHA256, f.OperationID, f.OperationItemKey = "direct", f.ID, op.ID, f.ID
	if err := d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	archive, err := d.FindActiveTask(TaskDirectArchive, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(archive.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err = d.SealTaskOperation(op.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	syncID, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.CancelTaskGroup(syncID, false); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Unix() - 60*86400
	if _, err = d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=?`, old, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE task_operations SET created_at=?,updated_at=?`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE task_operation_items SET created_at=?,updated_at=?`, old, old); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE state IN ('pending','running','failed')`).Scan(&active); err != nil || active != 0 {
		t.Fatal("fixture has an unrelated recovery protection", active, err)
	}
	if _, err = d.CleanupLogs(context.Background(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	retained, err := d.GetTask(syncID)
	if err != nil || retained.State != "canceled" {
		t.Fatal("retention removed live sync cancellation tombstone", retained, err)
	}
	if next, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, `{}`); err != nil || next != syncID {
		t.Fatal("cleanup permitted canceled synchronization to regenerate", next, syncID, err)
	}
}
