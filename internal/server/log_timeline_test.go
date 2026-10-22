package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestUnifiedLogTimelineAndClearRequireAdminAndExplicitConfirmation(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	done, _ := s.DB.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
	s.DB.FinishTask(done, "{}")
	failed, _ := s.DB.EnqueueTask(dbx.TaskVerify, "", "{}")
	s.DB.Exec(`UPDATE tasks SET state='failed',last_error='recoverable fixture' WHERE id=?`, failed)
	pending, _ := s.DB.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
	for _, payload := range []string{`{}`, `{"confirm":""}`, `{"confirm":"PURGE"}`, `{"confirm":true}`, `not json`} {
		response := request("POST", "/api/admin/logs/clear", strings.NewReader(payload), "application/json")
		if response.Code != 400 {
			t.Fatal(payload, response.Code, response.Body.String())
		}
	}
	if revision, _, err := s.DB.LogVisibilityBoundary(); err != nil || revision != -1 {
		t.Fatal("invalid confirmation changed visibility", revision, err)
	}
	for _, query := range []string{"page=0", "page=bad", "group_id=-1", "q=" + strings.Repeat("a", 513)} {
		response := request("GET", "/api/admin/logs?record=all&"+query, nil, "")
		if response.Code != 400 {
			t.Fatal(query, response.Code)
		}
	}
	for _, target := range []struct{ method, path string }{{"GET", "/api/admin/logs?record=all"}, {"POST", "/api/admin/logs/clear"}} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(target.method, target.path, bytes.NewBufferString(`{"confirm":"DELETE_ALL_LOGS"}`)))
		if w.Code != 401 {
			t.Fatal("unauthenticated log access", target, w.Code)
		}
	}
	owner := "user:log-clear"
	s.DB.CreateAccount(owner, "log-clear-user", "hash")
	sum := sha256.Sum256([]byte(owner))
	s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])})
	if w := uploaderCall(s, "POST", "/api/admin/logs/clear", owner, map[string]any{"confirm": "DELETE_ALL_LOGS"}); w.Code != 401 {
		t.Fatal("ordinary account cleared global logs", w.Code, w.Body.String())
	}
	response := request("GET", "/api/admin/logs?record=all", nil, "")
	var listing struct {
		Total int `json:"total"`
		Items []struct {
			RecordKey string `json:"record_key"`
			State     string `json:"state"`
		} `json:"items"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Total != 2 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request("POST", "/api/admin/logs/clear", strings.NewReader(`{"confirm":"DELETE_ALL_LOGS"}`), "application/json")
	var cleared dbx.ClearLogsResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &cleared) != nil || cleared.Hidden != 2 || cleared.RemovedTasks != 1 || cleared.RetainedActive != 1 || cleared.RetainedRecovery != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	if job, err := s.DB.GetTask(failed); err != nil || job.State != "failed" {
		t.Fatal("clear removed failed recovery work", job, err)
	}
	response = request("GET", "/api/admin/logs?record=all", nil, "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Total != 0 || len(listing.Items) != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request("GET", "/api/admin/logs?record=tasks&state=active", nil, "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Total != 1 {
		t.Fatal("existing Tasks view changed", response.Code, response.Body.String())
	}
	s.DB.FinishTask(pending, "{}")
	response = request("GET", "/api/admin/logs?record=all", nil, "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Total != 1 || listing.Items[0].State != "done" {
		t.Fatal("new result not visible after clear", response.Code, response.Body.String())
	}
	if days, err := s.DB.LogRetentionDays(); err != nil || days != 30 {
		t.Fatal("clear changed automatic retention", days, err)
	}
}

func TestClearLogHTTPFailureLeavesHistoryAndBoundaryUntouched(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	id, _ := s.DB.EnqueueTask(dbx.TaskCleanupTemp, "", "{}")
	s.DB.FinishTask(id, "{}")
	s.DB.Exec(`CREATE TRIGGER fail_clear_api BEFORE UPDATE OF cleared_revision ON log_visibility BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	response := request("POST", "/api/admin/logs/clear", strings.NewReader(`{"confirm":"DELETE_ALL_LOGS"}`), "application/json")
	if response.Code != 500 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := s.DB.GetTask(id); err != nil {
		t.Fatal("HTTP failure deleted task", err)
	}
	if revision, _, err := s.DB.LogVisibilityBoundary(); err != nil || revision != -1 {
		t.Fatal("HTTP failure hid history", revision, err)
	}
}
