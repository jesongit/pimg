package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"pimg/internal/dbx"
	"testing"
)

func TestGroupedImportAPIStopsIntakeAndPreservesAcceptedArchives(t *testing.T) {
	s := importServer(t)
	request := carrierRequest(t, s)
	r, err := s.DB.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule,state) VALUES('/source','[]',0,0,1,'now','running')`)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := r.LastInsertId()
	parent, err := s.DB.EnqueueTask(dbx.TaskLocalImportScan, "", fmt.Sprintf(`{"batch_id":%d}`, batch))
	if err != nil {
		t.Fatal(err)
	}
	seedInteractionFile(t, s, "accepted", 1, sql.NullInt64{})
	archive, err := s.DB.EnqueueTask(dbx.TaskArchive, "accepted", "{}")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,file_ids) VALUES(?,'accepted.png',1,'v','archiving','["accepted"]')`, batch)
	if err != nil {
		t.Fatal(err)
	}
	response := request("GET", "/api/admin/logs?record=tasks", nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var list struct {
		Total int
		Items []struct {
			ID    int64
			Type  string
			Group dbx.TaskGroup
		}
	}
	if err = json.Unmarshal(response.Body.Bytes(), &list); err != nil || list.Total != 1 || len(list.Items) != 1 || list.Items[0].Group.BatchID != batch {
		t.Fatal(err, response.Body.String())
	}
	response = request("POST", fmt.Sprintf("/api/admin/tasks/%d/cancel?group=1", parent), nil, "")
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	b, err := s.Svc.LocalImportBatch(batch)
	if err != nil || b.State != "canceled" {
		t.Fatal(b, err)
	}
	job, err := s.DB.GetTask(parent)
	if err != nil || job.State != "canceled" {
		t.Fatal(job, err)
	}
	job, err = s.DB.GetTask(archive)
	if err != nil || job.State != "pending" {
		t.Fatal("accepted original abandoned", job, err)
	}
	response = request("GET", fmt.Sprintf("/api/admin/tasks/%d/children", parent), nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request("POST", fmt.Sprintf("/api/admin/tasks/%d/retry?group=1", parent), nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	b, _ = s.Svc.LocalImportBatch(batch)
	if b.State != "running" {
		t.Fatal("canceled batch could not resume", b)
	}
}

func TestDeleteAutomaticallyCancelsQueuedArchivePublishAndSync(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	seedInteractionFile(t, s, "delete-queued", 1, sql.NullInt64{})
	var ids []int64
	for _, kind := range []string{dbx.TaskArchive, dbx.TaskPublish, dbx.TaskArchiveSync} {
		id, err := s.DB.EnqueueTask(kind, "delete-queued", "{}")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	response := request("DELETE", "/api/admin/files/delete-queued?confirm=PURGE", nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, id := range ids {
		job, err := s.DB.GetTask(id)
		if err != nil || job.State != "canceled" {
			t.Fatal("queued work survived deletion", job, err)
		}
	}
	f, err := s.DB.GetFile("delete-queued")
	if err != nil || f.PurgeState.String != "pending" {
		t.Fatal(f, err)
	}
	if _, err = s.DB.EnqueueUniqueTask(dbx.TaskArchiveSync, f.ID, "{}"); err == nil {
		t.Fatal("stale reconcile revived deleting file")
	}
}

func TestTaskCurrentAndHistoryAPIAreSeparate(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	for _, state := range []string{"pending", "running", "done", "canceled", "failed"} {
		id, err := s.DB.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec(`UPDATE tasks SET state=? WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		state string
		total int
	}{{"active", 2}, {"terminal", 3}} {
		r := request("GET", "/api/admin/logs?record=tasks&state="+test.state, nil, "")
		var data struct {
			Total int
			Items []struct{ State string }
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &data) != nil || data.Total != test.total {
			t.Fatal(r.Code, r.Body.String())
		}
		for _, item := range data.Items {
			active := item.State == "pending" || item.State == "running"
			if active != (test.state == "active") {
				t.Fatal(test.state, item)
			}
		}
	}
	r := request("GET", "/api/admin/tasks/summary", nil, "")
	var counts dbx.TaskCounts
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &counts) != nil || counts.Pending != 1 || counts.Running != 1 || counts.Failed != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
}
