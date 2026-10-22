package dbx

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mutationOperation(t *testing.T, d *DB, owner, kind, scope, key string, total int) *TaskOperation {
	t.Helper()
	op, err := d.BeginTaskOperation(owner, kind, key, scope, total)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestMoveFileInOperationRejectsClosedCapacityAndWrongScopeBeforeMutation(t *testing.T) {
	d := telegramTestDB(t)
	target, err := d.InsertAlbum("target")
	if err != nil {
		t.Fatal(err)
	}
	a, b := operationFixtureFile(t, d, "move-a"), operationFixtureFile(t, d, "move-b")
	scope := fmt.Sprintf("album:%d", target)
	op := mutationOperation(t, d, AdminOwner, "move_album", scope, "move-atomic", 1)
	if err = d.MoveFileInOperation(a.ID, target, op.ID); err != nil {
		t.Fatal(err)
	}
	items, total, err := d.TaskOperationItems(op.ID, AdminOwner, "done", 1, 50)
	if err != nil || total != 1 || items[0].FileID != a.ID || items[0].FileName != a.OrigName {
		t.Fatal("atomic move receipt missing", items, total, err)
	}
	if err = d.MoveFileInOperation(b.ID, target, op.ID); err == nil {
		t.Fatal("over-capacity move accepted")
	}
	if got, _ := d.GetFile(b.ID); got.AlbumID.Valid {
		t.Fatal("over-capacity move changed album", got.AlbumID)
	}
	wrong := mutationOperation(t, d, AdminOwner, "move_album", "album:0", "wrong-move-scope", 1)
	if err = d.MoveFileInOperation(b.ID, target, wrong.ID); !errors.Is(err, ErrTaskOperationConflict) {
		t.Fatal("wrong target scope accepted", err)
	}
	closed := mutationOperation(t, d, AdminOwner, "move_album", scope, "canceled-move", 1)
	if _, _, err = d.CancelTaskGroup(closed.TaskID, false); err != nil {
		t.Fatal(err)
	}
	if err = d.MoveFileInOperation(b.ID, target, closed.ID); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal("canceled move accepted", err)
	}
	if got, _ := d.GetFile(b.ID); got.AlbumID.Valid {
		t.Fatal("rejected move changed album", got.AlbumID)
	}
	if err = d.MoveFileInOperation(a.ID, target, op.ID); err != nil {
		t.Fatal("same input was not idempotent", err)
	}
}

func TestMoveFileInOperationKeepsOwnerFreezeConversionAndDeletionGuards(t *testing.T) {
	d := telegramTestDB(t)
	source, _ := d.InsertAlbum("source")
	target, _ := d.InsertAlbum("target")
	f := operationFixtureFile(t, d, "guarded-move")
	if err := d.UpdateFileMeta(f.ID, "", sql.NullInt64{Int64: source, Valid: true}, nil); err != nil {
		t.Fatal(err)
	}
	op := mutationOperation(t, d, AdminOwner, "move_album", fmt.Sprintf("album:%d", target), "guarded-move-op", 1)
	for _, album := range []int64{source, target} {
		if err := d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: int(album), State: "active"}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE album_topics SET state='deleting' WHERE album_id=?`, album); err != nil {
			t.Fatal(err)
		}
		if err := d.MoveFileInOperation(f.ID, target, op.ID); !errors.Is(err, ErrAlbumFrozen) {
			t.Fatal("source/target deletion freeze escaped", album, err)
		}
		if _, err := d.Exec(`UPDATE album_topics SET state='active' WHERE album_id=?`, album); err != nil {
			t.Fatal(err)
		}
	}
	foreign := mutationOperation(t, d, "user:foreign", "move_album", fmt.Sprintf("album:%d", target), "foreign-move", 1)
	if err := d.MoveFileInOperation(f.ID, target, foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign owner moved file", err)
	}
	if _, err := d.Exec(`UPDATE files SET purge_state='pending' WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.MoveFileInOperation(f.ID, target, op.ID); !errors.Is(err, ErrDeleting) {
		t.Fatal("purging file moved", err)
	}
	if _, err := d.Exec(`UPDATE files SET purge_state=NULL WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BeginConversion(f.ID, "guarded-move-target", true, []byte{1}, []byte{2}, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.MoveFileInOperation(f.ID, target, op.ID); !errors.Is(err, ErrConverting) {
		t.Fatal("converting file moved", err)
	}
	got, _ := d.GetFile(f.ID)
	var items int
	if err := d.QueryRow(`SELECT COUNT(*) FROM task_operation_items WHERE operation_id=?`, op.ID).Scan(&items); err != nil || got.AlbumID.Int64 != source || items != 0 {
		t.Fatal("guard rejection left partial mutation or receipt", got.AlbumID, items, err)
	}
}

func TestOperationTaskConcurrentIntakeReusesOwnWorkerWithoutStealing(t *testing.T) {
	d := telegramTestDB(t)
	f := operationFixtureFile(t, d, "concurrent-publish")
	op := mutationOperation(t, d, AdminOwner, "publish", "", "concurrent-publish-op", 1)
	const callers = 12
	start := make(chan struct{})
	ids := make(chan int64, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := d.EnqueueOperationTask(TaskPublish, f.ID, `{}`, op.ID)
			ids <- id
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var worker int64
	for id := range ids {
		if worker == 0 {
			worker = id
		}
		if id != worker {
			t.Fatal("concurrent input created multiple workers", worker, id)
		}
	}
	other := mutationOperation(t, d, AdminOwner, "publish", "", "other-publish-op", 1)
	if _, err := d.EnqueueOperationTask(TaskPublish, f.ID, `{}`, other.ID); !errors.Is(err, ErrTaskOperationConflict) {
		t.Fatal("other operation stole active worker", err)
	}
	if got, _ := followupOperation(t, d, worker); got != op.ID {
		t.Fatal("worker group changed", got, op.ID)
	}
	if _, _, err := d.CancelTaskGroup(op.TaskID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueOperationTask(TaskPublish, f.ID, `{}`, op.ID); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal("canceled operation accepted a later chunk", err)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='publish' AND file_id=?`, f.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate workers committed", count, err)
	}
}

