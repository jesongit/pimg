package dbx

import (
	"database/sql"
	"fmt"
	"testing"
)

func TestUploadWaveHistoryAndAccountBoundaries(t *testing.T) {
	d := telegramTestDB(t)
	for i := 0; i < 25; i++ {
		f := telegramTestFile(fmt.Sprintf("upload-%d", i), 0)
		f.SHA256 = f.ID
		f.ArchiveState = "pending"
		if err := d.InsertUploadBatch([]*File{f}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := d.QueryActivityTasks(LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Group.Steps != 25 || rows[0].FileID.Valid {
		t.Fatal(rows, total, err)
	}
	root := rows[0].ID
	if _, err = d.Exec(`UPDATE tasks SET state='done' WHERE type IN ('archive','direct_archive')`); err != nil {
		t.Fatal(err)
	}
	rows, total, err = d.QueryActivityTasks(LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 0 {
		t.Fatal(rows, total, err)
	}
	rows, total, err = d.QueryActivityTasks(LogQuery{Grouped: true, State: "terminal"})
	if err != nil || total != 1 || rows[0].ID != root || rows[0].State != "done" {
		t.Fatal(rows, total, err)
	}
	// Terminal failure belongs to history; it cannot keep a later upload wave open.
	if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, root); err != nil {
		t.Fatal(err)
	}
	f := telegramTestFile("upload-new", 0)
	f.SHA256 = f.ID
	if err = d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	rows, total, err = d.QueryActivityTasks(LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 1 || rows[0].ID == root || rows[0].Group.Steps != 1 {
		t.Fatal(rows, total, err)
	}
	if err = d.CreateAccount("user:upload-other", "upload-other", "x"); err != nil {
		t.Fatal(err)
	}
	f = telegramTestFile("other-upload", 0)
	f.SHA256 = f.ID
	f.OwnerID = "user:upload-other"
	f.AlbumID = sql.NullInt64{}
	if err = d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	rows, total, err = d.QueryActivityTasks(LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 2 {
		t.Fatal(rows, total, err)
	}
	counts, err := d.ActivityTaskCounts("")
	if err != nil || counts.Pending != 2 || counts.Failed != 0 {
		t.Fatal(counts, err)
	}
}

func TestTaskOperationUpgradeBackfillsActiveWavesWithoutChangingJobs(t *testing.T) {
	version := -1
	for i, migration := range migrations {
		if migration == taskOperationSchema {
			version = i
			break
		}
	}
	if version < 0 {
		t.Fatal("operation migration not found")
	}
	path, old := oldUpgradeDB(t, version)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("legacy-%d", i)
		if _, err := old.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at) VALUES(?,?,'old.jpg','jpg','image/jpeg',3,?,'image',X'01',X'02','pending',1,2)`, id, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('archive',?,'{}','pending',3,4,5)`, id); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, total, err := d.QueryActivityTasks(LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 1 || rows[0].Group.Steps != 12 {
		t.Fatal(rows, total, err)
	}
	var jobs int
	if err = d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive' AND state='pending' AND payload='{}' AND run_after=3 AND created_at=4 AND updated_at=5`).Scan(&jobs); err != nil || jobs != 12 {
		t.Fatal("worker jobs changed", jobs, err)
	}
}

func TestImportTaskGroupProgressCancellationAndPagination(t *testing.T) {
	d := telegramTestDB(t)
	r, err := d.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule,state) VALUES('/fixture','[]',0,0,1,'now','running')`)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := r.LastInsertId()
	parent, err := d.EnqueueTask(TaskLocalImportScan, "", fmt.Sprintf(`{"batch_id":%d}`, batch))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("batch-file-%d", i)
		f := telegramTestFile(id, 0)
		f.ArchiveState = "pending"
		if err = d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		_, err = d.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,file_ids) VALUES(?,?,1,'v','archiving',json_array(?))`, batch, id, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.EnqueueTask(TaskArchive, id, "{}"); err != nil {
			t.Fatal(err)
		}
		if i < 30 {
			if _, err = d.Exec(`UPDATE files SET archive_state='done' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, total, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "all", Limit: 50})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatal(total, err)
	}
	g := rows[0]
	if g.ID != parent || g.Group == nil || g.Group.Members != 121 || g.Group.ProcessedFiles != 30 || g.Group.TotalFiles != 120 {
		t.Fatalf("bad group %+v %+v", g, g.Group)
	}
	children, n, err := d.GroupedTaskMembers(parent, 2, 50)
	if err != nil || n != 121 || len(children) != 50 {
		t.Fatal(n, len(children), err)
	}
	_, affected, err := d.CancelTaskGroup(parent, true)
	if err != nil || affected != 1 {
		t.Fatal(affected, err)
	}
	var pending int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive' AND state='pending'`).Scan(&pending)
	if pending != 120 {
		t.Fatal("accepted archives canceled", pending)
	}
}

func TestSyncWaveIsOneCancelableTaskAndCancelDoesNotRequeue(t *testing.T) {
	d := telegramTestDB(t)
	for i := 0; i < 25; i++ {
		f := telegramTestFile(fmt.Sprintf("sync-%d", i), 0)
		if err := d.InsertFile(f); err != nil {
			t.Fatal(err)
		}
		if _, err := d.EnqueueUniqueTask(TaskArchiveSync, f.ID, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "all"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Type != "sync_group" {
		t.Fatal(rows, total, err)
	}
	root := rows[0].ID
	if rows[0].Group == nil || rows[0].Group.Steps != 25 {
		t.Fatal(rows[0])
	}
	_, count, err := d.CancelTaskGroup(root, false)
	if err != nil || count != 25 {
		t.Fatal(count, err)
	}
	if _, err = d.EnqueueUniqueTask(TaskArchiveSync, "sync-1", "{}"); err != nil {
		t.Fatal(err)
	}
	var pending int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='archive_sync' AND state='pending'`).Scan(&pending)
	if pending != 0 {
		t.Fatal("canceled work recreated", pending)
	}
	counts, err := d.ActivityTaskCounts("")
	if err != nil || counts.Pending != 0 || counts.Running != 0 {
		t.Fatal(counts, err)
	}
}
