package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestPerformance(t *testing.T) *Performance {
	t.Helper()
	p := NewPerformance(filepath.Join(t.TempDir(), "performance.sqlite"), nil)
	p.interval = time.Hour
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func TestPerformanceDefaultOffCreatesNothingAndStopsAllCollection(t *testing.T) {
	var samples atomic.Int64
	p := NewPerformance(filepath.Join(t.TempDir(), "diagnostics", "performance.sqlite"), func(context.Context) map[string]any { samples.Add(1); return map[string]any{"test": true} })
	p.interval = 5 * time.Millisecond
	defer p.Close()
	if p.Start(t.Context(), "http", "test") != nil {
		t.Fatal("disabled span allocated")
	}
	p.Observe("http", "test", time.Second, 0)
	time.Sleep(15 * time.Millisecond)
	if samples.Load() != 0 {
		t.Fatal("disabled sampler ran")
	}
	if _, err := os.Stat(p.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default created database", err)
	}
	if err := p.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for samples.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if samples.Load() == 0 {
		t.Fatal("enabled sampler never ran")
	}
	if err := p.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	count := samples.Load()
	time.Sleep(20 * time.Millisecond)
	if samples.Load() != count {
		t.Fatal("sampler continued after disable")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Stat(p.path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("performance file retained", suffix, err)
		}
	}
	if p.pending != nil || p.details != nil || p.inflight != nil {
		t.Fatal("disabled memory retained")
	}
}
func TestPerformancePersistsAcrossRestartThenPurgesAndRejectsOldSpans(t *testing.T) {
	p := newTestPerformance(t)
	if err := p.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		p.Observe("http", "GET /api/admin/files/{id}", time.Duration(i+1)*10*time.Millisecond, 8)
	}
	p.ObserveResult("database", "dbx.ListFiles", time.Second, 0, errors.New("password=never-export-this"))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := p.Query(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	metrics := data["metrics"].([]MetricSummary)
	if len(metrics) != 2 {
		t.Fatal(metrics)
	}
	var httpMetric MetricSummary
	for _, m := range metrics {
		if m.Kind == "http" {
			httpMetric = m
		}
	}
	if httpMetric.Count != 100 || httpMetric.Bytes != 800 || httpMetric.P95MS < 950 || httpMetric.P99MS < 990 {
		t.Fatal(httpMetric)
	}
	for _, d := range data["details"].([]any) {
		if strings.Contains(marshalSample(d.(map[string]any)), "never-export-this") {
			t.Fatal("raw error exported")
		}
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewPerformance(p.path, nil)
	restarted.interval = time.Hour
	defer restarted.Close()
	if err = restarted.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	data, err = restarted.Query(t.Context(), time.Hour)
	if err != nil || len(data["metrics"].([]MetricSummary)) != 2 {
		t.Fatal(data, err)
	}
	stale := restarted.Start(t.Context(), "task", "old-attempt")
	if err = restarted.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err = restarted.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	stale.Finish(nil)
	if err = restarted.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err = restarted.Query(t.Context(), time.Hour)
	if err != nil || len(data["metrics"].([]MetricSummary)) != 0 {
		t.Fatal("history or old span survived cleanup", data, err)
	}
}
func TestPerformanceRetentionBoundsAndReadOnlySummary(t *testing.T) {
	p := newTestPerformance(t)
	if err := p.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		p.Observe("http", strings.Repeat("x", i+1), time.Second, 0)
	}
	if len(p.pending) > maxMetricKeys || len(p.details) > maxPendingDetails {
		t.Fatal("unbounded pending memory")
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.db.Exec(`INSERT INTO resource_samples(ts,instance,body) VALUES(1,'old','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.db.Exec(`INSERT INTO details(ts,instance,body) VALUES(1,'old','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	p.store.db.QueryRow(`SELECT COUNT(*) FROM resource_samples WHERE ts=1`).Scan(&n)
	if n != 0 {
		t.Fatal("old sample retained")
	}
	p.store.db.QueryRow(`SELECT COUNT(*) FROM details WHERE ts=1`).Scan(&n)
	if n != 0 {
		t.Fatal("old detail retained")
	}
	if err := p.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	data, err := p.Query(t.Context(), time.Hour)
	if err != nil || len(data["metrics"].([]MetricSummary)) != 0 {
		t.Fatal(data, err)
	}
}
func TestPerformanceRefusesToDeleteNonDiagnosticDatabase(t *testing.T) {
	p := newTestPerformance(t)
	db, err := sql.Open("sqlite", performanceDSN(p.path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE business_data(value TEXT);INSERT INTO business_data VALUES('keep')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = p.SetEnabled(false); err == nil {
		t.Fatal("deleted foreign database")
	}
	db, err = sql.Open("sqlite", performanceDSN(p.path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err = db.QueryRow(`SELECT value FROM business_data`).Scan(&v); err != nil || v != "keep" {
		t.Fatal("foreign data lost", v, err)
	}
}
func TestPerformanceConcurrentToggleObserveAndQuery(t *testing.T) {
	p := newTestPerformance(t)
	if err := p.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				span := p.Start(context.Background(), "database", "concurrent")
				span.Finish(nil)
				_, _ = p.Query(context.Background(), time.Minute)
			}
		}()
	}
	for i := 0; i < 3; i++ {
		if err := p.SetEnabled(false); err != nil {
			t.Fatal(err)
		}
		if err := p.SetEnabled(true); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
func TestResourceSamplerExposesAvailabilityWithoutFakePlatformZeros(t *testing.T) {
	values := NewResourceSampler(t.TempDir())()
	if _, ok := values["goroutines"]; !ok {
		t.Fatal(values)
	}
	if _, ok := values["heap_alloc_bytes"]; !ok {
		t.Fatal(values)
	}
	if n, ok := values["process_rss_bytes"].(uint64); ok && n == 0 {
		t.Fatal("fake RSS 0")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if _, ok := values["process_cpu_seconds"]; !ok {
			t.Fatal("process CPU unavailable", values)
		}
		if _, ok := values["process_rss_bytes"]; !ok {
			t.Fatal("process RSS unavailable", values)
		}
		if _, ok := values["disk_available_bytes"]; !ok {
			t.Fatal("disk available bytes unavailable", values)
		}
	}
	t.Logf("platform metrics: %s", marshalSample(values))
}

func BenchmarkPerformanceDisabled(b *testing.B) {
	p := NewPerformance(filepath.Join(b.TempDir(), "performance.sqlite"), nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		span := p.Start(ctx, "http", "GET /api/admin/files")
		span.Finish(nil)
	}
}
