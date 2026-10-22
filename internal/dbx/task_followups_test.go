package dbx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func followupOperation(t *testing.T, d *DB, task int64) (int64, string) {
	t.Helper()
	var operation int64
	var item string
	if err := d.QueryRow(`SELECT operation_id,item_key FROM task_operation_jobs WHERE task_id=?`, task).Scan(&operation, &item); err != nil {
		t.Fatal(err)
	}
	return operation, item
}

func TestConversionCleanupKeepsOneOperationAndRecoveryRows(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("convert-source", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginConversion(f.ID, "convert-target", false, []byte{1}, []byte{2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	op, item := followupOperation(t, d, c.TaskID)
	if item != f.ID {
		t.Fatal("conversion lost its file identity", item)
	}
	if err = d.SetArchiveState(c.TargetID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.ConversionPhase(f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err != nil {
		t.Fatal(err)
	}
	var cleanup int64
	if err = d.QueryRow(`SELECT id FROM tasks WHERE type='cleanup_converted' AND file_id=?`, f.ID).Scan(&cleanup); err != nil {
		t.Fatal(err)
	}
	if got, gotItem := followupOperation(t, d, cleanup); got != op || gotItem != item {
		t.Fatal("cleanup escaped conversion operation", got, gotItem, op, item)
	}
	parent, _ := d.GetTask(c.TaskID)
	child, _ := d.GetTask(cleanup)
	if parent.State != "done" || child.State != "pending" {
		t.Fatal("phase recovery rows changed", parent.State, child.State)
	}
	var grouped string
	if err = d.QueryRow(taskGroupsCTE+` SELECT group_json FROM grouped_tasks WHERE id=?`, c.TaskID).Scan(&grouped); err != nil {
		t.Fatal(err)
	}
	var progress TaskGroup
	if err = json.Unmarshal([]byte(grouped), &progress); err != nil || progress.Kind != "convert" || progress.TotalFiles != 1 || progress.ProcessedFiles != 0 || progress.Members != 2 {
		t.Fatal("conversion phases appeared as multiple files", progress, err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed',last_error='fixture cleanup failure' WHERE id=?`, cleanup); err != nil {
		t.Fatal(err)
	}
	conversion, err := d.GetConversion(f.ID)
	if err != nil || conversion.Phase != "done" || !conversion.CleanupPending {
		t.Fatal("cleanup failure lost resumable conversion", conversion, err)
	}
}

func TestConversionCleanupGroupingFailureRollsBackPromotion(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("atomic-source", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginConversion(f.ID, "atomic-target", false, []byte{1}, []byte{2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetArchiveState(c.TargetID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.ConversionPhase(f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`CREATE TRIGGER reject_cleanup_membership BEFORE INSERT ON task_operation_jobs WHEN EXISTS(SELECT 1 FROM tasks WHERE id=NEW.task_id AND type='cleanup_converted') BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err == nil {
		t.Fatal("failed membership did not abort atomic phase transition")
	}
	var hiddenSource, hiddenTarget, cleanups int
	var phase string
	if err = d.QueryRow(`SELECT (SELECT hidden FROM files WHERE id=?),(SELECT hidden FROM files WHERE id=?),(SELECT phase FROM file_conversions WHERE source_id=?),(SELECT COUNT(*) FROM tasks WHERE type='cleanup_converted')`, f.ID, c.TargetID, f.ID).Scan(&hiddenSource, &hiddenTarget, &phase, &cleanups); err != nil {
		t.Fatal(err)
	}
	if hiddenSource != 0 || hiddenTarget != 1 || phase != "archive" || cleanups != 0 {
		t.Fatal("partial conversion phase committed", hiddenSource, hiddenTarget, phase, cleanups)
	}
}

func TestColdDeleteFollowsSyncWithoutStealingPriorGeneration(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("follow-sync", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	repo, err := d.AddColdRepository("replica", "local", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	parent, err := d.FindActiveTask(TaskColdSync, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.EnqueueColdDelete(repo, f.ID, []string{"old-folder"})
	if err != nil {
		t.Fatal(err)
	}
	op, _ := followupOperation(t, d, parent.ID)
	if got, _ := followupOperation(t, d, first); got != op {
		t.Fatal("cold deletion was separated from sync", got, op)
	}
	if err = d.FinishTask(parent.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(first, ""); err != nil {
		t.Fatal(err)
	}
	nextParent, err := d.EnqueueTask(TaskColdSync, f.ID, TaskPayloadJSON(map[string]any{"repository_id": repo}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE cold_sync_files SET task_id=? WHERE repository_id=? AND file_id=?`, nextParent, repo, f.ID); err != nil {
		t.Fatal(err)
	}
	again, err := d.EnqueueColdDelete(repo, f.ID, []string{"old-folder"})
	if err != nil || again != first {
		t.Fatal("existing generation was duplicated", again, first, err)
	}
	if got, _ := followupOperation(t, d, again); got != op {
		t.Fatal("completed cleanup moved into a later wave", got, op)
	}
	if _, err = d.Exec(`UPDATE cold_sync_files SET wanted_revision=wanted_revision+1 WHERE repository_id=? AND file_id=?`, repo, f.ID); err != nil {
		t.Fatal(err)
	}
	next, err := d.EnqueueColdDelete(repo, f.ID, []string{"old-folder"})
	if err != nil || next == first {
		t.Fatal("new revision lost cleanup", next, err)
	}
	newOp, _ := followupOperation(t, d, nextParent)
	if got, _ := followupOperation(t, d, next); got != newOp || got == op {
		t.Fatal("new cleanup has wrong sync wave", got, newOp, op)
	}
}

func TestRetiredSecondaryCleanupFollowsConversionRatherThanOldSync(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("retired-secondary", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	repo, err := d.AddColdRepository("replica", "local", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	oldSync, err := d.FindActiveTask(TaskColdSync, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(oldSync.ID, ""); err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginConversion(f.ID, "retired-secondary-target", false, []byte{1}, []byte{2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetArchiveState(c.TargetID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.ConversionPhase(f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err != nil {
		t.Fatal(err)
	}
	cleanup, err := d.EnqueueColdDelete(repo, f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldOp, _ := followupOperation(t, d, oldSync.ID)
	conversionOp, _ := followupOperation(t, d, c.TaskID)
	if op, item := followupOperation(t, d, cleanup); op != conversionOp || op == oldOp || item != f.ID {
		t.Fatal("retired source cleanup escaped the conversion", op, conversionOp, oldOp, item)
	}
}

func TestFollowupLinkNeverReassignsExistingOperationOrRecoveryState(t *testing.T) {
	d := telegramTestDB(t)
	parent, err := d.EnqueueTask(TaskVerify, "", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.EnqueueTask(TaskVerify, "", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	child, err := d.EnqueueTask(TaskCleanupTemp, "", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AttachTaskFollowup(child, parent); err != nil {
		t.Fatal(err)
	}
	op, item := followupOperation(t, d, child)
	if _, err = d.Exec(`UPDATE tasks SET state='running',attempts=3,cancel_requested=1,lease_until=10000000000,last_error='fixture',result='checkpoint' WHERE id=?`, child); err != nil {
		t.Fatal(err)
	}
	if err = d.AttachTaskFollowup(child, other); err != nil {
		t.Fatal(err)
	}
	if got, gotItem := followupOperation(t, d, child); got != op || gotItem != item {
		t.Fatal("follow-up was stolen by a later parent", got, gotItem, op, item)
	}
	var state, message, result string
	var attempts int
	var cancel bool
	var lease int64
	if err = d.QueryRow(`SELECT state,attempts,cancel_requested,lease_until,last_error,result FROM tasks WHERE id=?`, child).Scan(&state, &attempts, &cancel, &lease, &message, &result); err != nil {
		t.Fatal(err)
	}
	if state != "running" || attempts != 3 || !cancel || lease != 10000000000 || message != "fixture" || result != "checkpoint" {
		t.Fatal("operation link changed recovery state", state, attempts, cancel, lease, message, result)
	}
}

func TestMaintenanceWaveAtomicBoundedAndDoesNotDuplicateRecovery(t *testing.T) {
	d := telegramTestDB(t)
	var ids []string
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("layout-%03d", i)
		ids = append(ids, id)
		if err := d.InsertFile(telegramTestFile(id, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", ids[:100]); err != nil {
		t.Fatal(err)
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", ids[100:]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET state='failed',last_error='fixture' WHERE type='local_layout' AND file_id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", ids[:100]); err != nil {
		t.Fatal(err)
	}
	var tasks, operations, items, failed int
	if err := d.QueryRow(`SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM task_operations),(SELECT COUNT(*) FROM task_operation_items),(SELECT COUNT(*) FROM tasks WHERE state='failed')`).Scan(&tasks, &operations, &items, &failed); err != nil {
		t.Fatal(err)
	}
	if tasks != 105 || operations != 2 || items != 105 || failed != 1 {
		t.Fatal("maintenance wave duplicated or hid recoverable rows", tasks, operations, items, failed)
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", ids); err == nil {
		t.Fatal("unbounded maintenance wave accepted")
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"atomic-layout-a", "atomic-layout-b"} {
		if err := d.InsertFile(telegramTestFile(id, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`CREATE TRIGGER reject_layout_membership BEFORE INSERT ON task_operation_jobs WHEN EXISTS(SELECT 1 FROM tasks WHERE id=NEW.task_id AND file_id='atomic-layout-b') BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.EnqueueMaintenanceTasks("local_layout", []string{"atomic-layout-a", "atomic-layout-b"}); err == nil {
		t.Fatal("partial maintenance membership accepted")
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id LIKE 'atomic-layout-%'`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatal("partial maintenance tasks committed", tasks, err)
	}
}

func TestTelegramPushGroupsOnlyOneActiveAlbumWave(t *testing.T) {
	d := telegramTestDB(t)
	album, _ := d.InsertAlbum("first")
	other, _ := d.InsertAlbum("second")
	for i, id := range []int64{album, other} {
		if err := d.InsertTopicBinding(&TopicBinding{AlbumID: id, AccountID: 11, ChatID: 77, TopicID: 100 + i, State: "active", ScanDate: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE album_topics SET scan_date=1 WHERE album_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	queue := func(album int64, topic, message int) int64 {
		t.Helper()
		id, err := d.QueueTelegramMessage(album, &TelegramMessage{AccountID: 11, ChatID: 77, TopicID: topic, MessageID: message, Date: 100, Name: fmt.Sprintf("photo-%d.heic", message), Reference: `{}`})
		if err != nil || id == 0 {
			t.Fatal(id, err)
		}
		return id
	}
	a, b := queue(album, 100, 101), queue(album, 100, 102)
	op, item := followupOperation(t, d, a)
	if got, nextItem := followupOperation(t, d, b); got != op || item == nextItem {
		t.Fatal("same-album media lost one wave or distinct items", got, nextItem, op, item)
	}
	var grouped string
	if err := d.QueryRow(taskGroupsCTE+` SELECT group_json FROM grouped_tasks WHERE id=?`, a).Scan(&grouped); err != nil {
		t.Fatal(err)
	}
	var progress TaskGroup
	if err := json.Unmarshal([]byte(grouped), &progress); err != nil || progress.Kind != "telegram_receive" || progress.TotalFiles != 2 || progress.ProcessedFiles != 0 {
		t.Fatal("album serialization key collapsed per-message progress", progress, err)
	}
	c := queue(other, 101, 103)
	if got, _ := followupOperation(t, d, c); got == op {
		t.Fatal("different albums share a receive operation")
	}
	if err := d.RequestTaskCancel(a); err != nil {
		t.Fatal(err)
	}
	next := queue(album, 100, 104)
	if got, _ := followupOperation(t, d, next); got == op {
		t.Fatal("new arrival joined a canceled wave")
	}
	if err := d.FinishTask(next, ""); err != nil {
		t.Fatal(err)
	}
	latest := queue(album, 100, 105)
	latestOp, _ := followupOperation(t, d, latest)
	nextOp, _ := followupOperation(t, d, next)
	if latestOp == op || latestOp == nextOp {
		t.Fatal("new arrival joined a closed wave")
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil || count != 5 {
		t.Fatal("grouping replaced durable per-message jobs", count, err)
	}
}

func TestTelegramAcceptedArchiveRetainsCanceledWaveAndFileSnapshot(t *testing.T) {
	d := telegramTestDB(t)
	album, _ := d.InsertAlbum("incoming")
	if err := d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 100, State: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE album_topics SET scan_date=1 WHERE album_id=?`, album); err != nil {
		t.Fatal(err)
	}
	parent, err := d.QueueTelegramMessage(album, &TelegramMessage{AccountID: 11, ChatID: 77, TopicID: 100, MessageID: 101, Date: 100, Name: "received.heic", Reference: `{}`})
	if err != nil || parent == 0 {
		t.Fatal(parent, err)
	}
	op, item := followupOperation(t, d, parent)
	if err = d.RequestTaskCancel(parent); err != nil {
		t.Fatal(err)
	}
	f := telegramTestFile("accepted-media", album)
	f.ArchiveState, f.OrigName, f.ParentTaskID = "pending", "received.heic", parent
	if err = d.InsertUploadBatch([]*File{f}); err != nil {
		t.Fatal(err)
	}
	child, err := d.FindActiveTask(TaskArchive, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, gotItem := followupOperation(t, d, child.ID); got != op || gotItem != item {
		t.Fatal("accepted archive escaped its canceled intake wave", got, gotItem, op, item)
	}
	var fileID, name string
	if err = d.QueryRow(`SELECT file_id,name FROM task_operation_items WHERE operation_id=? AND item_key=?`, op, item).Scan(&fileID, &name); err != nil || fileID != f.ID || name != f.OrigName {
		t.Fatal("received file snapshot missing", fileID, name, err)
	}
	parentTask, _ := d.GetTask(parent)
	if parentTask.State != "canceled" || child.State != "pending" {
		t.Fatal("accepted archive recovery changed cancellation state", parentTask.State, child.State)
	}
}

func TestTelegramHistoryScanCountsAcceptedFilesRatherThanIntakeMarker(t *testing.T) {
	d := telegramTestDB(t)
	parent, err := d.EnqueueTask(TaskTelegramScan, TelegramAlbumTaskKey(1), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	a, b := telegramTestFile("scan-media-a", 0), telegramTestFile("scan-media-b", 0)
	for _, f := range []*File{a, b} {
		f.ArchiveState, f.ParentTaskID = "pending", parent
	}
	if err = d.InsertUploadBatch([]*File{a, b}); err != nil {
		t.Fatal(err)
	}
	var grouped string
	if err = d.QueryRow(taskGroupsCTE+` SELECT group_json FROM grouped_tasks WHERE id=?`, parent).Scan(&grouped); err != nil {
		t.Fatal(err)
	}
	var progress TaskGroup
	if err = json.Unmarshal([]byte(grouped), &progress); err != nil || progress.Kind != "telegram_receive" || progress.TotalFiles != 2 || progress.Members != 3 {
		t.Fatal("history intake counted as a media file", progress, err)
	}
	for _, f := range []*File{a, b} {
		var fileID, name string
		if err = d.QueryRow(`SELECT file_id,name FROM task_operation_items WHERE file_id=?`, f.ID).Scan(&fileID, &name); err != nil || fileID != f.ID || name != f.OrigName {
			t.Fatal("scan file snapshot missing", fileID, name, err)
		}
	}
}
