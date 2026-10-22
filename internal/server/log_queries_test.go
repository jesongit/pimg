package server

import (
	"encoding/json"
	"net/http/httptest"
	"pimg/internal/dbx"
	"testing"
)

func TestUnifiedLogFiltersValidationAndAdminAuth(t *testing.T) {
	s := coldRepositoryServer(t)
	if e := s.DB.RecordIngestEvent(dbx.IngestEvent{SourceKey: "failed-test", Type: "failed", Reason: "upload rejected", Body: json.RawMessage(`{"orig_name":"test.png"}`)}); e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/logs", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, query := range []string{"record=bad", "sort=DROP+TABLE+tasks", "record=receipts&sort=attempts_desc", "state=unknown", "from=invalid", "page=-1", "page=1000001"} {
		w = httptest.NewRecorder()
		s.handleUnifiedLogs(w, httptest.NewRequest("GET", "/?"+query, nil))
		if w.Code != 400 {
			t.Fatal(query, w.Code, w.Body)
		}
	}
	w = httptest.NewRecorder()
	s.handleUnifiedLogs(w, httptest.NewRequest("GET", "/?record=receipts&state=failed&q=test.png&sort=created_asc", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var data struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &data); e != nil {
		t.Fatal(e)
	}
	if data.Total != 1 || data.Items[0]["record"] != "receipt" || data.Items[0]["attempts"] != nil || data.Items[0]["file_name"] != "test.png" {
		t.Fatal(data)
	}
}
