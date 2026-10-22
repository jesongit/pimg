package dbx

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func querySnapshot(t *testing.T, d *DB, query string) [32]byte {
	t.Helper()
	rows, err := d.reader.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for rows.Next() {
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		if err = encoder.Encode(values); err != nil {
			t.Fatal(err)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return [32]byte(hash.Sum(nil))
}

func TestTaskGroupPrimaryKeyJoinPreservesGroupingAndStoredData(t *testing.T) {
	d := activityDB(t)
	explicit := beginOperationFixture(t, d, "upload", "join-explicit", 2)
	for i := 0; i < 2; i++ {
		f := operationFixtureFile(t, d, fmt.Sprintf("join-explicit-%d", i))
		if _, err := d.EnqueueTask(TaskArchive, f.ID, "{}", explicit.ID); err != nil {
			t.Fatal(err)
		}
	}
	legacy := telegramTestFile("join-legacy", 0)
	legacy.SHA256 = legacy.ID
	if err := d.InsertUploadBatch([]*File{legacy}); err != nil {
		t.Fatal(err)
	}
	// An imported file can also belong to a legacy upload wave. The import
	// group must continue to take precedence, ignoring the wave's open intake.
	batch, _ := importReceiptFixture(t, d, "/join-import", "done")
	_, err := d.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,file_ids) VALUES(?,'legacy.png',1,'v','archiving',json_array(?))`, batch, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET created_at=unixepoch(),updated_at=unixepoch(),state='done' WHERE file_id=?`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE task_operations SET input_state='open',expires_at=unixepoch()+3600 WHERE request_key=''`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.EnqueueTask(TaskVerify, "unbound-file", "{}"); err != nil {
		t.Fatal(err)
	}
	before := map[string][32]byte{}
	for _, table := range []string{"tasks", "task_operations", "task_operation_jobs", "task_operation_items", "local_import_batches", "local_import_items", "files", "ingest_events", "sqlite_schema"} {
		before[table] = querySnapshot(t, d, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	legacyGroups := strings.ReplaceAll(taskGroupsCTE, "o.id=m.group_operation_id", "(CASE WHEN o.request_key!='' THEN 'operation:' ELSE o.kind||':' END)||o.id=m.group_key")
	for _, suffix := range []string{
		` SELECT * FROM grouped_tasks ORDER BY group_key`,
		` SELECT id,group_key,state FROM grouped_tasks WHERE state IN ('pending','running') ORDER BY group_key`,
	} {
		if querySnapshot(t, d, taskGroupsCTE+suffix) != querySnapshot(t, d, legacyGroups+suffix) {
			t.Fatal("primary-key join changed grouped task results")
		}
	}
	legacyLogs := strings.ReplaceAll(unifiedLogsCTE, "o.id=m.group_operation_id", "(CASE WHEN o.request_key!='' THEN 'operation:' ELSE o.kind||':' END)||o.id=m.group_key")
	if querySnapshot(t, d, unifiedLogsCTE+` SELECT * FROM visible_logs ORDER BY record_key`) != querySnapshot(t, d, legacyLogs+` SELECT * FROM visible_logs ORDER BY record_key`) {
		t.Fatal("primary-key join changed merged history")
	}
	var expected TaskCounts
	if err = d.ReadQueryRow(legacyGroups+` SELECT COALESCE(SUM(state='running'),0),COALESCE(SUM(state='pending'),0) FROM grouped_tasks`).Scan(&expected.Running, &expected.Pending); err != nil {
		t.Fatal(err)
	}
	actual, err := d.ActivityTaskCounts("")
	if err != nil || actual != expected {
		t.Fatal("navigation counts changed", actual, expected, err)
	}
	for table, original := range before {
		if querySnapshot(t, d, "SELECT * FROM "+table+" ORDER BY rowid") != original {
			t.Fatal("query changed stored data or schema", table)
		}
	}
}

func TestTaskGroupOperationJoinAvoidsRepeatedOperationTableScans(t *testing.T) {
	d := activityDB(t)
	rows, err := d.ReadQuery("EXPLAIN QUERY PLAN " + taskGroupsCTE + " SELECT * FROM grouped_tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN o LEFT-JOIN") {
			t.Fatal("operation table scanned once per task", detail)
		}
		if strings.Contains(detail, "SEARCH o USING INTEGER PRIMARY KEY") {
			found = true
		}
	}
	if err = rows.Err(); err != nil || !found {
		t.Fatal("primary-key operation lookup missing", err)
	}
}
