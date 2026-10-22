package server

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"testing"

	"pimg/internal/dbx"
)

func TestEncryptedPermanentPublishRetryIsRejectedAtHTTPEntry(t *testing.T) {
	s := interactionServer(t)
	seedInteractionFile(t, s, "encrypted-publish-retry", 1, sql.NullInt64{})
	taskID, err := s.DB.EnqueueTask(dbx.TaskPublish, "encrypted-publish-retry", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE tasks SET state='failed' WHERE id=?", taskID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", fmt.Sprintf("/api/admin/tasks/%d/retry", taskID), nil)
	request.SetPathValue("id", fmt.Sprint(taskID))
	response := httptest.NewRecorder()
	s.handleRetryTask(response, request)
	task, err := s.DB.GetTask(taskID)
	if err != nil || response.Code != 409 || task.State != "failed" {
		t.Fatal("forbidden permanent retry entered the worker queue", response.Code, err)
	}
}
