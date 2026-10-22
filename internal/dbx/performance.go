package dbx

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"pimg/internal/diagnostics"
)

func (db *DB) SetPerformance(p *diagnostics.Performance) { db.performance.Store(p) }
func (db *DB) Performance() *diagnostics.Performance     { return db.performance.Load() }

type RowScanner interface{ Scan(...any) error }
type ReadRows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}
type measuredRow struct {
	row  *sql.Row
	span *diagnostics.Span
}

func (r *measuredRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	observed := err
	if errors.Is(err, sql.ErrNoRows) {
		observed = nil
	}
	r.span.Finish(observed)
	return err
}

type measuredRows struct {
	*sql.Rows
	span    *diagnostics.Span
	scanErr error
}

func (r *measuredRows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		err := r.Rows.Err()
		if err == nil {
			err = r.scanErr
		}
		r.span.Finish(err)
	}
	return ok
}
func (r *measuredRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.scanErr = err
	}
	return err
}
func (r *measuredRows) Close() error {
	err := r.Rows.Close()
	observed := err
	if observed == nil {
		observed = r.scanErr
	}
	if observed == nil {
		observed = r.Rows.Err()
	}
	r.span.Finish(observed)
	return err
}
func (db *DB) performanceSpan(ctx context.Context) *diagnostics.Span {
	p := db.Performance()
	if !p.Enabled() {
		return nil
	}
	name := diagnostics.CallerOperation()
	if strings.HasSuffix(name, ".TouchToken") {
		return nil
	}
	return p.Start(ctx, "database", name)
}

// Non-transactional writes are measured without changing the transaction API.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return db.ExecContext(context.Background(), query, args...)
}
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	span := db.performanceSpan(ctx)
	result, err := db.DB.ExecContext(ctx, query, args...)
	span.Finish(err)
	return result, err
}