func TestOperationTaskCapacityScopeOwnerAndFreezeRollBackWorker(t *testing.T) {
	d := telegramTestDB(t)
	a, b := operationFixtureFile(t, d, "bounded-a"), operationFixtureFile(t, d, "bounded-b")
	op := mutationOperation(t, d, AdminOwner, "publish", "", "bounded-publish", 1)
	if _, err := d.EnqueueOperationTask(TaskPublish, a.ID, `{}`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueOperationTask(TaskPublish, b.ID, `{}`, op.ID); err == nil {
		t.Fatal("capacity overflow accepted")
	}
	wrong := mutationOperation(t, d, AdminOwner, "unpublish", "", "wrong-kind", 1)
	if _, err := d.EnqueueOperationTask(TaskPublish, b.ID, `{}`, wrong.ID); !errors.Is(err, ErrTaskOperationConflict) {
		t.Fatal("wrong kind accepted", err)
	}
	foreign := mutationOperation(t, d, "user:foreign", "publish", "", "foreign-publish", 1)
	if _, err := d.EnqueueOperationTask(TaskPublish, b.ID, `{}`, foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign owner accepted", err)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=?`, b.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected input left worker", count, err)
	}
}

func TestRetryLatestTaskGroupNeverReplaysOldFailureOrSuccessfulItem(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "publish", "", "latest-retry", 4)
	files := []*File{operationFixtureFile(t, d, "retry-a"), operationFixtureFile(t, d, "retry-b"), operationFixtureFile(t, d, "retry-c"), operationFixtureFile(t, d, "retry-d")}
	queue := func(f *File, state string) int64 {
		t.Helper()
		id, err := d.EnqueueTask(TaskPublish, f.ID, `{}`, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET state=?,attempts=4,last_error='fixture' WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldFailure := queue(files[0], "failed")
	success := queue(files[0], "done")
	oldSuccess := queue(files[1], "done")
	failure := queue(files[1], "failed")
	canceled := queue(files[2], "canceled")
	pending := queue(files[3], "pending")
	if err := d.SealTaskOperation(op.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	count, err := d.RetryLatestTaskGroup(op.TaskID)
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, expected := range []struct {
		id       int64
		state    string
		attempts int
	}{{oldFailure, "failed", 4}, {success, "done", 4}, {oldSuccess, "done", 4}, {failure, "pending", 0}, {canceled, "pending", 0}, {pending, "pending", 4}} {
		job, err := d.GetTask(expected.id)
		if err != nil || job.State != expected.state || job.Attempts != expected.attempts {
			t.Fatal("wrong retry attempt selected", expected, job, err)
		}
	}
	if count, err = d.RetryLatestTaskGroup(op.TaskID); err != nil || count != 0 {
		t.Fatal("second retry replayed pending work", count, err)
	}
}

func TestRetryLatestTaskGroupRecoveryGuardFailureRollsBackWholeRetry(t *testing.T) {
	d := telegramTestDB(t)
	op := mutationOperation(t, d, AdminOwner, "publish", "", "retry-atomic", 2)
	a, b := operationFixtureFile(t, d, "retry-atomic-a"), operationFixtureFile(t, d, "retry-atomic-b")
	aid, err := d.EnqueueTask(TaskPublish, a.ID, `{}`, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	bid, err := d.EnqueueTask(TaskPublish, b.ID, `{}`, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed',attempts=4 WHERE id IN (?,?)`, aid, bid); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE files SET purge_state='pending' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RetryLatestTaskGroup(op.TaskID); !errors.Is(err, ErrDeleting) {
		t.Fatal("retry ignored destructive operation", err)
	}
	after, _ := d.GetTask(aid)
	if after.State != "failed" || after.Attempts != 4 {
		t.Fatal("failed group retry partially requeued earlier item", after)
	}
}

func TestTelegramReceiveRetryValidatesInboxAndCurrentAlbumBinding(t *testing.T) {
	d := telegramTestDB(t)
	album, _ := d.InsertAlbum("retry-inbox")
	if err := d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 100, State: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE album_topics SET scan_date=1 WHERE album_id=?`, album); err != nil {
		t.Fatal(err)
	}
	id, err := d.QueueTelegramMessage(album, &TelegramMessage{AccountID: 11, ChatID: 77, TopicID: 100, MessageID: 101, Date: 100, Name: "retry.heic", Reference: `{}`})
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed',attempts=5,cancel_requested=1,last_error='offline' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	op, item := followupOperation(t, d, id)
	if err = d.RetryTask(id); err != nil {
		t.Fatal("inbox serialization key was treated as a file", err)
	}
	job, _ := d.GetTask(id)
	if job.State != "pending" || job.Attempts != 0 {
		t.Fatal(job)
	}
	if got, gotItem := followupOperation(t, d, id); got != op || gotItem != item {
		t.Fatal("retry changed operation/message identity", got, gotItem)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE album_topics SET state='deleting' WHERE album_id=?`, album); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(id); !errors.Is(err, ErrAlbumFrozen) {
		t.Fatal("deleted topic resumed inbox", err)
	}
	if _, err = d.Exec(`UPDATE album_topics SET state='active',topic_id=101 WHERE album_id=?`, album); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(id); err == nil {
		t.Fatal("message resumed in a different topic")
	}
	job, _ = d.GetTask(id)
	if job.State != "failed" {
		t.Fatal("rejected retry mutated failed row", job)
	}
}

func TestPublishRetrySupportsV2MediaAndKeepsLegacyEncryptedGuard(t *testing.T) {
	for _, test := range []struct {
		name         string
		mediaVersion int
		storage      string
		wantError    bool
	}{
		{"v2-policy-managed", 2, "tg", false},
		{"legacy-encrypted", 0, "tg", true},
		{"legacy-direct", 0, "direct", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := telegramTestDB(t)
			f := operationFixtureFile(t, d, test.name)
			if _, err := d.Exec(`UPDATE files SET storage=?,media_version=? WHERE id=?`, test.storage, test.mediaVersion, f.ID); err != nil {
				t.Fatal(err)
			}
			job, err := d.EnqueueTask(TaskPublish, f.ID, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Exec(`UPDATE tasks SET state='failed',attempts=5,last_error='provider offline' WHERE id=?`, job); err != nil {
				t.Fatal(err)
			}
			err = d.RetryTask(job)
			if test.wantError {
				if !errors.Is(err, ErrPermanentEncrypted) {
					t.Fatal("legacy encrypted publishing guard lost", err)
				}
				after, _ := d.GetTask(job)
				if after.State != "failed" || after.Attempts != 5 {
					t.Fatal("rejected retry changed recovery state", after)
				}
				return
			}
			if err != nil {
				t.Fatal("supported publishing retry rejected", err)
			}
			after, _ := d.GetTask(job)
			if after.State != "pending" || after.Attempts != 0 {
				t.Fatal("supported publishing retry not queued", after)
			}
		})
	}
}
