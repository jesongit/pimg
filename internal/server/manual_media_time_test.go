package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"pimg/internal/dbx"
)

func TestBatchGalleryDateSpansChunksAndKeepsExactSeconds(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	const stamp int64 = 1709251199
	var op struct {
		ID   int64 `json:"operation_id"`
		Task int64 `json:"task_id"`
	}
	r := call("POST", "/api/admin/operations", bytes.NewBufferString(fmt.Sprintf(`{"kind":"set_media_time","key":"date-201","media_at":%d,"total_files":201}`, stamp)), "application/json")
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &op) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("date-%03d", i)
		seedInteractionFile(t, s, ids[i], int64(i+1), sql.NullInt64{})
		if _, err := s.DB.Exec(`UPDATE files SET created_at=? WHERE id=?`, i+1, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	for start := 0; start < len(ids); start += 200 {
		body, _ := json.Marshal(map[string]any{"action": "set_media_time", "media_at": stamp, "ids": ids[start:min(start+200, len(ids))], "operation_id": op.ID})
		r = call("POST", "/api/admin/batch", bytes.NewReader(body), "application/json")
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		var result struct {
			Done    int `json:"done"`
			Skipped int `json:"skipped"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.Done != min(200, len(ids)-start) || result.Skipped != 0 {
			t.Fatal(result, err, r.Body.String())
		}
	}
	for i, id := range ids {
		f, err := s.DB.GetFile(id)
		if err != nil || f.MediaAt != stamp || f.CreatedAt != int64(i+1) || f.OrigName != id+".png" || f.MediaTimeSource() != "manual" {
			t.Fatal(f, err)
		}
	}
	// A different date must never reuse this operation, even before sealing it.
	r = call("POST", "/api/admin/batch", bytes.NewBufferString(fmt.Sprintf(`{"action":"set_media_time","media_at":%d,"ids":["date-000"],"operation_id":%d}`, stamp+1, op.ID)), "application/json")
	if r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = call("POST", fmt.Sprintf("/api/admin/operations/%d/seal", op.ID), nil, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	group, err := s.DB.GetTaskGroup(op.Task)
	if err != nil || group.State != "done" || group.Group.SucceededFiles != 201 {
		t.Fatal(group, err)
	}
	r = call("GET", "/api/admin/files?sort=oldest&per_page=1", nil, "")
	var listed struct {
		Items []struct {
			MediaAt int64  `json:"media_at"`
			Source  string `json:"media_time_source"`
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 || listed.Items[0].MediaAt != stamp || listed.Items[0].Source != "manual" {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestInvalidGalleryDateCreatesNoOperationOrFileEdit(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	seedInteractionFile(t, s, "kept", 123, sql.NullInt64{})
	before, err := s.DB.GetFile("kept")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", `,"media_at":null`, `,"media_at":1.5`, `,"media_at":"2024-02-29"`, `,"media_at":253402300800`, `,"media_at":-2208988801`} {
		for _, endpoint := range []string{"operations", "batch"} {
			body := `{"kind":"set_media_time","action":"set_media_time","key":"bad-date","total_files":1,"ids":["kept"]` + value + `}`
			r := call("POST", "/api/admin/"+endpoint, bytes.NewBufferString(body), "application/json")
			if r.Code != 400 {
				t.Fatal(value, endpoint, r.Code, r.Body.String())
			}
		}
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM task_operations`).Scan(&count)
	f, err := s.DB.GetFile("kept")
	if count != 0 || err != nil || f.MediaAt != before.MediaAt || f.Naming.ManualTime != nil {
		t.Fatal(count, f, err)
	}
}

func TestAccountGalleryDateRejectsMixedOwnersBeforeAnyMutation(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{"user:date-a", "user:date-b"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.InsertFile(&dbx.File{ID: owner, OwnerID: owner, Name: owner, OrigName: owner + ".png", Ext: "png", Mime: "image/png", Size: 123, SHA256: "hash", Kind: "image", KeyWrapped: []byte{1}, KeyNonce: []byte{2}, ArchiveState: "done", Storage: "direct", ColdBackend: "local"}); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"action": "set_media_time", "ids": []string{"user:date-a", "user:date-b"}, "media_at": 1709251199}
	r := uploaderCall(s, "POST", "/api/account/batch", "user:date-a", body)
	if r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, id := range []string{"user:date-a", "user:date-b"} {
		f, err := s.DB.GetFile(id)
		if err != nil || f.Naming.ManualTime != nil {
			t.Fatal(f, err)
		}
	}
	body["ids"] = []string{"user:date-a"}
	r = uploaderCall(s, "POST", "/api/account/batch", "user:date-a", body)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	f, err := s.DB.GetFile("user:date-a")
	if err != nil || f.MediaAt != 1709251199 {
		t.Fatal(f, err)
	}
}

func TestManualDatesLocateBeforeAndAtUnixEpoch(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	for _, stamp := range []int64{-1, 0, 1} {
		id := fmt.Sprintf("epoch-%d", stamp)
		seedInteractionFile(t, s, id, 1, sql.NullInt64{})
		body, _ := json.Marshal(map[string]any{"action": "set_media_time", "media_at": stamp, "ids": []string{id}})
		r := call("POST", "/api/admin/batch", bytes.NewReader(body), "application/json")
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	for _, item := range []struct{ query, id string }{{"before=0", "epoch--1"}, {"after=0&before=1", "epoch-0"}, {"after=1&before=2", "epoch-1"}} {
		r := call("GET", "/api/admin/files?"+item.query, nil, "")
		var got struct {
			Total int
			Items []struct{ ID string }
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &got) != nil || got.Total != 1 || got.Items[0].ID != item.id {
			t.Fatal(item, r.Code, r.Body.String())
		}
	}
}
