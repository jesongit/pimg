package tasks

import (
	"io"
	"log/slog"
	"path/filepath"
	"pimg/internal/dbx"
	"testing"
	"time"
)

func TestSystemScheduleDoesNotCleanupOnStartupOrPolicyWake(t *testing.T) {
	d, err := dbx.Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().Unix()
	old := at - 40*86400
	add := func() int64 {
		id, err := d.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		d.Exec(`UPDATE tasks SET state='done',created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, id)
		return id
	}
	original := add()
	if err = d.PrepareSystemSchedules(at, true); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE system_schedules SET enabled=0,state='disabled',next_run=0 WHERE id!='log_cleanup'`)
	e := New(d, nil, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.wg.Add(1)
	go e.systemScheduleLoop()
	defer func() { e.cancel(); e.wg.Wait() }()
	e.WakeLogCleanup()
	time.Sleep(150 * time.Millisecond)
	if _, err = d.GetTask(original); err != nil {
		t.Fatal("startup/wake cleaned history", err)
	}
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, time.Now().Unix())
	e.WakeSchedules()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := d.SystemSchedule("log_cleanup")
		if s.LastStatus == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, _ := d.SystemSchedule("log_cleanup")
	if s.LastStatus != "done" || s.NextRun <= time.Now().Unix() {
		t.Fatal(s)
	}
	if _, err = d.GetTask(original); err == nil {
		t.Fatal("due scheduled cleanup did not run")
	}
	next := add()
	e.WakeLogCleanup()
	time.Sleep(150 * time.Millisecond)
	if _, err = d.GetTask(next); err != nil {
		t.Fatal("policy save ran cleanup outside cron", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='system_schedule'`).Scan(&n)
	if n != 1 {
		t.Fatal("maintenance did not produce one summary", n)
	}
}

func TestOperationExpiryRemainsIndependentFromDailyLogSchedule(t *testing.T) {
	d, err := dbx.Open(filepath.Join(t.TempDir(), "expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().Unix()
	d.PrepareSystemSchedules(at, true)
	d.Exec(`UPDATE system_schedules SET enabled=0,next_run=0,state='disabled'`)
	o, err := d.BeginTaskOperation(dbx.AdminOwner, "upload", "expired-input", "fixture", 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE task_operations SET expires_at=? WHERE id=?`, at-1, o.ID)
	old, err := d.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE tasks SET state='done',created_at=?,updated_at=?,done_at=? WHERE id=?`, at-40*86400, at-40*86400, at-40*86400, old)
	e := New(d, nil, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.wg.Add(1)
	go e.operationExpiryLoopEvery(10 * time.Millisecond)
	defer func() { e.cancel(); e.wg.Wait() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		item, _ := d.TaskOperationByID(o.ID, dbx.AdminOwner)
		if item.InputState == "expired" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, _ := d.TaskOperationByID(o.ID, dbx.AdminOwner)
	if item.InputState != "expired" {
		t.Fatal("daily schedule delayed input expiry", item)
	}
	if _, err = d.GetTask(old); err != nil {
		t.Fatal("input expiry ran log cleanup", err)
	}
}
