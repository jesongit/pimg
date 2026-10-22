package dbx

import (
	"errors"
	"testing"
	"time"
)

func TestColdSyncSelectionQueueCancellationAndDeletionReferences(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("replicated", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	repo, err := d.AddColdRepository("backup", "local", `{"directory":"replicas/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	if _, err = d.FindActiveTask(TaskColdSync, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("unselected repository started replication", err)
	}
	if err = d.SetColdRepositoryEnabled(repo, true); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	syncTask, err := d.FindActiveTask(TaskColdSync, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTask(time.Now().Unix(), 100); !errors.Is(err, ErrNotFound) {
		t.Fatal("normal worker claimed idle work", err)
	}
	foreground, err := d.EnqueueTask(TaskVerify, f.ID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimIdleTask(time.Now().Unix(), 100); !errors.Is(err, ErrNotFound) {
		t.Fatal("idle task passed foreground queue", err)
	}
	if err = d.FinishTask(foreground, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimIdleTask(time.Now().Unix(), 100)
	if err != nil || claimed.ID != syncTask.ID {
		t.Fatal(claimed, err)
	}
	if err = d.RequestTaskCancel(claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal("deletion should queue behind running cancellation", err)
	}
	if err = d.DeleteFile(f.ID); !errors.Is(err, ErrTaskReferenced) {
		t.Fatal("running cancellation released its reference early", err)
	}
	if err = d.FinishTaskCanceled(claimed.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	if _, err = d.FindActiveTask(TaskColdSync, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancelled job was silently recreated", err)
	}
	purge, err := d.EnqueuePurgeTask(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RetryTask(claimed.ID); !errors.Is(err, ErrDeleting) {
		t.Fatal("cancelled sync resumed during purge", err)
	}
	if _, err = d.EnqueueTask(TaskVerify, f.ID, "{}"); err == nil {
		t.Fatal("new reference entered a purging file")
	}
	if _, err = d.ClaimTask(time.Now().Unix(), 100); !errors.Is(err, ErrNotFound) {
		t.Fatal("normal worker claimed deletion", err)
	}
	if _, err = d.ClaimIdleTask(time.Now().Unix(), 100); err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteFileAfterTask(f.ID, purge); err != nil {
		t.Fatal("owning deletion task cannot finish", err)
	}
}

func TestColdSyncRevisionTracksAlbumAndContentButNotPublication(t *testing.T) {
	d := telegramTestDB(t)
	album, _ := d.InsertAlbum("before")
	f := telegramTestFile("changed", album)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	repo, _ := d.AddColdRepository("backup", "local", `{"directory":"replicas/a"}`)
	_ = d.SetColdRepositoryEnabled(repo, true)
	if err := d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	j, err := d.GetColdSyncJob(repo, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := d.FindActiveTask(TaskColdSync, f.ID)
	_ = d.FinishColdSync(j)
	_ = d.FinishTask(task.ID, "")
	if err = d.SetPublished(f.ID, true, "path", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	after, _ := d.GetColdSyncJob(repo, f.ID)
	if after.Revision != j.Revision {
		t.Fatal("publication churn caused replication")
	}
	if err = d.RenameAlbum(album, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	after, _ = d.GetColdSyncJob(repo, f.ID)
	if after.Revision <= j.Revision {
		t.Fatal("album rename was not replicated")
	}
	if _, err = d.FindActiveTask(TaskColdSync, f.ID); err != nil {
		t.Fatal("changed source was not queued", err)
	}
}

func TestTaskCancelSurvivesCompletionRaceAndRestart(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("reference", 0)
	_ = d.InsertFile(f)
	for _, finish := range []func(int64) error{func(id int64) error { return d.FinishTask(id, "") }, func(id int64) error { return d.FailTask(id, "cancel race") }, func(id int64) error { return d.PostponeIdleTask(id) }, func(id int64) error { _, err := d.ResetRunningTasks(); return err }} {
		id, _ := d.EnqueueTask(TaskVerify, f.ID, "{}")
		if _, err := d.ClaimTask(time.Now().Unix(), 100); err != nil {
			t.Fatal(err)
		}
		_ = d.RequestTaskCancel(id)
		if err := finish(id); err != nil {
			t.Fatal(err)
		}
		task, _ := d.GetTask(id)
		if task.State != "canceled" {
			t.Fatal("cancelled task was revived", task.State)
		}
	}
	id, _ := d.EnqueueTask(TaskVerify, f.ID, "{}")
	_, _ = d.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, id)
	if _, err := d.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal("failed work should be canceled automatically", err)
	}
	job, _ := d.GetTask(id)
	if job.State != "canceled" {
		t.Fatal("failed work not retired", job)
	}
	if _, err := d.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCancelArchiveCanBeRetriedFromFile(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("cancelled-upload", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	id, err := d.EnqueueTask(TaskArchive, f.ID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RequestTaskCancel(id); err != nil {
		t.Fatal(err)
	}
	retried, err := d.RetryFileArchive(f.ID)
	if err != nil || retried != id {
		t.Fatal(retried, err)
	}
	claimed, err := d.ClaimTask(time.Now().Unix(), 100)
	if err != nil || claimed.ID != id {
		t.Fatal("file retry retained its cancellation flag", claimed, err)
	}
}

func TestColdSyncCleanupRequeuesForLaterMovesButRespectsCancellation(t *testing.T) {
	d := telegramTestDB(t)
	f := telegramTestFile("repeated-moves", 0)
	if err := d.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	repo, _ := d.AddColdRepository("backup", "local", `{"directory":"replicas/a"}`)
	_ = d.SetColdRepositoryEnabled(repo, true)
	if err := d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	first, err := d.EnqueueColdDelete(repo, f.ID, []string{"same-old-location"})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.FinishTask(first, "")
	if _, err = d.Exec(`UPDATE files SET name='after another move' WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.ReconcileColdSync(); err != nil {
		t.Fatal(err)
	}
	next, err := d.EnqueueColdDelete(repo, f.ID, []string{"same-old-location"})
	if err != nil || next == first {
		t.Fatal("historical completed cleanup suppressed a later deletion", next, err)
	}
	if err = d.RequestTaskCancel(next); err != nil {
		t.Fatal(err)
	}
	again, err := d.EnqueueColdDelete(repo, f.ID, []string{"same-old-location"})
	if err != nil || again != next {
		t.Fatal("cancelled cleanup was silently recreated", again, err)
	}
}
