package dbx

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func operationFixtureFile(t *testing.T, d *DB, id string) *File {
	t.Helper()
	f := telegramTestFile(id, 0)
	f.Storage = "direct"
	f.SHA256 = id
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	return f
}

func beginOperationFixture(t *testing.T, d *DB, kind, key string, total int) *TaskOperation {
	t.Helper()
	o, err := d.BeginTaskOperation(AdminOwner, kind, key, "fixture", total)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestExplicitOperationKeeps201FileIntakeAcrossFastHTTPChunks(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "publish", "one-selection", 201)
	for i := 0; i < 201; i++ {
		if i == 200 {
			again := beginOperationFixture(t, d, "publish", "one-selection", 201)
			if again.ID != o.ID || again.TaskID != o.TaskID {
				t.Fatal("HTTP chunk changed operation", again, o)
			}
		}
		f := operationFixtureFile(t, d, fmt.Sprintf("selected-%03d", i))
		id, err := d.EnqueueTask(TaskPublish, f.ID, "{}", o.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.FinishTask(id, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	items, n, err := d.QueryActivityTasks(LogQuery{Grouped: true, View: "current"})
	if err != nil || n != 1 || len(items) != 1 || items[0].ID != o.TaskID || items[0].State != "pending" || items[0].Group.ProcessedFiles != 201 {
		t.Fatal("open intake disappeared after fast workers", n, items, err)
	}
	if err = d.SealTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	group, err := d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "done" || group.Group.TotalFiles != 201 || group.Group.SucceededFiles != 201 || group.Group.Steps != 201 {
		t.Fatal(group, err)
	}
	if _, n, err = d.QueryActivityTasks(LogQuery{Grouped: true, View: "current"}); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestExplicitOperationRequestIdentityAndOwnership(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "publish", "first", 1)
	second := beginOperationFixture(t, d, "publish", "second", 1)
	if o.ID == second.ID {
		t.Fatal("concurrent clicks merged")
	}
	for _, attempt := range []struct {
		kind, scope string
		total       int
	}{{"purge", "fixture", 1}, {"publish", "other", 1}, {"publish", "fixture", 2}} {
		if _, err := d.BeginTaskOperation(AdminOwner, attempt.kind, "first", attempt.scope, attempt.total); !errors.Is(err, ErrTaskOperationConflict) {
			t.Fatal("request identity changed", attempt, err)
		}
	}
	if err := d.CreateAccount("user:operation", "operation-user", "hash"); err != nil {
		t.Fatal(err)
	}
	user, err := d.BeginTaskOperation("user:operation", "publish", "first", "fixture", 1)
	if err != nil || user.ID == o.ID {
		t.Fatal(user, err)
	}
	f := operationFixtureFile(t, d, "admin-file")
	if _, err = d.EnqueueTask(TaskPublish, f.ID, "{}", user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("ordinary user attached another account file", err)
	}
	var jobs int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='publish' AND file_id=?`, f.ID).Scan(&jobs)
	if jobs != 0 {
		t.Fatal("failed attach escaped enqueue transaction", jobs)
	}
	if _, err = d.TaskOperationByID(o.ID, "user:operation"); !errors.Is(err, ErrNotFound) {
		t.Fatal("operation metadata leaked", err)
	}
	if err = d.TouchTaskOperation(o.ID, "user:operation"); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal(err)
	}
}

func TestExplicitOperationCancelSealsLaterChunkAtomically(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "publish", "cancel-between-chunks", 2)
	a := operationFixtureFile(t, d, "already-accepted")
	id, err := d.EnqueueTask(TaskPublish, a.ID, "{}", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, count, err := d.CancelTaskGroup(o.TaskID, false); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	b := operationFixtureFile(t, d, "late-chunk")
	if _, err = d.EnqueueTask(TaskPublish, b.ID, "{}", o.ID); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal("canceled operation accepted another chunk", err)
	}
	if err = d.RecordTaskOperationItem(o.ID, AdminOwner, "late", "", "late.jpg", "failed", "network"); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal(err)
	}
	if err = d.SealTaskOperation(o.ID, AdminOwner); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal(err)
	}
	job, _ := d.GetTask(id)
	if job.State != "canceled" {
		t.Fatal(job)
	}
	var leaked int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=?`, b.ID).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("late task survived rollback", leaked)
	}
}

func TestExplicitOperationItemIdentityCannotHideAnotherFile(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "single-input-identity", 1)
	a := operationFixtureFile(t, d, "first-input")
	b := operationFixtureFile(t, d, "different-input")
	for i, f := range []*File{a, b} {
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('direct_archive',?,'{}','pending',unixepoch(),unixepoch(),unixepoch())`, f.ID)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		err = AttachTaskOperationItem(tx, id, TaskDirectArchive, f.ID, "{}", o.ID, "same-client-input")
		if i == 0 {
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		} else {
			tx.Rollback()
			if !errors.Is(err, ErrTaskOperationConflict) {
				t.Fatal("same key accepted a different file", err)
			}
		}
	}
	items, n, err := d.TaskOperationItems(o.ID, AdminOwner, "all", 1, 50)
	if err != nil || n != 1 || items[0].FileID != a.ID {
		t.Fatal(items, n, err)
	}
	var jobs int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=?`, b.ID).Scan(&jobs)
	if jobs != 0 {
		t.Fatal("conflicting input left a hidden job", jobs)
	}
}

