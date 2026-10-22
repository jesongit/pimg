package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

const performanceApplicationID = 0x50494d44

type performanceStore struct {
	db     *sql.DB
	lock   *flock.Flock
	insert string
}

func performanceDSN(path string, readOnly bool) string {
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(filepath.ToSlash(path), "/") {
		path = "/" + filepath.ToSlash(path)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	q.Add("_pragma", "busy_timeout(2000)")
	q.Add("_pragma", "cache_size(-256)")
	q.Add("_pragma", "mmap_size(0)")
	q.Add("_pragma", "temp_store(FILE)")
	u.RawQuery = q.Encode()
	return u.String()
}
func openPerformanceStore(path string) (*performanceStore, error) {
	_, statErr := os.Lstat(path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil || !ok {
		if err == nil {
			err = errors.New("诊断数据库已被其他进程使用")
		}
		return nil, err
	}
	if exists {
		if err := validatePerformanceFile(path); err != nil {
			_ = lock.Unlock()
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", performanceDSN(path, false))
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &performanceStore{db: db, lock: lock}
	fail := func(err error) (*performanceStore, error) { _ = s.close(); return nil, err }
	var appID int
	if err = db.QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
		return fail(err)
	}
	if exists && appID != performanceApplicationID && appID != 0 {
		return fail(errors.New("拒绝使用非 Pimg 诊断数据库"))
	}
	columns, insertCols, updates, placeholders := []string{}, []string{}, []string{}, []string{}
	for i := range latencyBounds {
		name := fmt.Sprintf("b%d", i)
		columns = append(columns, name+" INTEGER NOT NULL DEFAULT 0")
		insertCols = append(insertCols, name)
		updates = append(updates, name+"="+name+"+excluded."+name)
		placeholders = append(placeholders, "?")
	}
	schema := fmt.Sprintf(`PRAGMA application_id=%d;
 CREATE TABLE IF NOT EXISTS metric_buckets(ts INTEGER NOT NULL,instance TEXT NOT NULL,kind TEXT NOT NULL,name TEXT NOT NULL,n INTEGER NOT NULL,errors INTEGER NOT NULL,bytes INTEGER NOT NULL,total_ms REAL NOT NULL,max_ms REAL NOT NULL,%s,PRIMARY KEY(ts,instance,kind,name));
 CREATE TABLE IF NOT EXISTS resource_samples(id INTEGER PRIMARY KEY,ts INTEGER NOT NULL,instance TEXT NOT NULL,body TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS samples_time ON resource_samples(ts,id);
 CREATE TABLE IF NOT EXISTS details(id INTEGER PRIMARY KEY,ts INTEGER NOT NULL,instance TEXT NOT NULL,body TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS details_time ON details(ts,id);`, performanceApplicationID, strings.Join(columns, ","))
	if _, err = db.Exec(schema); err != nil {
		return fail(err)
	}
	_ = os.Chmod(path, 0600)
	s.insert = `INSERT INTO metric_buckets(ts,instance,kind,name,n,errors,bytes,total_ms,max_ms,` + strings.Join(insertCols, ",") + `) VALUES(?,?,?,?,?,?,?,?,?,` + strings.Join(placeholders, ",") + `) ON CONFLICT(ts,instance,kind,name) DO UPDATE SET n=n+excluded.n,errors=errors+excluded.errors,bytes=bytes+excluded.bytes,total_ms=total_ms+excluded.total_ms,max_ms=MAX(max_ms,excluded.max_ms),` + strings.Join(updates, ",")
	return s, nil
}

func validatePerformanceFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("诊断数据库路径不是普通文件")
	}
	db, err := sql.Open("sqlite", performanceDSN(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	var appID int
	if err = db.QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
		return err
	}
	if appID == performanceApplicationID {
		return nil
	}
	var tables int
	if appID == 0 && db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables) == nil && tables == 0 {
		return nil
	}
	return errors.New("拒绝使用或删除非 Pimg 诊断数据库")
}
func (s *performanceStore) close() error {
	err := s.db.Close()
	unlock := s.lock.Unlock()
	if err != nil {
		return err
	}
	return unlock
}

// Delete only the fixed diagnostic file and SQLite sidecars after acquiring the
// diagnostic lock. Never recurse or remove business data, logs or directories.
func purgePerformanceStore(path string) error {
	files := []string{path, path + "-wal", path + "-shm", path + "-journal"}
	found := false
	for _, name := range files {
		if _, err := os.Lstat(name); err == nil {
			found = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !found {
		return nil
	}
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("诊断数据库正在使用，暂时无法清理")
	}
	defer lock.Unlock()
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("诊断数据库路径不是普通文件")
		}
		if err := validatePerformanceFile(path); err != nil {
			return err
		}
	}
	for _, name := range files {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func marshalSample(v map[string]any) string {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 16384 {
		return `{"available":false}`
	}
	return string(raw)
}
func (s *performanceStore) write(ctx context.Context, instance string, batch map[metricKey]*aggregate, details []Detail, sample *sampleRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(batch) > 0 {
		stmt, err := tx.PrepareContext(ctx, s.insert)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for k, v := range batch {
			args := []any{k.Minute, instance, k.Kind, k.Name, v.Count, v.Errors, v.Bytes, v.TotalMS, v.MaxMS}
			for _, n := range v.Hist {
				args = append(args, n)
			}
			if _, err = stmt.ExecContext(ctx, args...); err != nil {
				return err
			}
		}
	}
	for _, v := range details {
		raw, _ := json.Marshal(v)
		if _, err = tx.ExecContext(ctx, `INSERT INTO details(ts,instance,body) VALUES(?,?,?)`, v.Time/1000, instance, string(raw)); err != nil {
			return err
		}
	}
	if sample != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO resource_samples(ts,instance,body) VALUES(?,?,?)`, sample.Time, instance, sample.Body); err != nil {
			return err
		}
	}
	cutoff := time.Now().Add(-PerformanceRetention).Unix()
	for _, q := range []string{
		`DELETE FROM metric_buckets WHERE rowid IN(SELECT rowid FROM metric_buckets WHERE ts<? LIMIT 2000)`,
		`DELETE FROM resource_samples WHERE id IN(SELECT id FROM resource_samples WHERE ts<? LIMIT 2000)`,
		`DELETE FROM details WHERE id IN(SELECT id FROM details WHERE ts<? OR id<(SELECT COALESCE(MAX(id),0)-50000 FROM details) LIMIT 2000)`,
	} {
		if _, err = tx.ExecContext(ctx, q, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type MetricSummary struct {
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Count     int64   `json:"count"`
	Errors    int64   `json:"errors"`
	Bytes     int64   `json:"bytes"`
	AverageMS float64 `json:"average_ms"`
	MaxMS     float64 `json:"max_ms"`
	P95MS     float64 `json:"p95_ms_estimate"`
	P99MS     float64 `json:"p99_ms_estimate"`
}

func quantile(hist []int64, n int64, q, max float64) float64 {
	if n == 0 {
		return 0
	}
	target := int64(math.Ceil(float64(n) * q))
	var count int64
	for i, v := range hist {
		count += v
		if count >= target {
			return math.Min(latencyBounds[i], max)
		}
	}
	return max
}
func (p *Performance) Query(ctx context.Context, window time.Duration) (map[string]any, error) {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	result := map[string]any{"state": p.State(), "window_seconds": int64(window.Seconds()), "metrics": []MetricSummary{}, "samples": []any{}, "details": []any{}}
	if p.store == nil || !p.Enabled() {
		return result, nil
	}
	since := time.Now().Add(-window).Unix() / 60 * 60
	result["from"] = since
	cols := []string{}
	for i := range latencyBounds {
		cols = append(cols, fmt.Sprintf("SUM(b%d)", i))
	}
	rows, err := p.store.db.QueryContext(ctx, `SELECT kind,name,SUM(n),SUM(errors),SUM(bytes),SUM(total_ms),MAX(max_ms),`+strings.Join(cols, ",")+` FROM metric_buckets WHERE ts>=? GROUP BY kind,name ORDER BY SUM(total_ms) DESC LIMIT 256`, since)
	if err != nil {
		return nil, err
	}
	metrics := []MetricSummary{}
	for rows.Next() {
		var m MetricSummary
		var total float64
		hist := make([]int64, len(latencyBounds))
		dest := []any{&m.Kind, &m.Name, &m.Count, &m.Errors, &m.Bytes, &total, &m.MaxMS}
		for i := range hist {
			dest = append(dest, &hist[i])
		}
		if err = rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		m.AverageMS = total / float64(m.Count)
		m.P95MS = quantile(hist, m.Count, .95, m.MaxMS)
		m.P99MS = quantile(hist, m.Count, .99, m.MaxMS)
		metrics = append(metrics, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result["metrics"] = metrics
	resolution := int64(math.Max(10, math.Ceil(window.Seconds()/120)))
	result["sample_resolution_seconds"] = resolution
	rows, err = p.store.db.QueryContext(ctx, `SELECT ts,instance,body FROM resource_samples WHERE id IN(SELECT MAX(id) FROM resource_samples WHERE ts>=? GROUP BY CAST(ts/? AS INTEGER)) ORDER BY ts LIMIT 121`, since, resolution)
	if err != nil {
		return nil, err
	}
	samples := []any{}
	for rows.Next() {
		var ts int64
		var instance, body string
		if err = rows.Scan(&ts, &instance, &body); err != nil {
			rows.Close()
			return nil, err
		}
		samples = append(samples, map[string]any{"time": ts, "instance_id": instance, "values": json.RawMessage(body)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result["samples"] = samples
	rows, err = p.store.db.QueryContext(ctx, `SELECT instance,body FROM details WHERE ts>=? ORDER BY id DESC LIMIT 100`, since)
	if err != nil {
		return nil, err
	}
	details := []any{}
	for rows.Next() {
		var instance, body string
		if err = rows.Scan(&instance, &body); err != nil {
			rows.Close()
			return nil, err
		}
		details = append(details, map[string]any{"instance_id": instance, "event": json.RawMessage(body)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result["details"] = details
	return result, nil
}
