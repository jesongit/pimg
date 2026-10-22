package dbx

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func dateOperation(t *testing.T, d *DB, owner, key string, stamp int64, count int) *TaskOperation {
	t.Helper()
	op, err := d.BeginTaskOperation(owner, "set_media_time", key, MediaDateScope(stamp), count)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestManualMediaTimePreservesOriginalsAndUpdatesIndexedTimeline(t *testing.T) {
	d := activityDB(t)
	operationFixtureFile(t, d, "dated")
	operationFixtureFile(t, d, "unchanged")
	const original = `{"original_time":946684800,"time_source":"exif","unknown":{"keep":[1,"two"]}}`
	if _, err := d.Exec(`UPDATE files SET naming_json=?,created_at=1800000000,updated_at=1800000001 WHERE id='dated'`, original); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE files SET created_at=1600000000 WHERE id='unchanged'`); err != nil {
		t.Fatal(err)
	}
	if nav, err := d.FileNavigation(FileFilter{Sort: "oldest"}, "dated"); err != nil || nav.Position != 1 {
		t.Fatal(nav, err)
	}
	// Compare every persisted column except the two explicitly changed fields.
	columns, err := d.Query(`SELECT name FROM pragma_table_info('files') WHERE name NOT IN ('naming_json','updated_at')`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for columns.Next() {
		var name string
		if err = columns.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	columns.Close()
	query := `SELECT ` + strings.Join(names, ",") + ` FROM files ORDER BY id`
	before := querySnapshot(t, d, query)
	const stamp int64 = 1709251199 // 2024-02-29 23:59:59 UTC
	op := dateOperation(t, d, AdminOwner, "date-seconds", stamp, 1)
	if err = d.SetMediaTimeInOperation("dated", stamp, op.ID); err != nil {
		t.Fatal(err)
	}
	if before != querySnapshot(t, d, query) {
		t.Fatal("original file fields changed")
	}
	if nav, err := d.FileNavigation(FileFilter{Sort: "oldest"}, "dated"); err != nil || nav.Position != 2 {
		t.Fatal("navigation cache was not invalidated", nav, err)
	}
	f, err := d.GetFile("dated")
	if err != nil || f.MediaAt != stamp || f.Naming.OriginalTime != 946684800 || f.Naming.TimeSource != "exif" || f.MediaTimeSource() != "manual" {
		t.Fatal(f, err)
	}
	var unknown string
	if err = d.QueryRow(`SELECT json_extract(naming_json,'$.unknown') FROM files WHERE id='dated'`).Scan(&unknown); err != nil || unknown != `{"keep":[1,"two"]}` {
		t.Fatal(unknown, err)
	}
	groups, err := d.FileTimelineGroups(FileFilter{Query: "dated"}, 0, "day")
	if err != nil || len(groups) != 1 || groups[0].Key != "2024-02-29" {
		t.Fatal(groups, err)
	}
	rows, n, err := d.ListFiles(FileFilter{After: stamp, Before: stamp + 1, Sort: "oldest"})
	if err != nil || n != 1 || len(rows) != 1 || rows[0].ID != "dated" {
		t.Fatal(rows, n, err)
	}
	// Replaying the same chunk is idempotent and does not duplicate receipts.
	if err = d.SetMediaTimeInOperation("dated", stamp, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.SealTaskOperation(op.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	group, err := d.GetTaskGroup(op.TaskID)
	if err != nil || group.State != "done" || group.Group.SucceededFiles != 1 {
		t.Fatal(group, err)
	}
	plan, err := d.Query(`EXPLAIN QUERY PLAN SELECT id FROM files WHERE owner_id=? ORDER BY (`+mediaTimeSQL+`) DESC,id DESC LIMIT 60`, AdminOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var details string
	for plan.Next() {
		var a, b, c int
		var detail string
		if err = plan.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		details += detail
	}
	if !strings.Contains(details, "files_owner_media_time") || strings.Contains(details, "TEMP B-TREE") {
		t.Fatal(details)
	}
}

func TestManualMediaTimeRejectsWithoutPartialEdits(t *testing.T) {
	for _, mode := range []string{"wrong-date", "closed", "expired", "foreign", "hidden", "deleting", "capacity", "invalid-metadata", "invalid-date", "receipt-failure"} {
		t.Run(mode, func(t *testing.T) {
			d := activityDB(t)
			operationFixtureFile(t, d, "kept")
			const stamp int64 = 1709251199
			op := dateOperation(t, d, AdminOwner, "guard", stamp, 1)
			input := stamp
			switch mode {
			case "wrong-date":
				input++
			case "closed":
				if err := d.SealTaskOperation(op.ID, AdminOwner); err != nil {
					t.Fatal(err)
				}
			case "expired":
				d.Exec(`UPDATE task_operations SET expires_at=1 WHERE id=?`, op.ID)
			case "foreign":
				d.Exec(`UPDATE task_operations SET owner_id='user:someone' WHERE id=?`, op.ID)
			case "hidden":
				d.Exec(`UPDATE files SET hidden=1 WHERE id='kept'`)
			case "deleting":
				d.Exec(`UPDATE files SET purge_state='pending' WHERE id='kept'`)
			case "capacity":
				if err := d.RecordTaskOperationItem(op.ID, AdminOwner, "other", "", "other", "done", ""); err != nil {
					t.Fatal(err)
				}
			case "invalid-metadata":
				d.Exec(`UPDATE files SET naming_json='[]' WHERE id='kept'`)
			case "invalid-date":
				input = 253402300800
			case "receipt-failure":
				d.Exec(`CREATE TRIGGER deny_date_receipt BEFORE INSERT ON task_operation_items BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END`)
			}
			before := querySnapshot(t, d, `SELECT * FROM files ORDER BY id`)
			receipts := querySnapshot(t, d, `SELECT * FROM task_operation_items ORDER BY rowid`)
			if err := d.SetMediaTimeInOperation("kept", input, op.ID); err == nil {
				t.Fatal("guard accepted")
			}
			if before != querySnapshot(t, d, `SELECT * FROM files ORDER BY id`) || receipts != querySnapshot(t, d, `SELECT * FROM task_operation_items ORDER BY rowid`) {
				t.Fatal("failed mutation changed file or receipt")
			}
		})
	}
}

func TestManualDateMigrationFrom27PreservesRowsAndRebuildsOnlyDateIndexes(t *testing.T) {
	path, old := oldUpgradeDB(t, 27)
	const naming = `{"original_time":946684800,"time_source":"exif","keep":"yes"}`
	if _, err := old.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at,naming_json) VALUES('legacy','kept','camera.jpg','jpg','image/jpeg',3,'hash','image',X'01',X'02','done',1800000000,1800000001,?)`, naming); err != nil {
		t.Fatal(err)
	}
	var indexBefore string
	if err := old.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='files_owner_media_time'`).Scan(&indexBefore); err != nil {
		t.Fatal(err)
	}
	var legacyColumns string
	if err := old.QueryRow(`SELECT group_concat(name,',') FROM pragma_table_info('files')`).Scan(&legacyColumns); err != nil {
		t.Fatal(err)
	}
	legacyFileQuery := `SELECT ` + legacyColumns + ` FROM files ORDER BY id`
	legacyRows := querySnapshot(t, &DB{reader: old}, legacyFileQuery)
	const unrelatedSchema = `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE name NOT IN ('files_media_time','files_owner_media_time','albums','albums_displayed','files','gallery_file_visibility_update','gallery_album_visibility_update') ORDER BY type,name`
	legacySchema := querySnapshot(t, &DB{reader: old}, unrelatedSchema)
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFile("legacy")
	if err != nil || f.MediaAt != 946684800 || f.CreatedAt != 1800000000 || f.UpdatedAt != 1800000001 || f.Name != "kept" {
		t.Fatal(f, err)
	}
	var got, indexAfter, integrity string
	d.QueryRow(`SELECT naming_json FROM files WHERE id='legacy'`).Scan(&got)
	d.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='files_owner_media_time'`).Scan(&indexAfter)
	d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if got != naming || indexAfter == indexBefore || !strings.Contains(indexAfter, "manual_time") || integrity != "ok" {
		t.Fatal(got, indexAfter, integrity)
	}
	before := querySnapshot(t, d, `SELECT * FROM files ORDER BY id`)
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if legacyRows != querySnapshot(t, d, legacyFileQuery) || legacySchema != querySnapshot(t, d, unrelatedSchema) {
		t.Fatal("upgrade changed legacy rows or unrelated schema")
	}
	if !reflect.DeepEqual(before, querySnapshot(t, d, `SELECT * FROM files ORDER BY id`)) {
		t.Fatal("restart changed file")
	}
	// The previous schema must truly be the released schema, not a moving offset.
	if CurrentSchemaVersion() != 30 {
		t.Fatal("review this upgrade fixture for a new schema", CurrentSchemaVersion())
	}
}

func TestManualMediaTimeMissingOperation(t *testing.T) {
	d := activityDB(t)
	operationFixtureFile(t, d, "kept")
	if err := d.SetMediaTimeInOperation("kept", 0, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var stamp sql.NullInt64
	if err := d.QueryRow(`SELECT json_extract(naming_json,'$.manual_time') FROM files WHERE id='kept'`).Scan(&stamp); err != nil || stamp.Valid {
		t.Fatal(stamp, err)
	}
}
