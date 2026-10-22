package dbx

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPressureArchiveRetryKeepsQueueAge(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	old, err := db.EnqueueTask(TaskArchiveSync, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.EnqueueTask(TaskArchiveSync, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	// Retrying the older task gives it a newer run_after, but new arrivals must
	// not overtake it forever. The due-time gate still preserves its backoff.
	if _, err = db.Exec(`UPDATE tasks SET run_after=?,created_at=? WHERE id=?`, now+2, now-60, old); err != nil {
		t.Fatal(err)
	}
	task, err := db.ClaimIdleTask(now, 60)
	if err != nil || task.ID != fresh {
		t.Fatal("backoff was ignored", task, err)
	}
	if err = db.FinishTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	later, err := db.EnqueueTask(TaskArchiveSync, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	task, err = db.ClaimIdleTask(now+3, 60)
	if err != nil || task.ID != old {
		t.Fatal("new task overtook eligible old archive", later, task, err)
	}
}

func TestPressureOnlyBlockedSameFileForegroundNeedsImmediateYield(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	_, err = db.EnqueueTask(TaskArchiveSync, "same", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimIdleTask(now, 60); err != nil {
		t.Fatal(err)
	}
	if _, err = db.EnqueueTask(TaskArchive, "unrelated", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.EnqueueTask(TaskTelegramScan, "same", `{"automatic":true}`); err != nil {
		t.Fatal(err)
	}
	id, err := db.EnqueueTask(TaskVerify, "same", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE tasks SET run_after=? WHERE id=?`, now+10, id); err != nil {
		t.Fatal(err)
	}
	if blocked, err := db.IdleBlocksForeground(now); err != nil || blocked {
		t.Fatal("unrelated/passive/future task preempted idle", blocked, err)
	}
	if blocked, err := db.IdleBlocksForeground(now + 10); err != nil || !blocked {
		t.Fatal("due same-file foreground was not recognized", blocked, err)
	}
}
