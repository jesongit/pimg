package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const PerformanceRetention = 7 * 24 * time.Hour
const PerformanceInterval = 10 * time.Second
const maxMetricKeys = 256
const maxPendingDetails = 128

var latencyBounds = [...]float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 300000, 1800000, math.Inf(1)}

type metricKey struct {
	Minute     int64
	Kind, Name string
}
type aggregate struct {
	Count, Errors, Bytes int64
	TotalMS, MaxMS       float64
	Hist                 [len(latencyBounds)]int64
}
type Detail struct {
	Time       int64   `json:"time"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	DurationMS float64 `json:"duration_ms"`
	Status     int     `json:"status,omitempty"`
	Error      string  `json:"error,omitempty"`
	RequestID  string  `json:"request_id,omitempty"`
	TaskID     int64   `json:"task_id,omitempty"`
}
type sampleRecord struct {
	Time int64
	Body string
}

// Performance retains only a small pending write batch. Historical windows and
// details live in a separate on-disk database, never the business database.
type Performance struct {
	enabled          atomic.Bool
	epoch            atomic.Uint64
	control          sync.Mutex
	dataMu           sync.Mutex
	mu               sync.Mutex
	path, instance   string
	store            *performanceStore
	cancel           context.CancelFunc
	done             chan struct{}
	source           func(context.Context) map[string]any
	interval         time.Duration
	pending          map[metricKey]*aggregate
	details          []Detail
	inflight         map[string]int64
	dropped          int64
	lastError        string
	started, flushed time.Time
}

func NewPerformance(path string, source func(context.Context) map[string]any) *Performance {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	return &Performance{path: path, instance: hex.EncodeToString(id[:]), source: source, interval: PerformanceInterval}
}
func (p *Performance) Enabled() bool { return p != nil && p.enabled.Load() }

// Prepare validates and opens storage before the setting transaction commits.
func (p *Performance) Prepare() error {
	p.control.Lock()
	defer p.control.Unlock()
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	return p.openStore()
}
func (p *Performance) openStore() error {
	if p.store != nil {
		return nil
	}
	s, err := openPerformanceStore(p.path)
	if err != nil {
		p.setError("无法打开性能诊断数据库")
		return fmt.Errorf("无法打开性能诊断数据库: %w", err)
	}
	p.store = s
	p.setError("")
	return nil
}

func (p *Performance) SetEnabled(enabled bool) error {
	p.control.Lock()
	defer p.control.Unlock()
	if enabled {
		if p.Enabled() {
			return nil
		}
		p.dataMu.Lock()
		err := p.openStore()
		p.dataMu.Unlock()
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.pending = make(map[metricKey]*aggregate)
		p.details = nil
		p.inflight = make(map[string]int64)
		p.started = time.Now().UTC()
		p.dropped = 0
		p.mu.Unlock()
		p.epoch.Add(1)
		p.enabled.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		p.done = make(chan struct{})
		go p.loop(ctx, p.done)
		return nil
	}
	p.enabled.Store(false)
	p.epoch.Add(1)
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
	p.mu.Lock()
	p.pending = nil
	p.details = nil
	p.inflight = nil
	p.started = time.Time{}
	p.flushed = time.Time{}
	p.dropped = 0
	p.mu.Unlock()
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	if p.store != nil {
		err := p.store.close()
		p.store = nil
		if err != nil {
			p.setError("调试已关闭，但诊断数据库关闭失败")
			return err
		}
	}
	if err := purgePerformanceStore(p.path); err != nil {
		p.setError("调试已关闭，但性能诊断数据清理失败，请检查诊断目录权限后重新保存关闭设置")
		return err
	}
	p.setError("")
	return nil
}

// Close preserves enabled-mode history for the next process. Turning the switch
// off uses SetEnabled(false), which instead removes all performance history.
func (p *Performance) Close() error {
	if p == nil {
		return nil
	}
	p.control.Lock()
	defer p.control.Unlock()
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
	if p.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = p.Flush(ctx)
		cancel()
	}
	p.enabled.Store(false)
	p.epoch.Add(1)
	p.mu.Lock()
	p.pending = nil
	p.details = nil
	p.inflight = nil
	p.mu.Unlock()
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	if p.store != nil {
		err := p.store.close()
		p.store = nil
		return err
	}
	return nil
}

func (p *Performance) State() map[string]any {
	if p == nil {
		return map[string]any{"enabled": false}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	active := map[string]int64{}
	for k, v := range p.inflight {
		active[k] = v
	}
	return map[string]any{"enabled": p.Enabled(), "instance_id": p.instance, "started_at": p.started, "flushed_at": p.flushed, "retention_days": 7, "sample_interval_seconds": 10, "pending_metric_keys": len(p.pending), "pending_details": len(p.details), "dropped_observations": p.dropped, "in_flight": active, "error": p.lastError}
}
func (p *Performance) setError(message string) { p.mu.Lock(); p.lastError = message; p.mu.Unlock() }

type contextKey int

const (
	requestContext contextKey = iota
	taskContext
	suppressedContext
)

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestContext, id)
}
func WithTaskID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, taskContext, id)
}
func WithoutPerformance(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressedContext, true)
}
func RequestID(ctx context.Context) string { v, _ := ctx.Value(requestContext).(string); return v }

type Span struct {
	p                   *Performance
	epoch               uint64
	start               time.Time
	kind, name, request string
	task                int64
	once                sync.Once
}

func (s *Span) SetName(name string) {
	if s != nil {
		s.name = name
	}
}

func (p *Performance) Start(ctx context.Context, kind, name string) *Span {
	if !p.Enabled() {
		return nil
	}
	if v, _ := ctx.Value(suppressedContext).(bool); v {
		return nil
	}
	s := &Span{p: p, epoch: p.epoch.Load(), start: time.Now(), kind: kind, name: name, request: RequestID(ctx)}
	s.task, _ = ctx.Value(taskContext).(int64)
	p.mu.Lock()
	if !p.Enabled() || s.epoch != p.epoch.Load() {
		p.mu.Unlock()
		return nil
	}
	p.inflight[kind]++
	p.mu.Unlock()
	return s
}
func (s *Span) Finish(err error) { s.FinishResult(err, 0, 0) }
func (s *Span) FinishResult(err error, status int, bytes int64) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		p := s.p
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.Enabled() || s.epoch != p.epoch.Load() {
			return
		}
		p.inflight[s.kind]--
		p.recordLocked(s.kind, s.name, time.Since(s.start), err, status, bytes, s.request, s.task)
	})
}
func (p *Performance) Observe(kind, name string, duration time.Duration, bytes int64) {
	p.ObserveResult(kind, name, duration, bytes, nil)
}
func (p *Performance) ObserveResult(kind, name string, duration time.Duration, bytes int64, err error) {
	if !p.Enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Enabled() {
		p.recordLocked(kind, name, duration, err, 0, bytes, "", 0)
	}
}
func (p *Performance) recordLocked(kind, name string, duration time.Duration, err error, status int, bytes int64, request string, task int64) {
	now := time.Now()
	key := metricKey{now.Unix() / 60 * 60, kind, Clip(name, 160)}
	a := p.pending[key]
	if a == nil {
		if len(p.pending) >= maxMetricKeys {
			p.dropped++
			return
		}
		a = &aggregate{}
		p.pending[key] = a
	}
	ms := math.Max(0, float64(duration)/float64(time.Millisecond))
	a.Count++
	a.Bytes += bytes
	a.TotalMS += ms
	if ms > a.MaxMS {
		a.MaxMS = ms
	}
	for i, bound := range latencyBounds {
		if ms <= bound {
			a.Hist[i]++
			break
		}
	}
	category := ""
	if err != nil && !errors.Is(err, context.Canceled) {
		category = "operation_error"
		var coded interface{ Code() int }
		if errors.As(err, &coded) {
			category = fmt.Sprintf("sqlite_%d", coded.Code()&255)
		}
	}
	if status >= 400 && category == "" {
		category = "http_error"
	}
	if category != "" {
		a.Errors++
	}
	threshold := 500.0
	if kind == "database" {
		threshold = 200
	}
	if kind == "task" || kind == "stage" {
		threshold = 5000
	}
	if ms >= threshold || category != "" {
		if len(p.details) < maxPendingDetails {
			p.details = append(p.details, Detail{now.UnixMilli(), kind, key.Name, ms, status, category, request, task})
		} else {
			p.dropped++
		}
	}
}

// Database operation names come from compiled function names, never SQL text,
// parameter values, file names or request URLs.
func CallerOperation() string {
	var pcs [10]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, "pimg/") && !strings.Contains(f.Function, "internal/diagnostics.") && !strings.HasSuffix(f.Function, ".performanceSpan") && !strings.HasSuffix(f.Function, ".ReadQuery") && !strings.HasSuffix(f.Function, ".ReadQueryContext") && !strings.HasSuffix(f.Function, ".ReadQueryRow") && !strings.HasSuffix(f.Function, ".Exec") && !strings.HasSuffix(f.Function, ".ExecContext") {
			return strings.TrimPrefix(f.Function, "pimg/internal/")
		}
		if !more {
			break
		}
	}
	return "database.operation"
}

func (p *Performance) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTicker(p.interval)
	defer timer.Stop()
	var previousTime time.Time
	var previousCPU float64
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !p.Enabled() {
				continue
			}
			sample := map[string]any{}
			if p.source != nil {
				child, cancel := context.WithTimeout(WithoutPerformance(ctx), time.Second)
				sample = p.source(child)
				cancel()
			}
			now := time.Now()
			if cpu, ok := sample["process_cpu_seconds"].(float64); ok {
				if !previousTime.IsZero() {
					sample["process_cpu_core_percent"] = math.Max(0, (cpu-previousCPU)/now.Sub(previousTime).Seconds()*100)
				}
				previousTime = now
				previousCPU = cpu
			}
			body := marshalSample(sample)
			child, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = p.flush(child, &sampleRecord{time.Now().Unix(), body})
			cancel()
		}
	}
}
func (p *Performance) Flush(ctx context.Context) error { return p.flush(ctx, nil) }
func (p *Performance) flush(ctx context.Context, sample *sampleRecord) error {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	if !p.Enabled() || p.store == nil {
		return nil
	}
	p.mu.Lock()
	batch, details := p.pending, p.details
	p.pending = make(map[metricKey]*aggregate)
	p.details = nil
	p.mu.Unlock()
	if err := p.store.write(ctx, p.instance, batch, details, sample); err != nil {
		p.mu.Lock()
		if p.Enabled() {
			for k, v := range batch {
				a := p.pending[k]
				if a == nil {
					if len(p.pending) >= maxMetricKeys {
						p.dropped += v.Count
						continue
					}
					p.pending[k] = v
					continue
				}
				a.Count += v.Count
				a.Errors += v.Errors
				a.Bytes += v.Bytes
				a.TotalMS += v.TotalMS
				if v.MaxMS > a.MaxMS {
					a.MaxMS = v.MaxMS
				}
				for i, n := range v.Hist {
					a.Hist[i] += n
				}
			}
			for _, d := range details {
				if len(p.details) < maxPendingDetails {
					p.details = append(p.details, d)
				} else {
					p.dropped++
				}
			}
			p.lastError = "性能诊断数据写入失败，已保留有限待写数据以重试"
		}
		p.mu.Unlock()
		return err
	}
	p.mu.Lock()
	p.flushed = time.Now().UTC()
	p.lastError = ""
	p.mu.Unlock()
	return nil
}
