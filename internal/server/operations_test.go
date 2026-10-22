package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"testing"

	"pimg/internal/dbx"
)

func TestOperationBatchSpans401FilesAndKeepsStableHistory(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	var op struct {
		OperationID int64 `json:"operation_id"`
		TaskID      int64 `json:"task_id"`
	}
	r := call("POST", "/api/admin/operations", bytes.NewBufferString(`{"key":"delete-401","kind":"purge","total_files":401}`), "application/json")
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &op) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	ids := make([]string, 401)
	for i := range ids {
		ids[i] = fmt.Sprintf("batch-delete-%03d", i)
		seedInteractionFile(t, s, ids[i], int64(i+1), sql.NullInt64{})
	}
	for start := 0; start < len(ids); start += 200 {
		end := min(start+200, len(ids))
		body, _ := json.Marshal(map[string]any{"action": "purge", "confirm": "PURGE", "ids": ids[start:end], "operation_id": op.OperationID})
		r = call("POST", "/api/admin/batch", bytes.NewBuffer(body), "application/json")
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	groups, total, err := s.DB.QueryActivityTasks(dbx.LogQuery{Grouped: true, State: "active"})
	if err != nil || total != 1 || groups[0].ID != op.TaskID || groups[0].Group.TotalFiles != 401 {
		t.Fatal(groups, total, err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='done',done_at=unixepoch() WHERE type='purge_file'`); err != nil {
		t.Fatal(err)
	}
	group, err := s.DB.GetTaskGroup(op.TaskID)
	if err != nil || group.State != "pending" {
		t.Fatal("intake ended before final chunk", group, err)
	}
	r = call("POST", fmt.Sprintf("/api/admin/operations/%d/seal", op.OperationID), nil, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	group, err = s.DB.GetTaskGroup(op.TaskID)
	if err != nil || group.State != "done" || group.Group.SucceededFiles != 401 {
		t.Fatal(group, err)
	}
	r = call("GET", "/api/admin/logs?state=terminal&group_id="+jsonNumber(op.TaskID), nil, "")
	var logs struct {
		Total int
		Items []struct{ ID int64 }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &logs) != nil || logs.Total != 1 || logs.Items[0].ID != op.TaskID {
		t.Fatal(r.Code, r.Body.String())
	}
	// A later invocation remains separate even while an earlier one is active.
	r = call("POST", "/api/admin/operations", bytes.NewBufferString(`{"key":"delete-again","kind":"purge","total_files":1}`), "application/json")
	var second struct {
		TaskID int64 `json:"task_id"`
	}
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &second) != nil || second.TaskID == op.TaskID {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestOperationCancelFencesLaterChunksAndRetryOnlyFailed(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "purge", "cancel-split", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		seedInteractionFile(t, s, fmt.Sprintf("split-%d", i), int64(i+1), sql.NullInt64{})
	}
	for i := 0; i < 2; i++ {
		if _, err = s.DB.EnqueuePurgeTask(fmt.Sprintf("split-%d", i), op.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state=CASE file_id WHEN 'split-0' THEN 'done' ELSE 'failed' END,last_error=CASE file_id WHEN 'split-1' THEN 'synthetic failure' END WHERE type='purge_file'`); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SealTaskOperation(op.ID, op.OwnerID); err != nil {
		t.Fatal(err)
	}
	r := call("POST", fmt.Sprintf("/api/admin/operations/%d/retry", op.ID), nil, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	var done, pending int
	s.DB.QueryRow(`SELECT SUM(state='done'),SUM(state='pending') FROM tasks WHERE type='purge_file'`).Scan(&done, &pending)
	if done != 1 || pending != 1 {
		t.Fatal("successful file replayed", done, pending)
	}
	r = call("POST", fmt.Sprintf("/api/admin/operations/%d/cancel", op.ID), nil, "")
	if r.Code != 202 {
		t.Fatal(r.Code, r.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"action": "purge", "confirm": "PURGE", "ids": []string{"split-2"}, "operation_id": op.ID})
	r = call("POST", "/api/admin/batch", bytes.NewBuffer(body), "application/json")
	if r.Code != 409 {
		t.Fatal("canceled batch accepted another chunk", r.Code, r.Body.String())
	}
	f, err := s.DB.GetFile("split-2")
	if err != nil || f.PurgeState.String != "" {
		t.Fatal(f, err)
	}
	if _, err = s.DB.Exec(`DELETE FROM files WHERE id='split-0'`); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.DB.TaskOperationItems(op.ID, op.OwnerID, "", 1, 50)
	if err != nil || len(items) != 2 || items[0].FileName != "split-0.png" || items[0].FileExists {
		t.Fatal(items, err)
	}
	rows, total, err := s.DB.QueryActivityTasks(dbx.LogQuery{Grouped: true, Search: "split-0.png"})
	if err != nil || total != 1 || rows[0].ID != op.TaskID {
		t.Fatal(rows, total, err)
	}
}

func TestAccountOperationIsolationAndFrozenIntent(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
	r := uploaderCall(s, "POST", "/api/account/operations", "user:alpha", map[string]any{"kind": "upload", "key": "same-key", "total_files": 2, "encrypted": false})
	var op struct {
		ID int64 `json:"operation_id"`
	}
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &op) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, suffix := range []string{"", "/items", "/cancel", "/retry", "/seal"} {
		method := "GET"
		if suffix != "" && suffix != "/items" {
			method = "POST"
		}
		r = uploaderCall(s, method, fmt.Sprintf("/api/account/operations/%d%s", op.ID, suffix), "user:bravo", nil)
		if r.Code != 404 {
			t.Fatal("foreign operation exposed", suffix, r.Code, r.Body.String())
		}
	}
	r = uploaderCall(s, "POST", "/api/account/operations", "user:alpha", map[string]any{"kind": "purge", "key": "same-key", "total_files": 2})
	if r.Code != 409 {
		t.Fatal("operation intent changed", r.Code, r.Body.String())
	}
	r = uploaderCall(s, "GET", fmt.Sprintf("/api/account/operations/%d/items?state=all", op.ID), "user:alpha", nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestUploadOperationKeepsSequentialRequestsAndGroupedReceipts(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "upload", "selection", "album:0;encrypted:false", 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("files", fmt.Sprintf("selection-%d.png", i))
		part.Write(interactionPNG(t))
		writer.Close()
		r := call("POST", fmt.Sprintf("/api/admin/upload?operation_id=%d&item_key=item-%d&encrypt=false", op.ID, i), &body, writer.FormDataContentType())
		if r.Code != 201 {
			t.Fatal(r.Code, r.Body.String())
		}
		// Simulate a fast first archive finishing before the next HTTP upload.
		if _, err = s.DB.Exec(`UPDATE tasks SET state='done' WHERE type='direct_archive'; UPDATE files SET archive_state='done'`); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.DB.SealTaskOperation(op.ID, op.OwnerID); err != nil {
		t.Fatal(err)
	}
	r := call("GET", "/api/admin/logs?record=receipts&grouped=1", nil, "")
	var logs struct {
		Total int
		Items []struct{ Group *dbx.TaskGroup }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &logs) != nil || logs.Total != 1 || len(logs.Items) != 1 || logs.Items[0].Group.OperationID != op.ID {
		t.Fatal(r.Code, r.Body.String())
	}
	group, err := s.DB.GetTaskGroup(op.TaskID)
	if err != nil || group.Group.TotalFiles != 2 || group.Group.SkippedFiles != 1 {
		t.Fatal(group, err)
	}
}

func TestLegacyMultipartStillAcceptsMultipleFiles(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i, name := range []string{"legacy-one.png", "legacy-two.png"} {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(append(interactionPNG(t), byte(i)))
	}
	writer.Close()
	r := call("POST", "/api/admin/upload?encrypt=false", &body, writer.FormDataContentType())
	var result struct {
		Files       []map[string]any
		OperationID int64 `json:"operation_id"`
	}
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &result) != nil || len(result.Files) != 2 {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, f := range result.Files {
		if f["error"] != nil {
			t.Fatal(result.Files)
		}
	}
	group, err := s.DB.GetTaskOperation(result.OperationID, dbx.AdminOwner)
	if err != nil || group.Group.TotalFiles != 2 {
		t.Fatal(group, err)
	}
}

func TestUploadItemIdentityCannotHideAnotherFile(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "upload", "retry-input", "album:0;encrypted:false", 1)
	if err != nil {
		t.Fatal(err)
	}
	upload := func(data []byte) map[string]any {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("files", "same-key.png")
		part.Write(data)
		writer.Close()
		r := call("POST", fmt.Sprintf("/api/admin/upload?operation_id=%d&item_key=same&encrypt=false", op.ID), &body, writer.FormDataContentType())
		var result struct{ Files []map[string]any }
		if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &result) != nil || len(result.Files) != 1 {
			t.Fatal(r.Code, r.Body.String())
		}
		return result.Files[0]
	}
	data := interactionPNG(t)
	first := upload(data)
	again := upload(data)
	if first["id"] == nil || again["id"] != first["id"] {
		t.Fatal(first, again)
	}
	// Extra trailing bytes are valid input but a different source identity.
	other := upload(append(append([]byte{}, data...), 1))
	if other["error"] == nil || other["id"] != nil {
		t.Fatal("different bytes reused one input item", other)
	}
	var files, jobs int
	s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files)
	s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='direct_archive'`).Scan(&jobs)
	if files != 1 || jobs != 1 {
		t.Fatal("hidden extra work", files, jobs)
	}
}

func TestActiveOperationOrderAndCompletedInputExpiry(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	old, _ := s.DB.EnqueueTask(dbx.TaskVerify, "", `{"test":"waiting"}`)
	running, _ := s.DB.EnqueueTask(dbx.TaskVerify, "", `{"test":"running"}`)
	if _, err := s.DB.Exec(`UPDATE tasks SET state='running' WHERE id=?`, running); err != nil {
		t.Fatal(err)
	}
	r := call("GET", "/api/admin/logs?record=tasks&state=active&sort=active", nil, "")
	var result struct{ Items []struct{ ID int64 } }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || len(result.Items) != 2 || result.Items[0].ID != running || result.Items[1].ID != old {
		t.Fatal(r.Code, r.Body.String())
	}
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "purge", "all-arrived", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	seedInteractionFile(t, s, "arrived", 1, sql.NullInt64{})
	if _, err = s.DB.EnqueuePurgeTask("arrived", op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE task_operations SET expires_at=1 WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.ExpireTaskOperations(2); err != nil {
		t.Fatal(err)
	}
	anchor, err := s.DB.GetTask(op.TaskID)
	if err != nil || anchor.State != "done" || anchor.LastError.String != "" {
		t.Fatal("lost seal misreported successful intake", anchor, err)
	}
}
