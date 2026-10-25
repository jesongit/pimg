// Package proxytraffic accounts for the bytes sent and received on proxy sockets.
package proxytraffic

import (
	"errors"
	"net"
	"sync"
	"time"
)

const GB int64 = 1_000_000_000

var ErrLimit = errors.New("代理流量已达上限，等待额度刷新后继续")

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

type State struct {
	DailyLimit      int64  `json:"daily_limit_bytes"`
	MonthlyLimit    int64  `json:"monthly_limit_bytes"`
	DailyUsed       int64  `json:"daily_used_bytes"`
	MonthlyUsed     int64  `json:"monthly_used_bytes"`
	DailyReset      string `json:"daily_reset_time"`
	MonthlyResetDay int    `json:"monthly_reset_day"`
	DayStart        int64  `json:"day_start"`
	MonthStart      int64  `json:"month_start"`
}

type View struct {
	State
	Blocked         bool   `json:"blocked"`
	DailyResetsAt   int64  `json:"daily_resets_at"`
	MonthlyResetsAt int64  `json:"monthly_resets_at"`
	ResumeAt        int64  `json:"resume_at,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Pointer fields distinguish omitted usage from an explicit correction to zero.
// Period tokens prevent a stale form from overwriting usage in a new cycle.
type Patch struct {
	DailyLimit      *int64  `json:"daily_limit_bytes"`
	MonthlyLimit    *int64  `json:"monthly_limit_bytes"`
	DailyUsed       *int64  `json:"daily_used_bytes"`
	MonthlyUsed     *int64  `json:"monthly_used_bytes"`
	DailyReset      *string `json:"daily_reset_time"`
	MonthlyResetDay *int    `json:"monthly_reset_day"`
	DayStart        *int64  `json:"day_start"`
	MonthStart      *int64  `json:"month_start"`
}

type Meter struct {
	mu    sync.Mutex
	state State
	now   func() time.Time
	save  func(State) error
	conns map[*Conn]struct{}
	err   error
}

func DefaultState() State {
	return State{DailyLimit: 10 * GB, MonthlyLimit: 300 * GB, DailyReset: "00:00", MonthlyResetDay: 1}
}

func Validate(s State) error {
	if s.DailyLimit <= 0 || s.MonthlyLimit <= 0 || s.DailyLimit > 1_000_000*GB || s.MonthlyLimit > 1_000_000*GB {
		return errors.New("日/月代理额度应大于 0 且不超过 1000000 GB")
	}
	if s.DailyUsed < 0 || s.MonthlyUsed < 0 || s.DailyUsed > 1_000_000*GB || s.MonthlyUsed > 1_000_000*GB {
		return errors.New("日/月已用流量应为 0–1000000 GB")
	}
	t, err := time.Parse("15:04", s.DailyReset)
	if err != nil || t.Format("15:04") != s.DailyReset {
		return errors.New("日刷新时间应为 HH:MM")
	}
	if s.MonthlyResetDay < 1 || s.MonthlyResetDay > 31 {
		return errors.New("月刷新日期应为 1–31 日")
	}
	return nil
}

func New(s State, save func(State) error) (*Meter, error) {
	if err := Validate(s); err != nil {
		return nil, err
	}
	m := &Meter{state: s, save: save, now: time.Now, conns: map[*Conn]struct{}{}}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rollLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func monthBoundary(t time.Time, day int) time.Time {
	last := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, beijing).Day()
	if day > last {
		day = last
	}
	return time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, beijing)
}

func boundaries(s State, now time.Time) (day, nextDay, month, nextMonth time.Time) {
	now = now.In(beijing)
	t, _ := time.Parse("15:04", s.DailyReset)
	day = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, beijing)
	if now.Before(day) {
		day = day.AddDate(0, 0, -1)
	}
	nextDay = day.AddDate(0, 0, 1)
	month = monthBoundary(now, s.MonthlyResetDay)
	if now.Before(month) {
		month = monthBoundary(time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, beijing), s.MonthlyResetDay)
	}
	nextMonth = monthBoundary(time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, beijing), s.MonthlyResetDay)
	return
}

func (m *Meter) persistLocked() error {
	if m.save != nil {
		if err := m.save(m.state); err != nil {
			m.err = errors.New("代理流量保存失败，已暂停代理传输")
			return m.err
		}
	}
	m.err = nil
	return nil
}

func (m *Meter) rollLocked() error {
	d, _, mo, _ := boundaries(m.state, m.now())
	changed := false
	if d.Unix() > m.state.DayStart {
		m.state.DayStart, m.state.DailyUsed = d.Unix(), 0
		changed = true
	}
	if mo.Unix() > m.state.MonthStart {
		m.state.MonthStart, m.state.MonthlyUsed = mo.Unix(), 0
		changed = true
	}
	if changed || m.err != nil {
		return m.persistLocked()
	}
	return nil
}

func (m *Meter) viewLocked() View {
	_, d, _, mo := boundaries(m.state, m.now())
	v := View{State: m.state, DailyResetsAt: d.Unix(), MonthlyResetsAt: mo.Unix()}
	if m.state.DailyUsed >= m.state.DailyLimit {
		v.Blocked = true
		v.ResumeAt = d.Unix()
	}
	if m.state.MonthlyUsed >= m.state.MonthlyLimit {
		v.Blocked = true
		if mo.Unix() > v.ResumeAt {
			v.ResumeAt = mo.Unix()
		}
	}
	if m.err != nil {
		v.Blocked = true
		v.Error = m.err.Error()
	}
	return v
}

func (m *Meter) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.rollLocked()
	return m.viewLocked()
}

func (m *Meter) Check() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rollLocked(); err != nil {
		return err
	}
	if m.viewLocked().Blocked {
		return ErrLimit
	}
	return nil
}

func (m *Meter) Update(p Patch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rollLocked(); err != nil {
		return err
	}
	old, s := m.state, m.state
	if p.DailyUsed != nil && (p.DayStart == nil || *p.DayStart != s.DayStart) {
		return errors.New("日流量周期已更新，请刷新后重新校准")
	}
	if p.MonthlyUsed != nil && (p.MonthStart == nil || *p.MonthStart != s.MonthStart) {
		return errors.New("月流量周期已更新，请刷新后重新校准")
	}
	if p.DailyLimit != nil {
		s.DailyLimit = *p.DailyLimit
	}
	if p.MonthlyLimit != nil {
		s.MonthlyLimit = *p.MonthlyLimit
	}
	if p.DailyUsed != nil {
		s.DailyUsed = *p.DailyUsed
	}
	if p.MonthlyUsed != nil {
		s.MonthlyUsed = *p.MonthlyUsed
	}
	if p.DailyReset != nil {
		s.DailyReset = *p.DailyReset
	}
	if p.MonthlyResetDay != nil {
		s.MonthlyResetDay = *p.MonthlyResetDay
	}
	if err := Validate(s); err != nil {
		return err
	}
	// Changing the schedule retains the current counters; it is not a reset.
	d, _, mo, _ := boundaries(s, m.now())
	s.DayStart = d.Unix()
	s.MonthStart = mo.Unix()
	m.state = s
	if err := m.persistLocked(); err != nil {
		m.state = old
		return err
	}
	if m.viewLocked().Blocked {
		m.closeLocked()
	}
	return nil
}

func (m *Meter) closeLocked() {
	for c := range m.conns {
		_ = c.Conn.Close()
	}
}

func (m *Meter) Wrap(c net.Conn) net.Conn {
	if m == nil {
		return c
	}
	w := &Conn{Conn: c, meter: m}
	m.mu.Lock()
	m.conns[w] = struct{}{}
	m.mu.Unlock()
	return w
}

// Each in-flight socket operation is bounded to 32 KiB. Reads already in the
// kernel can finish concurrently at the limit; their actual bytes are counted.
func (m *Meter) allowance(size int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rollLocked(); err != nil {
		return 0, err
	}
	remaining := min(m.state.DailyLimit-m.state.DailyUsed, m.state.MonthlyLimit-m.state.MonthlyUsed)
	if remaining <= 0 {
		return 0, ErrLimit
	}
	return int(min(int64(size), remaining, 32<<10)), nil
}

func (m *Meter) add(n int) error {
	if n == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Even if a rollover write failed, never drop bytes already transferred.
	_ = m.rollLocked()
	m.state.DailyUsed += int64(n)
	m.state.MonthlyUsed += int64(n)
	err := m.persistLocked()
	if err != nil || m.viewLocked().Blocked {
		m.closeLocked()
	}
	return err
}

type Conn struct {
	net.Conn
	meter *Meter
}

func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	size, err := c.meter.allowance(len(p))
	if err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p[:size])
	if saveErr := c.meter.add(n); saveErr != nil {
		return n, saveErr
	}
	return n, err
}

func (c *Conn) Write(p []byte) (total int, err error) {
	for len(p) > 0 {
		size, e := c.meter.allowance(len(p))
		if e != nil {
			return total, e
		}
		n, e := c.Conn.Write(p[:size])
		total += n
		p = p[n:]
		if saveErr := c.meter.add(n); saveErr != nil {
			return total, saveErr
		}
		if e != nil {
			return total, e
		}
		if n == 0 {
			return total, errors.New("代理连接未写入数据")
		}
	}
	return total, nil
}

func (c *Conn) Close() error {
	err := c.Conn.Close()
	c.meter.mu.Lock()
	delete(c.meter.conns, c)
	c.meter.mu.Unlock()
	return err
}