func TestExplicitOperationSealCountsUnsubmittedFilesWithoutPlaceholderRows(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "partial-intake", 1000000)
	d.RecordTaskOperationItem(o.ID, AdminOwner, "first", "", "first.jpg", "done", "")
	if err := d.SealTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	group, err := d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "failed" || group.Group.UnsubmittedFiles != 999999 || group.Group.FailedFiles != 999999 || group.Group.ProcessedFiles != 1000000 || group.Group.SucceededFiles != 1 {
		t.Fatal(group, err)
	}
	var count int
	d.QueryRow(`SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?`, o.ID).Scan(&count)
	if count != 1 {
		t.Fatal("missing input allocated placeholder rows", count)
	}
	if err = d.RetryTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	group, _ = d.GetTaskOperation(o.ID, AdminOwner)
	if group.State != "failed" || group.Group.UnsubmittedFiles != 999999 {
		t.Fatal("retry erased unsubmitted input", group)
	}
}

func TestExplicitOperationFilesRetainNamesSearchAndLatestRetryState(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "publish", "retained-files", 3)
	a := operationFixtureFile(t, d, "gone-after-success")
	b := operationFixtureFile(t, d, "retry-after-failure")
	aid, _ := d.EnqueueTask(TaskPublish, a.ID, "{}", o.ID)
	bid, _ := d.EnqueueTask(TaskPublish, b.ID, "{}", o.ID)
	d.FinishTask(aid, "{}")
	d.Exec(`UPDATE tasks SET state='failed',last_error='provider unavailable' WHERE id=?`, bid)
	if err := d.RecordTaskOperationItem(o.ID, AdminOwner, "missing", "", "not-supported.xyz", "skipped", "unsupported"); err != nil {
		t.Fatal(err)
	}
	// A late HTTP response must not turn completed work back into queued work.
	if err := d.RecordTaskOperationItem(o.ID, AdminOwner, a.ID, a.ID, a.OrigName, "queued", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SealTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	d.Exec(`DELETE FROM files WHERE id=?`, a.ID)
	group, err := d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "failed" || group.Group.TotalFiles != 3 || group.Group.ProcessedFiles != 3 || group.Group.SucceededFiles != 1 || group.Group.SkippedFiles != 1 || group.Group.FailedFiles != 1 {
		t.Fatal(group, err)
	}
	items, n, err := d.TaskOperationItems(o.ID, AdminOwner, "done", 1, 50)
	if err != nil || n != 1 || len(items) != 1 || items[0].FileName != a.OrigName || items[0].FileExists {
		t.Fatal(items, n, err)
	}
	if _, n, err = d.QueryActivityTasks(LogQuery{Grouped: true, Search: a.OrigName}); err != nil || n != 1 {
		t.Fatal("deleted filename not searchable", n, err)
	}
	children, _, err := d.GroupedTaskMembers(o.TaskID, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child.ID == aid && child.FileName != a.OrigName {
			t.Fatal("deleted child lost filename", child)
		}
	}
	if err = d.RetryTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(bid); err != nil {
		t.Fatal(err)
	}
	d.FinishTask(bid, "{}")
	group, err = d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "done" || group.Group.SucceededFiles != 2 || group.Group.FailedFiles != 0 || group.Group.SkippedFiles != 1 {
		t.Fatal(group, err)
	}
}

