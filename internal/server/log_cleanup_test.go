package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"pimg/internal/dbx"
	"testing"
	"time"
)

func TestLogRetentionSettingsAndManualCleanupAPI(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	r := request("GET", "/api/admin/settings", nil, "")
	var settings struct {
		Days int `json:"log_retention_days"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &settings) != nil || settings.Days != 30 {
		t.Fatal(r.Code, r.Body.String())
	}
	old := time.Now().Unix() - 8*86400
	id, err := s.DB.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='done',created_at=?,updated_at=?,done_at=? WHERE id=?`, old, old, old, id); err != nil {
		t.Fatal(err)
	}
	r = request("POST", "/api/admin/logs/cleanup", bytes.NewBufferString(`{}`), "application/json")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	job, err := s.DB.GetTask(id)
	if err != nil || job.State != "done" {
		t.Fatal("default cutoff ignored", job, err)
	}
	for _, value := range []string{"0", "-1", "1.5", "3651", `"7"`, "null"} {
		r = request("PATCH", "/api/admin/settings", bytes.NewBufferString(`{"log_retention_days":`+value+`}`), "application/json")
		if r.Code != 400 {
			t.Fatal(value, r.Code, r.Body.String())
		}
		if days, e := s.DB.LogRetentionDays(); e != nil || days != 30 {
			t.Fatal("invalid policy persisted", days, e)
		}
	}
	r = request("PATCH", "/api/admin/settings", bytes.NewBufferString(`{"log_retention_days":7}`), "application/json")
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &settings) != nil || settings.Days != 7 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = request("POST", "/api/admin/logs/cleanup", bytes.NewBufferString(`{}`), "application/json")
	var result dbx.LogCleanupResult
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.RetentionDays != 7 || result.Tasks != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err = s.DB.GetTask(id); err == nil {
		t.Fatal("manual cleanup did not apply saved policy")
	}
	r = request("POST", "/api/admin/logs/cleanup", bytes.NewBufferString(`{}`), "application/json")
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Tasks != 0 {
		t.Fatal("cleanup is not repeatable", r.Code, r.Body.String())
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/admin/logs/cleanup", bytes.NewBufferString(`{}`)))
	if w.Code != 401 {
		t.Fatal("cleanup is not authenticated", w.Code)
	}
}
