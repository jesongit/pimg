package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestPressureExpiryPreemptsIdleFileLock(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunTempMaterialize(context.Background(), f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE temp_objects SET expires_at=1; UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	unlock, err := s.lockFile(idle, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { <-idle.Done(); unlock(); close(released) }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	n, err := s.CleanupExpiredTemp(ctx)
	if err != nil || n != 1 || time.Since(start) > 750*time.Millisecond {
		t.Errorf("expiry queued behind idle sync: count=%d time=%v err=%v", n, time.Since(start), err)
	}
	finish()
	<-released
	if !errors.Is(idle.Err(), context.Canceled) {
		t.Fatal(idle.Err())
	}
}

func TestPressureBusyExpiryFileDoesNotStarveOtherFiles(t *testing.T) {
	s, _, plain := peerFixture(t)
	busy := archiveMedia(t, s, plain)
	other, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(append(append([]byte{}, plain...), 1)), "other.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{busy.ID, other.ID} {
		if err = s.RunTempMaterialize(context.Background(), id, "original"); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := s.DB.FileTempObjects(busy.ID)
	if err != nil || len(objs) != 1 {
		t.Fatal(err, len(objs))
	}
	for i := 0; i < 3; i++ {
		clone := *objs[0]
		clone.ID = string(rune('a' + i))
		if err = s.DB.InsertTempObject(&clone); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec(`UPDATE temp_objects SET expires_at=CASE WHEN file_id=? THEN 1 ELSE 2 END`, busy.ID); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lockFile(context.Background(), busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := s.CleanupExpiredTemp(context.Background())
	unlock()
	if n != 1 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 1500*time.Millisecond {
		t.Fatal("busy file blocked unrelated cleanup", n, time.Since(start), err)
	}
	if n, err = s.CleanupExpiredTemp(context.Background()); err != nil || n != 4 {
		t.Fatal("busy objects were not retained for retry", n, err)
	}
}

func TestPressureShortTransfersKeepIdleProgress(t *testing.T) {
	s, _, _ := peerFixture(t)
	if _, err := s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	for i := 0; i < 8; i++ {
		done := s.BeginForegroundTransfer()
		if _, _, err = s.BeginIdle(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatal("new idle work started during foreground transfer", err)
		}
		time.Sleep(25 * time.Millisecond)
		done()
	}
	time.Sleep(foregroundYieldGrace + 25*time.Millisecond)
	if idle.Err() != nil {
		t.Fatal("short requests repeatedly discarded idle transfer progress", idle.Err())
	}
}

func TestPressureSustainedAndDestructiveWorkStillPreempts(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "sustained", true: "destructive"}[hard], func(t *testing.T) {
			s, _, _ := peerFixture(t)
			if _, err := s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
				t.Fatal(err)
			}
			idle, finish, err := s.BeginIdle(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			var done func()
			if hard {
				done = s.BeginForeground()
			} else {
				done = s.BeginForegroundTransfer()
			}
			defer done()
			if hard && idle.Err() != context.Canceled {
				t.Fatal("destructive work did not immediately preempt")
			}
			select {
			case <-idle.Done():
			case <-time.After(750 * time.Millisecond):
				t.Fatal("sustained foreground did not preempt")
			}
			finish()
			if _, _, err = s.BeginIdle(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal("idle restarted with active foreground", err)
			}
		})
	}
}

func TestPressureOldYieldTimerCannotCancelNewIdle(t *testing.T) {
	s, _, _ := peerFixture(t)
	if _, err := s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	_, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := s.BeginForegroundTransfer()
	done()
	finish()
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	time.Sleep(foregroundYieldGrace + 50*time.Millisecond)
	if idle.Err() != nil {
		t.Fatal("old timer canceled a new idle operation")
	}
}

func TestPressureArchiveCompletesAcrossShortForegroundRequests(t *testing.T) {
	s, peer, plain := peerFixture(t)
	p := s.StoragePolicy()
	p.Archives = []string{"local", peer}
	p.NormalMB = 1
	p.AcceleratedMB = 1
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f := archiveMedia(t, s, append(append([]byte{}, plain...), bytes.Repeat([]byte{7}, 256<<10)...))
	if _, err := s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tk := time.NewTicker(25 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				done := s.BeginForegroundTransfer()
				time.Sleep(5 * time.Millisecond)
				done()
			}
		}
	}()
	err = s.RunArchiveSync(idle, f.ID)
	close(stop)
	<-stopped
	if err != nil {
		t.Fatal("frequent short requests prevented useful replication", err)
	}
	objects, err := s.DB.ArchiveObjects(f.ID, peer)
	if err != nil || len(objects) == 0 {
		t.Fatal(err, len(objects))
	}
	for _, o := range objects {
		if !o.Verified {
			t.Fatal("unverified copy after sync", o.Kind)
		}
	}
}
