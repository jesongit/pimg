package dbx

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestWALReadersAndWriterDoNotQueueBehindEachOther(t *testing.T) {
	d := activityDB(t)
	if _, err := d.Exec(`INSERT INTO settings(k,v) VALUES('pool-test','before')`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE settings SET v='uncommitted' WHERE k='pool-test'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	rows, err := d.ReadQueryContext(ctx, `SELECT v FROM settings WHERE k='pool-test'`)
	if err != nil {
		t.Fatal("reader waited for the writer", err)
	}
	var value string
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	if err = rows.Scan(&value); err != nil || value != "before" {
		t.Fatal("dirty read", value, err)
	}
	rows.Close()
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Occupy both reader connections; writes must still have their own pool.
	r1, err := d.ReadQuery(`SELECT v FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r1.Next()
	r2, err := d.ReadQuery(`SELECT v FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	r2.Next()
	if _, err = d.ExecContext(ctx, `UPDATE settings SET v='after' WHERE k='pool-test'`); err != nil {
		t.Fatal("writer waited for readers", err)
	}
	r1.Close()
	r2.Close()
	if _, err = d.ReadQueryContext(ctx, `DELETE FROM settings`); err == nil {
		t.Fatal("read pool allowed mutation")
	}
}

func TestLightTaskCountsMatchHistoryWithLargeImport(t *testing.T) {
	d := activityDB(t)
	batch, _ := importReceiptFixture(t, d, "/bulk")
	if _, err := d.Exec(`UPDATE local_import_batches SET state='running' WHERE id=?`, batch); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if _, err = tx.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,file_ids) VALUES(?,?,1,'v','done',?)`, batch, fmt.Sprintf("/bulk/%d", i), fmt.Sprintf(`["original-%d"]`, i)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO tasks(type,payload,state,run_after,created_at,updated_at) VALUES('cleanup_temp','{}','done',1,1,1)`); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	o := beginOperationFixture(t, d, "upload", "still-receiving", 3)
	f := operationFixtureFile(t, d, "running-file")
	task, err := d.EnqueueTask(TaskArchive, f.ID, "{}", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET state='running' WHERE id=?`, task); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "pending", "done", "failed", "canceled"} {
		if _, err = d.Exec(`UPDATE tasks SET state=? WHERE id=?`, state, task); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		var expected TaskCounts
		if err = d.ReadQueryRow(taskGroupsCTE+` SELECT COALESCE(SUM(state='running'),0),COALESCE(SUM(state='pending'),0) FROM grouped_tasks WHERE state IN ('running','pending')`).Scan(&expected.Running, &expected.Pending); err != nil {
			t.Fatal(err)
		}
		full := time.Since(start)
		start = time.Now()
		actual, err := d.ActivityTaskCounts("")
		light := time.Since(start)
		if err != nil || actual != expected {
			t.Fatal(state, actual, expected, err)
		}
		t.Logf("5000 import items + history, state=%s: original=%s simplified=%s", state, full, light)
	}
}
