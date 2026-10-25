package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"pimg/internal/proxytraffic"
)

func TestProxyRouteChangeWakesWaitWithoutClearingUsage(t *testing.T) {
	s, _, _ := newTestEnv(t)
	proxy := "http://127.0.0.1:1"
	if err := s.SaveStorageConnection(context.Background(), &proxy, nil, nil); err != nil {
		t.Fatal(err)
	}
	v := s.ProxyTrafficView()
	used := v.DailyLimit
	if err := s.SaveProxyTraffic(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	id, err := s.DB.EnqueueTask("quota_fixture", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.DB.Exec(`UPDATE tasks SET state='running',attempts=1 WHERE id=?`, id)
	if err := s.PostponeProxyTrafficTask(id); err != nil {
		t.Fatal(err)
	}
	direct := ""
	if err := s.SaveStorageConnection(context.Background(), &direct, nil, nil); err != nil {
		t.Fatal(err)
	}
	task, _ := s.DB.GetTask(id)
	if task.State != "pending" || task.RunAfter > time.Now().Unix() || task.Attempts != 0 {
		t.Fatal("route change did not recheck waiting task", task)
	}
	if s.ProxyTrafficView().DailyUsed != used {
		t.Fatal("route change cleared usage")
	}
}

func TestProxyTrafficWaitPersistenceCorrectionAndResume(t *testing.T) {
	s, _, _ := newTestEnv(t)
	v := s.ProxyTrafficView()
	used := 10 * proxytraffic.GB
	reset := "08:30"
	day := 23
	if err := s.SaveProxyTraffic(proxytraffic.Patch{DailyUsed: &used, DayStart: &v.DayStart, DailyReset: &reset, MonthlyResetDay: &day}); err != nil {
		t.Fatal(err)
	}
	if !s.ProxyTrafficView().Blocked {
		t.Fatal("not blocked")
	}
	id, err := s.DB.EnqueueTask("quota_fixture", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`UPDATE tasks SET state='running',attempts=4 WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PostponeProxyTrafficTask(id); err != nil {
		t.Fatal(err)
	}
	task, _ := s.DB.GetTask(id)
	if task.State != "pending" || task.Attempts != 3 || task.LeaseUntil.Valid || task.RunAfter <= time.Now().Unix() || !strings.HasPrefix(task.LastError.String, proxyTrafficWait) {
		t.Fatal("wait not durable", task)
	}
	// Reconstruct just the meter from SQLite, as service startup does.
	s.proxyTraffic = nil
	if err := s.initProxyTraffic(); err != nil {
		t.Fatal(err)
	}
	v = s.ProxyTrafficView()
	if v.DailyUsed != used || v.DailyReset != reset || v.MonthlyResetDay != day || !v.Blocked {
		t.Fatal("restart lost configuration", v)
	}
	if err := s.RefreshProxyTrafficTasks(); err != nil {
		t.Fatal(err)
	}
	task, _ = s.DB.GetTask(id)
	if task.RunAfter <= time.Now().Unix() {
		t.Fatal("resumed before quota available")
	}
	limit := int64(11 * proxytraffic.GB)
	if err := s.SaveProxyTraffic(proxytraffic.Patch{DailyLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	task, _ = s.DB.GetTask(id)
	if task.State != "pending" || task.Attempts != 3 || task.RunAfter > time.Now().Unix() || task.LastError.Valid {
		t.Fatal("budget increase did not wake wait", task)
	}
	v = s.ProxyTrafficView()
	zero := int64(0)
	if err := s.SaveProxyTraffic(proxytraffic.Patch{DailyUsed: &zero, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	if s.ProxyTrafficView().DailyUsed != 0 {
		t.Fatal("manual zero failed")
	}
	// Explicit cancellation during a quota wait cannot be revived by a save.
	_, _ = s.DB.Exec(`UPDATE tasks SET state='running',attempts=1,cancel_requested=1 WHERE id=?`, id)
	if err := s.PostponeProxyTrafficTask(id); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshProxyTrafficTasks(); err != nil {
		t.Fatal(err)
	}
	task, _ = s.DB.GetTask(id)
	if task.State != "canceled" {
		t.Fatal("canceled wait resurrected", task)
	}
}