func TestExplicitOperationExpiredInputKeepsAcceptedWorkAndRecovery(t *testing.T) {
	d := telegramTestDB(t)
	o := beginOperationFixture(t, d, "upload", "abandoned-browser", 2)
	f := operationFixtureFile(t, d, "accepted-upload")
	id, err := d.EnqueueTask(TaskDirectArchive, f.ID, "{}", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE task_operations SET expires_at=? WHERE id=?`, now()-1, o.ID)
	group, err := d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "pending" || group.Group.InputState != "expired" {
		t.Fatal(group, err)
	}
	job, _ := d.GetTask(id)
	if job.State != "pending" {
		t.Fatal("expired intake canceled accepted archive", job)
	}
	d.FinishTask(id, "{}")
	group, err = d.GetTaskOperation(o.ID, AdminOwner)
	if err != nil || group.State != "failed" || group.LastError.String == "" {
		t.Fatal(group, err)
	}
	if err = d.RetryTaskOperation(o.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	group, _ = d.GetTaskOperation(o.ID, AdminOwner)
	if group.State != "failed" {
		t.Fatal("retry erased evidence of unreceived files", group)
	}
}

func TestExplicitOperationRetentionPreservesWholeRecoverableGroup(t *testing.T) {
	d := telegramTestDB(t)
	old := now() - 40*86400
	closed := beginOperationFixture(t, d, "publish", "old-closed", 1)
	f := operationFixtureFile(t, d, "closed-original")
	id, _ := d.EnqueueTask(TaskPublish, f.ID, "{}", closed.ID)
	d.FinishTask(id, "{}")
	d.SealTaskOperation(closed.ID, AdminOwner)
	failed := beginOperationFixture(t, d, "publish", "old-failed-item", 1)
	d.RecordTaskOperationItem(failed.ID, AdminOwner, "failed-file", "", "failed.jpg", "failed", "upload failure")
	d.SealTaskOperation(failed.ID, AdminOwner)
	open := beginOperationFixture(t, d, "upload", "still-receiving", 1)
	d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=CASE WHEN state='done' THEN ? END`, old, old, old)
	d.Exec(`UPDATE task_operations SET created_at=?,updated_at=?`, old, old)
	d.Exec(`UPDATE task_operation_items SET created_at=?,updated_at=?`, old, old)
	result, err := d.CleanupLogs(context.Background(), now())
	if err != nil || result.Operations != 1 || result.Tasks != 2 {
		t.Fatal(result, err)
	}
	if _, err = d.TaskOperationByID(closed.ID, AdminOwner); !errors.Is(err, ErrNotFound) {
		t.Fatal("closed metadata not cleaned", err)
	}
	for _, keep := range []*TaskOperation{failed, open} {
		if _, err = d.TaskOperationByID(keep.ID, AdminOwner); err != nil {
			t.Fatal("recoverable operation deleted", keep.ID, err)
		}
	}
	var count int
	d.QueryRow(`SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?`, closed.ID).Scan(&count)
	if count != 0 {
		t.Fatal("item foreign-key cleanup incomplete", count)
	}
	if _, err = d.GetFile(f.ID); err != nil {
		t.Fatal("log cleanup deleted original", err)
	}
	var integrity string
	d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Fatal(integrity)
	}
}
