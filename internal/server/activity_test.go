package server

import (
	"encoding/json"
	"net/http/httptest"
	"pimg/internal/dbx"
	"testing"
)

func TestActivityAPIScopeAndPaging(t *testing.T) {
	s := coldRepositoryServer(t)
	f := &dbx.File{ID: "activity-file", Kind: "image", KeyWrapped: []byte{}, KeyNonce: []byte{}, Name: "stored", OrigName: "test.png", ArchiveState: "pending"}
	if err := s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 51; i++ {
		if _, err := s.DB.EnqueueTask(dbx.TaskVerify, f.ID, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/admin/tasks/summary", "/api/admin/logs/receipts", "/api/admin/logs/receipts/1"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.handleActivityTasks(w, httptest.NewRequest("GET", "/?view=current&file_id=activity-file&page=2", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
		Next  int              `json:"next_page"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 51 || page.Next != 0 || len(page.Items) != 1 || page.Items[0]["file_name"] != "test.png" {
		t.Fatal(page)
	}
	w = httptest.NewRecorder()
	s.handleTaskSummary(w, httptest.NewRequest("GET", "/?file_id=activity-file", nil))
	var c dbx.TaskCounts
	json.Unmarshal(w.Body.Bytes(), &c)
	if c.Pending != 51 {
		t.Fatal(c)
	}
	w = httptest.NewRecorder()
	s.handleActivityTasks(w, httptest.NewRequest("GET", "/?view=invalid", nil))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	s.handleReceiptLogs(w, httptest.NewRequest("GET", "/?file_id=activity-file", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
