package dbx

import (
	"context"
	"path/filepath"
	"testing"
)

func TestChangeVersionTracksOnlyCommittedChangesOnStableReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	first, err := d.ChangeVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		current, err := d.ChangeVersion(ctx)
		if err != nil || current != first {
			t.Fatal("unchanged database version drifted", first, current, err)
		}
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO settings(k,v) VALUES('version-test','rolled-back')`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if version, err := d.ChangeVersion(ctx); err != nil || version != first {
		t.Fatal("rollback invalidated committed version", version, err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = other.Exec(`INSERT INTO settings(k,v) VALUES('version-test','committed')`); err != nil {
		t.Fatal(err)
	}
	next, err := d.ChangeVersion(ctx)
	if err != nil || next == first {
		t.Fatal("other connection's commit was missed", next, err)
	}
	tx, err = d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE settings SET v='transaction' WHERE k='version-test'`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if version, err := d.ChangeVersion(ctx); err != nil || version == next {
		t.Fatal("transaction commit was missed", version, err)
	}
}
