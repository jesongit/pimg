package dbx

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func priorityScanAlbum(t *testing.T, d *DB) int64 {
	t.Helper()
	album, err := d.InsertAlbum("scan priority")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: 55, Name: "scan priority", CreateName: "priority", State: "active"}); err != nil {
		t.Fatal(err)
	}
	return album
}

func TestTelegramScanPriorityOnlyAcceptsExplicitBooleanAutomatic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		passive bool
	}{
		{"omitted", `{}`, false},
		{"null", `{"automatic":null}`, false},
		{"false", `{"automatic":false}`, false},
		{"true", `{"automatic":true}`, true},
		{"numeric-one", `{"automatic":1}`, false},
		{"string-true", `{"automatic":"true"}`, false},
		{"malformed", `not-json`, false},
		{"scalar", `true`, false},
	} {
		for _, state := range []string{"pending", "running"} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				d := telegramTestDB(t)
				id, err := d.EnqueueTask(TaskTelegramScan, "tg-album:probe", tc.payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = d.Exec(`UPDATE tasks SET state=? WHERE id=?`, state, id); err != nil {
					t.Fatal(err)
				}
				task, err := d.GetTask(id)
				if err != nil || PassiveTask(task) != tc.passive {
					t.Fatal("Go task priority differs from explicit JSON boolean", err)
				}
				busy, err := d.ColdSyncForegroundBusy(time.Now().Unix())
				if err != nil || busy == tc.passive {
					t.Fatal("SQL task priority differs from Go classification", busy, tc.passive, err)
				}
				if _, err = d.EnqueueTask(TaskPurgeFile, "independent-fixture", "{}"); err != nil {
					t.Fatal(err)
				}
				_, err = d.ClaimIdleTask(time.Now().Unix(), 100)
				if tc.passive && err != nil || !tc.passive && !errors.Is(err, ErrNotFound) {
					t.Fatal("idle claim did not follow task priority", err)
				}
			})
		}
	}
}

func TestAutomaticTelegramScanDoesNotBlockIdleWhilePendingOrRunning(t *testing.T) {
	d := telegramTestDB(t)
	album := priorityScanAlbum(t, d)
	probeID, err := d.QueueLinkedAlbumScan(album, `{"automatic":true,"since":100,"offset":50}`)
	if err != nil {
		t.Fatal(err)
	}
	idleID, err := d.EnqueueTask(TaskPurgeFile, "independent-fixture", "{}")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "running"} {
		if state == "running" {
			probe, err := d.ClaimTask(time.Now().Unix(), 100)
			if err != nil || probe.ID != probeID || !PassiveTask(probe) {
				t.Fatal("normal worker failed to claim passive probe", probe, err)
			}
		}
		busy, err := d.ColdSyncForegroundBusy(time.Now().Unix())
		if err != nil || busy {
			t.Fatal("automatic probe became foreground", state, err)
		}
		idle, err := d.ClaimIdleTask(time.Now().Unix(), 100)
		if err != nil || idle.ID != idleID {
			t.Fatal("automatic probe blocked idle task", state, idle, err)
		}
		if state == "pending" {
			if _, err := d.Exec(`UPDATE tasks SET state='pending' WHERE id=?`, idleID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestManualTelegramScanPromotesProbeWithoutLosingCursor(t *testing.T) {
	d := telegramTestDB(t)
	album := priorityScanAlbum(t, d)
	id, err := d.QueueLinkedAlbumScan(album, `{"automatic":true,"since":100,"until":200,"offset":50}`)
	if err != nil {
		t.Fatal(err)
	}
	// Claim it first: manual promotion must also affect a currently running probe.
	if _, err = d.ClaimTask(time.Now().Unix(), 100); err != nil {
		t.Fatal(err)
	}
	again, err := d.QueueLinkedAlbumScan(album, `{"automatic":false,"since":999}`)
	if err != nil || again != id {
		t.Fatal("manual request duplicated active scan", again, err)
	}
	if busy, err := d.ColdSyncForegroundBusy(time.Now().Unix()); err != nil || !busy {
		t.Fatal("promoted manual scan does not block idle", busy, err)
	}
	if _, err = d.EnqueueTask(TaskPurgeFile, "independent-fixture", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimIdleTask(time.Now().Unix(), 100); !errors.Is(err, ErrNotFound) {
		t.Fatal("idle task passed running manual scan", err)
	}
	// The probe's in-memory checkpoint still says automatic=true. It must not
	// erase the manual promotion committed after that payload was loaded.
	if err = d.TelegramScanProgress(album, id, 49, `{"automatic":true,"since":100,"until":200,"offset":49}`); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		task, err := d.GetTask(id)
		if err != nil || PassiveTask(task) {
			t.Fatal("manual scan was downgraded", err)
		}
		var p map[string]any
		if err = json.Unmarshal([]byte(task.Payload), &p); err != nil || p["since"] != float64(100) || p["until"] != float64(200) || p["offset"] != float64(49) {
			t.Fatal("manual join reset durable range or cursor", task.Payload, err)
		}
	}
	check()
	if resumed, err := d.ResumeTelegramScan(album, true); err != nil || resumed != id {
		t.Fatal("automatic resume replaced manual task", resumed, err)
	}
	check()
}

func TestAutomaticTelegramScanFailureResumeAndCancellationKeepIdentity(t *testing.T) {
	d := telegramTestDB(t)
	album := priorityScanAlbum(t, d)
	id, err := d.QueueLinkedAlbumScan(album, `{"automatic":true,"since":100,"offset":50}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='failed',attempts=8,last_error='interrupted page' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.ResumeTelegramScan(album, true)
	if err != nil || resumed != id {
		t.Fatal("automatic failure resume duplicated scan", resumed, err)
	}
	task, err := d.GetTask(id)
	if err != nil || task.State != "pending" || task.Attempts != 0 || !PassiveTask(task) {
		t.Fatal("automatic resume changed priority or failed to reset attempts", task, err)
	}
	if _, err = d.ResumeTelegramScan(album, false); err != nil {
		t.Fatal(err)
	}
	task, err = d.GetTask(id)
	if err != nil || PassiveTask(task) {
		t.Fatal("manual failure resume failed to promote", task, err)
	}
	if err = d.RequestTaskCancel(id); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ResumeTelegramScan(album, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("canceled task was resurrected", err)
	}
}
