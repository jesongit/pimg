package dbx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestLogTaskFiltersSortAndPaging(t *testing.T) {
	d := activityDB(t)
	ids := []int64{}
	for i := 0; i < 61; i++ {
		id, e := d.EnqueueTask(TaskCleanupTemp, "", `{}`)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
		if _, e = d.Exec(`UPDATE tasks SET state='done',created_at=100,updated_at=200,attempts=? WHERE id=?`, i%4, id); e != nil {
			t.Fatal(e)
		}
	}
	one, total, e := d.QueryActivityTasks(LogQuery{View: "all", Sort: "created_asc", Page: 1, Limit: 50})
	if e != nil || total != 61 || len(one) != 50 || one[0].ID != ids[0] {
		t.Fatal(total, len(one), e)
	}
	two, _, e := d.QueryActivityTasks(LogQuery{View: "all", Sort: "created_asc", Page: 2, Limit: 50})
	if e != nil || len(two) != 11 || two[0].ID != ids[50] {
		t.Fatal(two, e)
	}
	_, _ = d.Exec(`UPDATE tasks SET state='pending',attempts=3,last_error='source not found',created_at=500,updated_at=600 WHERE id=?`, ids[0])
	_, _ = d.Exec(`UPDATE tasks SET state='running',cancel_requested=1 WHERE id=?`, ids[1])
	checks := []struct {
		q     LogQuery
		count int
		first int64
	}{
		{LogQuery{State: "retrying", Type: TaskCleanupTemp, From: 400, Search: "source not found"}, 1, ids[0]},
		{LogQuery{State: "active", Sort: "created_desc"}, 2, ids[0]},
		{LogQuery{State: "stopping"}, 1, ids[1]},
		{LogQuery{Search: fmt.Sprint(ids[60]), Sort: "created_desc"}, 1, ids[60]},
		{LogQuery{Search: "#" + fmt.Sprint(ids[60]), Sort: "created_desc"}, 1, ids[60]},
		{LogQuery{From: 700}, 0, 0},
		{LogQuery{Type: TaskVerify}, 0, 0},
	}
	for _, c := range checks {
		c.q.View = "all"
		c.q.Page = 1
		c.q.Limit = 50
		rows, n, e := d.QueryActivityTasks(c.q)
		if e != nil || n != c.count {
			t.Fatalf("%+v: total=%d err=%v", c.q, n, e)
		}
		if c.first > 0 && rows[0].ID != c.first {
			t.Fatal(c.q, rows[0].ID)
		}
	}
	rows, _, e := d.QueryActivityTasks(LogQuery{Sort: "attempts_desc", Page: 1, Limit: 50})
	if e != nil {
		t.Fatal(e)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Attempts > rows[i-1].Attempts {
			t.Fatal("attempt sort not applied before pagination")
		}
	}
}

