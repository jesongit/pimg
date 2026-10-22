package dbx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSystemScheduleRulesAndStartupSkipMissedRuns(t *testing.T) {
	d := telegramTestDB(t)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Unix()
	if err := d.PrepareSystemSchedules(at, true); err != nil {
		t.Fatal(err)
	}
	rows, err := d.SystemSchedules()
	if err != nil || len(rows) != 4 {
		t.Fatal(rows, err)
	}
	log, _ := d.SystemSchedule("log_cleanup")
	if log.Cron != "0 6 * * *" || log.Timezone != "Asia/Shanghai" || log.NextRun != time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC).Unix() {
		t.Fatal(log)
	}
	for _, r := range rows {
		if r.NextRun <= at || r.LastRun != 0 {
			t.Fatal(r)
		}
	}
	if _, err = d.ClaimSystemSchedule(at, "token", 90); !errors.Is(err, ErrNotFound) {
		t.Fatal("startup ran maintenance", err)
	}
	d.Exec(`UPDATE system_schedules SET next_run=?`, at-86400)
	if err = d.PrepareSystemSchedules(at, true); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimSystemSchedule(at, "token", 90); !errors.Is(err, ErrNotFound) {
		t.Fatal("missed runs were replayed", err)
	}
	for _, rule := range []string{"* * * * * *", "@daily", "@every 1s", "0 0 31 2 *", "*/0 * * * *", "99 * * * *"} {
		if _, err = NextSystemSchedule(rule, "Asia/Shanghai", at); err == nil {
			t.Fatal("invalid rule accepted", rule)
		}
	}
	if _, err = NextSystemSchedule("* * * * *", "Local", at); err == nil {
		t.Fatal("host-dependent Local accepted")
	}
	before, _ := d.SystemSchedule("log_cleanup")
	invalid := "not cron"
	if _, err = d.PatchSystemSchedule("log_cleanup", SystemSchedulePatch{Cron: &invalid}, at); !errors.Is(err, ErrInvalidSystemSchedule) {
		t.Fatal(err)
	}
	after, _ := d.SystemSchedule("log_cleanup")
	if after.Cron != before.Cron || after.NextRun != before.NextRun {
		t.Fatal("invalid rule saved")
	}
}

