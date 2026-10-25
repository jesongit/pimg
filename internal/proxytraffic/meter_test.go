package proxytraffic

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func at(s string) time.Time { t, _ := time.ParseInLocation("2006-01-02 15:04", s, beijing); return t }
func testMeter(t *testing.T, now *time.Time, save func(State) error) *Meter {
	t.Helper()
	m := &Meter{state: DefaultState(), now: func() time.Time { return *now }, save: save, conns: map[*Conn]struct{}{}}
	if err := m.rollLocked(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConfiguredBoundaries(t *testing.T) {
	s := DefaultState()
	s.DailyReset = "06:30"
	s.MonthlyResetDay = 31
	for _, tc := range []struct{ now, day, nextDay, month, nextMonth string }{
		{"2026-02-28 06:29", "2026-02-27 06:30", "2026-02-28 06:30", "2026-02-28 00:00", "2026-03-31 00:00"},
		{"2026-03-01 06:30", "2026-03-01 06:30", "2026-03-02 06:30", "2026-02-28 00:00", "2026-03-31 00:00"},
		{"2028-02-28 08:00", "2028-02-28 06:30", "2028-02-29 06:30", "2028-01-31 00:00", "2028-02-29 00:00"},
		{"2026-12-31 23:59", "2026-12-31 06:30", "2027-01-01 06:30", "2026-12-31 00:00", "2027-01-31 00:00"},
	} {
		t.Run(tc.now, func(t *testing.T) {
			d, nd, m, nm := boundaries(s, at(tc.now).UTC())
			for i, pair := range [][2]time.Time{{d, at(tc.day)}, {nd, at(tc.nextDay)}, {m, at(tc.month)}, {nm, at(tc.nextMonth)}} {
				if !pair[0].Equal(pair[1]) {
					t.Fatalf("boundary %d: %v want %v", i, pair[0], pair[1])
				}
			}
		})
	}
}

func TestDailyMonthlyIndependentResetAndScheduleEdit(t *testing.T) {
	now := at("2026-10-25 08:00")
	var saved State
	m := testMeter(t, &now, func(s State) error { saved = s; return nil })
	if m.View().DailyLimit != 10*GB || m.View().MonthlyLimit != 300*GB {
		t.Fatal("defaults")
	}
	v := m.View()
	used := int64(10 * GB)
	monthUsed := int64(299 * GB)
	if err := m.Update(Patch{DailyUsed: &used, MonthlyUsed: &monthUsed, DayStart: &v.DayStart, MonthStart: &v.MonthStart}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.Check(), ErrLimit) {
		t.Fatal("daily cap not enforced")
	}
	now = at("2026-10-26 00:00")
	v = m.View()
	if v.Blocked || v.DailyUsed != 0 || v.MonthlyUsed != monthUsed {
		t.Fatal("day reset cleared month or failed", v)
	}
	if err := m.add(int(GB)); err != nil {
		t.Fatal(err)
	}
	v = m.View()
	if !v.Blocked || v.ResumeAt != at("2026-11-01 00:00").Unix() {
		t.Fatal("monthly cap", v)
	}
	reset := "09:15"
	day := 15
	if err := m.Update(Patch{DailyReset: &reset, MonthlyResetDay: &day}); err != nil {
		t.Fatal(err)
	}
	if m.View().DailyUsed != GB || saved.MonthlyUsed != 300*GB {
		t.Fatal("schedule edit reset counters")
	}
	now = at("2026-11-15 00:00")
	v = m.View()
	if v.MonthlyUsed != 0 || v.DailyUsed != 0 || v.Blocked {
		t.Fatal("missed multiple boundaries", v)
	}
}

func TestStaleCorrectionValidationAndPersistence(t *testing.T) {
	now := at("2026-10-25 12:00")
	var saved State
	m := testMeter(t, &now, func(s State) error { saved = s; return nil })
	_ = m.add(1234)
	v := m.View()
	zero := int64(0)
	if err := m.Update(Patch{DailyUsed: &zero}); err == nil {
		t.Fatal("missing period token accepted")
	}
	now = at("2026-10-26 00:00")
	if err := m.Update(Patch{DailyUsed: &zero, DayStart: &v.DayStart}); err == nil {
		t.Fatal("stale period accepted")
	}
	for _, tc := range []Patch{{DailyLimit: &zero}, {MonthlyResetDay: new(32)}, {DailyReset: new("24:00")}, {DailyReset: new("6:30")}, {MonthlyUsed: new(int64(-1))}} {
		if err := m.Update(tc); err == nil {
			t.Fatal("invalid patch accepted", tc)
		}
	}
	v = m.View()
	n := int64(4567)
	if err := m.Update(Patch{DailyUsed: &n, DayStart: &v.DayStart}); err != nil {
		t.Fatal(err)
	}
	// Loading a saved state in the same cycle retains exact byte counts.
	loaded := &Meter{state: saved, now: func() time.Time { return now }, conns: map[*Conn]struct{}{}}
	if got := loaded.View(); got.DailyUsed != n || got.MonthlyUsed != 1234 {
		t.Fatal("saved counters lost", got)
	}
}

func TestSocketActualBytesAndCap(t *testing.T) {
	now := at("2026-10-25 12:00")
	m := testMeter(t, &now, nil)
	if err := m.Update(Patch{DailyLimit: new(int64(10))}); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer b.Close()
	c := m.Wrap(a)
	defer c.Close()
	go func() { _, _ = b.Write([]byte("abc")); _, _ = io.ReadFull(b, make([]byte, 7)) }()
	buf := make([]byte, 3)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	n, err := c.Write([]byte("123456789"))
	if n != 7 || !errors.Is(err, ErrLimit) {
		t.Fatal("cap did not interrupt write", n, err)
	}
	if v := m.View(); v.DailyUsed != 10 || v.MonthlyUsed != 10 || !v.Blocked {
		t.Fatal("actual byte count", v)
	}
}

func TestConcurrentAccountingAndFailureClosed(t *testing.T) {
	now := at("2026-10-25 12:00")
	fail := false
	m := testMeter(t, &now, func(State) error {
		if fail {
			return errors.New("disk full")
		}
		return nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				_ = m.add(71)
			}
		})
	}
	wg.Wait()
	if m.View().DailyUsed != 8*20*71 {
		t.Fatal("lost concurrent bytes")
	}
	fail = true
	if m.add(3) == nil || !m.View().Blocked || m.Check() == nil {
		t.Fatal("storage failure allowed traffic")
	}
	fail = false
	if err := m.Check(); err != nil {
		t.Fatal("save retry did not recover", err)
	}
}

func TestTrackedProxyCancelsButLocalContextSurvives(t *testing.T) {
	now := at("2026-10-25 12:00")
	m := testMeter(t, &now, nil)
	ctx, cancel := Track(context.Background())
	defer cancel()
	local, stop := Track(context.Background())
	defer stop()
	request, finishRequest := context.WithCancel(ctx)
	if err := Mark(request, m); err != nil {
		t.Fatal(err)
	}
	finishRequest() // A completed request must not stop tracking the whole task.
	_ = m.add(int(10 * GB))
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrLimit) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("proxy operation did not pause")
	}
	if local.Err() != nil || PauseError(local) != nil {
		t.Fatal("local task was paused")
	}
}