func TestLogReceiptsMergeAuditStagesAndSearchIdentities(t *testing.T) {
	d := activityDB(t)
	f := &File{ID: "merged-receipt", Kind: "image", KeyWrapped: []byte{}, KeyNonce: []byte{}, Name: "stored", OrigName: "接管原件.png", ArchiveState: "done"}
	if e := d.InsertFile(f); e != nil {
		t.Fatal(e)
	}
	if e := d.RecordIngestEvent(IngestEvent{SourceKey: "tool:one", Type: "adopted", FileID: f.ID, ToolFileID: "tool-identity", UploadID: "upload-group", Body: json.RawMessage(`{"source":"uploader"}`)}); e != nil {
		t.Fatal(e)
	}
	if e := d.RecordIngestEvent(IngestEvent{SourceKey: "verify:" + f.ID, Type: "verified", FileID: f.ID, ToolFileID: "tool-identity", UploadID: "upload-group", Body: json.RawMessage(`{"verification":"verified"}`)}); e != nil {
		t.Fatal(e)
	}
	if e := d.RecordIngestEvent(IngestEvent{SourceKey: "tool:one", Type: "duplicate", Reason: "same source already indexed", FileID: f.ID, ToolFileID: "tool-identity", UploadID: "upload-group", Body: json.RawMessage(`{"source":"uploader"}`)}); e != nil {
		t.Fatal(e)
	}
	_, _ = d.Exec(`UPDATE ingest_events SET created_at=CASE WHEN event_type='adopted' THEN 100 ELSE 500 END WHERE source_key='tool:one'`)
	_, _ = d.Exec(`UPDATE ingest_events SET created_at=900 WHERE source_key=?`, "verify:"+f.ID)
	_, _ = d.Exec(`UPDATE files SET updated_at=0 WHERE id=?`, f.ID)
	rows, total, e := d.QueryLogReceipts(LogQuery{Search: "upload-group", Page: 1, Limit: 50})
	if e != nil || total != 1 || len(rows) != 1 || rows[0].Outcome != "duplicate" {
		t.Fatal(rows, total, e)
	}
	if rows[0].CreatedAt != 100 || rows[0].UpdatedAt != 900 {
		t.Fatal("receipt creation or update lost", rows[0])
	}
	id := rows[0].ID
	events, e := d.ReceiptEvents(id)
	if e != nil || len(events) != 3 {
		t.Fatal(events, e)
	}
	hasVerification, hasIdentity := false, false
	for _, event := range events {
		hasVerification = hasVerification || event.Type == "verified"
		hasIdentity = hasIdentity || event.ToolFileID == "tool-identity" && event.UploadID == "upload-group"
	}
	if !hasVerification || !hasIdentity {
		t.Fatal("audit stage or tool identity lost")
	}
	rows, total, e = d.QueryLogReceipts(LogQuery{FileID: f.ID, State: "duplicate", Type: "verified", Search: "R" + fmt.Sprint(id), Sort: "updated_desc", Page: 1, Limit: 50})
	if e != nil || total != 1 || rows[0].ID != id {
		t.Fatal(rows, total, e)
	}
	for i := 0; i < 53; i++ {
		if e := d.RecordIngestEvent(IngestEvent{SourceKey: fmt.Sprintf("rejected:%d", i), Type: "failed", Reason: "invalid input", Body: json.RawMessage(`{"orig_name":"bad.png"}`)}); e != nil {
			t.Fatal(e)
		}
	}
	page, n, e := d.QueryLogReceipts(LogQuery{State: "failed", Sort: "created_asc", Page: 2, Limit: 50})
	if e != nil || n != 53 || len(page) != 3 {
		t.Fatal(n, len(page), e)
	}
	_, _ = d.Exec(`DELETE FROM files WHERE id=?`, f.ID)
	rows, total, e = d.QueryLogReceipts(LogQuery{FileID: f.ID, State: "deleted", Page: 1, Limit: 50})
	if e != nil || total != 1 || rows[0].Name != f.OrigName || rows[0].Exists {
		t.Fatal(rows, total, e)
	}
}

func TestLogLinksRetainTheOwningAccount(t *testing.T) {
	d := activityDB(t)
	owner := "user:log-link-test"
	if e := d.CreateAccount(owner, "log-owner", "hash"); e != nil {
		t.Fatal(e)
	}
	album, e := d.InsertOwnerAlbum(owner, "own album")
	if e != nil {
		t.Fatal(e)
	}
	id, e := d.EnqueueTask(TaskTelegramRenameAlbum, fmt.Sprintf("tg-album:%d", album), `{}`)
	if e != nil {
		t.Fatal(e)
	}
	rows, n, e := d.QueryActivityTasks(LogQuery{Search: "#" + fmt.Sprint(id), Page: 1, Limit: 50})
	if e != nil || n != 1 || rows[0].OwnerID != owner {
		t.Fatal(rows, n, e)
	}
	f := &File{ID: "owned-log-file", OwnerID: owner, Kind: "image", KeyWrapped: []byte{}, KeyNonce: []byte{}, Name: "owned", ArchiveState: "done"}
	if e = d.InsertFile(f); e != nil {
		t.Fatal(e)
	}
	receipts, n, e := d.QueryLogReceipts(LogQuery{FileID: f.ID, Page: 1, Limit: 50})
	if e != nil || n != 1 || receipts[0].OwnerID != owner {
		t.Fatal(receipts, n, e)
	}
}