func TestSystemScheduleLeaseFencingEditsAndSummaryHistory(t *testing.T) {
	d := telegramTestDB(t)
	at := time.Now().Unix()
	d.PrepareSystemSchedules(at, true)
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at)
	s, err := d.ClaimSystemSchedule(at, "first-owner", 90)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimSystemSchedule(at, "second-owner", 90); !errors.Is(err, ErrNotFound) {
		t.Fatal("duplicate claim", err)
	}
	if _, err = d.ResetRunningTasks(); err != nil {
		t.Fatal(err)
	}
	job, _ := d.GetTask(s.TaskID)
	if job.State != "running" {
		t.Fatal("ordinary startup recovery changed schedule", job)
	}
	if err = d.HeartbeatSystemSchedule(s.ID, "wrong-owner", s.TaskID, at+1, 90); !errors.Is(err, ErrScheduleLeaseLost) {
		t.Fatal(err)
	}
	if err = d.HeartbeatSystemSchedule(s.ID, s.LeaseToken, s.TaskID, at+1, 90); err != nil {
		t.Fatal(err)
	}
	newCron := "30 7 * * *"
	patched, err := d.PatchSystemSchedule(s.ID, SystemSchedulePatch{Cron: &newCron}, at+2)
	if err != nil || patched.State != "running" || patched.TaskID != s.TaskID {
		t.Fatal(patched, err)
	}
	if err = d.FinishSystemSchedule(s.ID, "wrong-owner", s.TaskID, at+3, nil, nil); !errors.Is(err, ErrScheduleLeaseLost) {
		t.Fatal(err)
	}
	if err = d.FinishSystemSchedule(s.ID, s.LeaseToken, s.TaskID, at+3, map[string]any{"changed": 1, "summary": "fixture cleanup"}, nil); err != nil {
		t.Fatal(err)
	}
	finished, _ := d.SystemSchedule(s.ID)
	expected, _ := NextSystemSchedule(newCron, "Asia/Shanghai", at+3)
	if finished.State != "scheduled" || finished.NextRun != expected || finished.LastStatus != "done" {
		t.Fatal(finished)
	}
	logs, n, err := d.QueryUnifiedLogs(LogQuery{Search: "日志过期清理"})
	if err != nil || n != 1 || logs[0].Task.FileName != "日志过期清理" {
		t.Fatal(logs, n, err)
	}
	// Healthy/no-change maintenance updates only the fixed schedule row.
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at+4)
	noop, _ := d.ClaimSystemSchedule(at+4, "noop-owner", 90)
	if err = d.FinishSystemSchedule(noop.ID, noop.LeaseToken, noop.TaskID, at+5, map[string]any{"changed": 0, "summary": "no expired history"}, nil); err != nil {
		t.Fatal(err)
	}
	if hasTask(t, d, noop.TaskID) {
		t.Fatal("empty maintenance created visible journal noise")
	}
	if _, n, err = d.QueryUnifiedLogs(LogQuery{Search: "日志过期清理"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestSystemScheduleInterruptedLeaseAndCleanupProtection(t *testing.T) {
	d := telegramTestDB(t)
	at := time.Now().Unix()
	d.PrepareSystemSchedules(at, true)
	d.Exec(`UPDATE system_schedules SET enabled=0,state='disabled',next_run=0 WHERE id!='log_cleanup'`)
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at)
	s, err := d.ClaimSystemSchedule(at, "interrupted-owner", 90)
	if err != nil {
		t.Fatal(err)
	}
	// The cleanup run cannot erase its current lease or persistent definition.
	if _, err = d.ClearLogs(context.Background(), at+1); err != nil {
		t.Fatal(err)
	}
	if job, err := d.GetTask(s.TaskID); err != nil || job.State != "running" {
		t.Fatal(job, err)
	}
	rows, _ := d.SystemSchedules()
	if len(rows) != 4 {
		t.Fatal("log cleanup erased plans")
	}
	if err = d.PrepareSystemSchedules(at+91, false); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishSystemSchedule(s.ID, s.LeaseToken, s.TaskID, at+92, map[string]any{"changed": 1}, nil); !errors.Is(err, ErrScheduleLeaseLost) {
		t.Fatal("stale worker committed", err)
	}
	recovered, _ := d.SystemSchedule(s.ID)
	if recovered.LastStatus != "interrupted" || recovered.State != "scheduled" || recovered.NextRun <= at+91 {
		t.Fatal(recovered)
	}
	job, _ := d.GetTask(s.TaskID)
	if job.State != "failed" {
		t.Fatal(job)
	}
	if _, err = d.ClaimSystemSchedule(at+92, "new-owner", 90); !errors.Is(err, ErrNotFound) {
		t.Fatal("interruption immediately replayed", err)
	}
}

func TestSystemScheduleFailedJournalExpiresButDefinitionsAndRecoveryRemain(t *testing.T) {
	d := telegramTestDB(t)
	at := time.Now().Unix()
	old := at - 40*86400
	d.PrepareSystemSchedules(at, true)
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at)
	retired, err := d.ClaimSystemSchedule(at, "failed-old-run", 90)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishSystemSchedule(retired.ID, retired.LeaseToken, retired.TaskID, at+1, map[string]any{"changed": 0}, errors.New("old maintenance failure")); err != nil {
		t.Fatal(err)
	}
	d.Exec(`UPDATE tasks SET created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, retired.TaskID)
	ordinary, _ := d.EnqueueTask(TaskVerify, "", "{}")
	d.Exec(`UPDATE tasks SET state='failed',last_error='file recovery still needed',created_at=?,updated_at=? WHERE id=?`, old, old, ordinary)
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='storage_probe'`, at)
	protected, err := d.ClaimSystemSchedule(at, "referenced-failed-run", 90)
	if err != nil {
		t.Fatal(err)
	}
	// Even an inconsistent failed journal must remain while a definition still
	// owns its run id. A future recovery pass resolves that reference safely.
	d.Exec(`UPDATE tasks SET state='failed',lease_until=NULL,created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, protected.TaskID)
	result, err := d.CleanupLogs(context.Background(), at)
	if err != nil || result.Tasks != 1 {
		t.Fatal(result, err)
	}
	if hasTask(t, d, retired.TaskID) || !hasTask(t, d, ordinary) || !hasTask(t, d, protected.TaskID) {
		t.Fatal("maintenance journal/recovery boundary changed")
	}
	plan, _ := d.SystemSchedule("log_cleanup")
	if plan.LastStatus != "failed" || plan.LastError != "old maintenance failure" {
		t.Fatal("journal cleanup erased persisted plan failure", plan)
	}
	// Clear also reclaims a new unreferenced failure, rather than calling it a
	// recoverable file task in its response.
	d.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at)
	newFailure, err := d.ClaimSystemSchedule(at, "failed-current-history", 90)
	if err != nil {
		t.Fatal(err)
	}
	d.FinishSystemSchedule(newFailure.ID, newFailure.LeaseToken, newFailure.TaskID, at+1, map[string]any{"changed": 0}, errors.New("new maintenance failure"))
	cleared, err := d.ClearLogs(context.Background(), at+2)
	if err != nil || hasTask(t, d, newFailure.TaskID) || cleared.RetainedRecovery != 2 {
		t.Fatal(cleared, err)
	}
	if !hasTask(t, d, ordinary) || !hasTask(t, d, protected.TaskID) {
		t.Fatal("clear removed referenced recovery")
	}
}
