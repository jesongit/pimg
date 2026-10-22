package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

type changeVersionReader struct {
	mu    sync.Mutex
	pool  *sql.DB
	conn  *sql.Conn
	epoch uint64
}

// data_version is connection-local. Keep one dedicated read-only connection so
// pooled connection changes cannot make a new commit look like an old version.
// It observes committed transactions, including writes outside this process;
// it needs no schema, triggers, full scans or business database writes.
func (db *DB) ChangeVersion(ctx context.Context) (string, error) {
	v := &db.versions
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pool == nil {
		pool, err := sql.Open("sqlite", db.readDSN)
		if err != nil {
			return "", err
		}
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		v.pool = pool
	}
	if v.conn == nil {
		conn, err := v.pool.Conn(ctx)
		if err != nil {
			return "", err
		}
		v.conn = conn
		v.epoch++
	}
	var revision int64
	if err := v.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&revision); err != nil {
		v.conn.Close()
		v.conn = nil
		return "", err
	}
	return fmt.Sprintf("%d:%d", v.epoch, revision), nil
}

func (db *DB) closeChangeVersion() {
	v := &db.versions
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.conn != nil {
		v.conn.Close()
		v.conn = nil
	}
	if v.pool != nil {
		v.pool.Close()
		v.pool = nil
	}
}

func (db *DB) NextOperationExpiry() (int64, error) {
	var expiry int64
	err := db.ReadQueryRow(`SELECT COALESCE(MIN(expires_at),0) FROM task_operations WHERE input_state='open'`).Scan(&expiry)
	return expiry, err
}
