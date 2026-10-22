package dbx

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func activityDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestActivityActiveQueueIndependentOfHistory(t *testing.T) {
	db := activityDB(t)
	old, err := db.EnqueueTask(TaskCleanupTemp, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 240; i++ {
		id, e := db.EnqueueTask(TaskCleanupTemp, "", "{}")
		if e != nil {
			t.Fatal(e)
		}
		if e = db.FinishTask(id, "{}"); e != nil {
			t.Fatal(e)
		}
	}
	running, _ := db.EnqueueTask(TaskCleanupTemp, "", "{}")
	db.Exec(`UPDATE tasks SET state='running' WHERE id=?`, running)
	failed, _ := db.EnqueueTask(TaskCleanupTemp, "", "{}")
	db.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, failed)
	items, total, err := db.ActivityTasks("", "current", "", 1, 50)
	if err != nil || total != 2 || len(items) != 2 || items[0].ID != running || items[1].ID != old {
		t.Fatalf("active priority/history isolation: %+v total=%d err=%v", items, total, err)
	}
	c, err := db.ActivityTaskCounts("")
	if err != nil || c.Pending != 1 || c.Running != 1 || c.Failed != 0 {
		t.Fatal(c, err)
	}
	history, total, err := db.ActivityTasks("", "history", "", 2, 50)
	if err != nil || total != 240 || len(history) != 50 {
		t.Fatal(total, len(history), err)
	}
	for _, item := range history {
		if item.State != "done" {
			t.Fatal(item.State)
		}
	}
	attention, total, err := db.ActivityTasks("", "attention", "", 1, 50)
	if err != nil || total != 1 || attention[0].ID != failed {
		t.Fatal(total, err)
	}
}

func TestReceiptGroupingCursorAndDeletionSnapshot(t *testing.T) {
	db := activityDB(t)
	f := &File{ID: "receipt", Kind: "image", KeyWrapped: []byte{}, KeyNonce: []byte{}, Name: "stored", OrigName: "旅行照片.jpg", ArchiveState: "done", Size: 12, SHA256: "receipt-hash"}
	if err := db.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordIngestEvent(IngestEvent{SourceKey: "pimg:" + f.ID, Type: "verified", FileID: f.ID, Body: json.RawMessage(`{"verification":"verified"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFileVerification(f, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 53; i++ {
		id := string(rune('a' + i))
		if err := db.RecordIngestEvent(IngestEvent{SourceKey: "rejected:" + id, Type: "failed", Reason: "bad input", Body: json.RawMessage(`{"orig_name":"bad.jpg"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := db.IntakeReceipts("", "", 0)
	if err != nil || len(items) != 51 {
		t.Fatal(len(items), err)
	}
	older, err := db.IntakeReceipts("", "", items[49].ID)
	if err != nil || len(older) != 4 {
		t.Fatal(len(older), err)
	}
	var receipt IntakeReceipt
	for _, v := range older {
		if v.FileID == f.ID {
			receipt = v
		}
	}
	if receipt.Type != "verified" || receipt.Name != f.OrigName || receipt.Outcome != "archived" {
		t.Fatal(receipt)
	}
	last, err := db.IntakeReceipts("", "", receipt.ID)
	if err != nil || len(last) != 0 {
		t.Fatal("older receipt stage repeated", last, err)
	}
	if _, err = db.Exec(`DELETE FROM files WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := db.IntakeReceipts(f.ID, "deleted", 0)
	if err != nil || len(deleted) != 1 || deleted[0].Name != f.OrigName || deleted[0].Exists {
		t.Fatal(deleted, err)
	}
	events, err := db.ReceiptEvents(receipt.ID)
	if err != nil || len(events) != 3 {
		t.Fatal(events, err)
	}
	if _, err = db.ReceiptEvents(-9); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestReceiptMigrationPreservesExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &File{ID: "preexisting", Kind: "image", KeyWrapped: []byte{}, KeyNonce: []byte{}, Name: "原文件", OrigName: "before.png", ArchiveState: "done"}
	if err = db.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DELETE FROM ingest_events; DROP INDEX idx_ingest_source_latest; DROP INDEX idx_tasks_state_id`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(activitySchema); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetFile(f.ID)
	if err != nil || got.OrigName != f.OrigName {
		t.Fatal(got, err)
	}
	r, err := db.IntakeReceipts(f.ID, "", 0)
	if err != nil || len(r) != 1 || r[0].Name != "before.png" || r[0].Outcome != "archived" {
		t.Fatal(r, err)
	}
	var body map[string]any
	json.Unmarshal(r[0].Body, &body)
	if body["source"] != "existing" {
		t.Fatal(body)
	}
}
