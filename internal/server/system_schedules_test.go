package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
)

func TestSystemScheduleManagementAPISavesFutureRulesWithoutRunning(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	response := request("GET", "/api/admin/schedules", nil, "")
	var listing struct {
		Items []dbx.SystemSchedule `json:"items"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &listing) != nil || len(listing.Items) != 4 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "lease_token") || strings.Contains(response.Body.String(), "run_task_id") {
		t.Fatal("private lease fields exposed")
	}
	for _, item := range listing.Items {
		if item.NextRun <= time.Now().Unix() || item.State != "scheduled" {
			t.Fatal(item)
		}
	}
	for _, body := range []string{`{}`, `{"cron":"* * * * * *"}`, `{"cron":"@every 1s"}`, `{"cron":"0 0 31 2 *"}`, `{"timezone":"unknown/timezone"}`, `{"enabled":"yes"}`} {
		response = request("PATCH", "/api/admin/schedules/log_cleanup", strings.NewReader(body), "application/json")
		if response.Code != 400 {
			t.Fatal(body, response.Code, response.Body.String())
		}
	}
	log, _ := s.DB.SystemSchedule("log_cleanup")
	if log.Cron != "0 6 * * *" || log.Timezone != "Asia/Shanghai" {
		t.Fatal("invalid rule persisted", log)
	}
	response = request("PATCH", "/api/admin/schedules/log_cleanup", strings.NewReader(`{"cron":"30 7 * * *","timezone":"UTC"}`), "application/json")
	var updated struct {
		Schedule dbx.SystemSchedule `json:"schedule"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &updated) != nil || updated.Schedule.Cron != "30 7 * * *" || updated.Schedule.Timezone != "UTC" || updated.Schedule.NextRun <= time.Now().Unix() {
		t.Fatal(response.Code, response.Body.String())
	}
	var jobs int
	s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='system_schedule'`).Scan(&jobs)
	if jobs != 0 {
		t.Fatal("saving ran maintenance", jobs)
	}
	response = request("PATCH", "/api/admin/schedules/log_cleanup", strings.NewReader(`{"enabled":false}`), "application/json")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &updated) != nil || updated.Schedule.NextRun != 0 || updated.Schedule.State != "disabled" {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request("PATCH", "/api/admin/schedules/missing", strings.NewReader(`{"enabled":true}`), "application/json")
	if response.Code != 404 {
		t.Fatal(response.Code)
	}
	for _, target := range []struct{ method, path string }{{"GET", "/api/admin/schedules"}, {"PATCH", "/api/admin/schedules/log_cleanup"}} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(target.method, target.path, strings.NewReader(`{"enabled":true}`)))
		if w.Code != 401 {
			t.Fatal("plan management not authenticated", w.Code)
		}
	}
}

func TestSystemScheduleRunningEditAndOrdinaryTaskControls(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	at := time.Now().Unix()
	s.DB.PrepareSystemSchedules(at, true)
	s.DB.Exec(`UPDATE system_schedules SET next_run=? WHERE id='log_cleanup'`, at)
	run, err := s.DB.ClaimSystemSchedule(at, "test-system-owner", 90)
	if err != nil {
		t.Fatal(err)
	}
	response := request("PATCH", "/api/admin/schedules/log_cleanup", strings.NewReader(`{"enabled":false}`), "application/json")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	job, _ := s.DB.GetTask(run.TaskID)
	if job.State != "running" {
		t.Fatal("editing interrupted current execution", job)
	}
	for _, suffix := range []string{"retry", "cancel", "retry?group=1", "cancel?group=1"} {
		response = request("POST", fmt.Sprintf("/api/admin/tasks/%d/%s", run.TaskID, suffix), strings.NewReader(`{}`), "application/json")
		if response.Code != 409 || !strings.Contains(response.Body.String(), "系统定时任务") {
			t.Fatal(suffix, response.Code, response.Body.String())
		}
	}
	if err = s.DB.FinishSystemSchedule(run.ID, run.LeaseToken, run.TaskID, at+1, map[string]any{"changed": 1, "summary": "测试清理"}, nil); err != nil {
		t.Fatal(err)
	}
	finished, _ := s.DB.SystemSchedule(run.ID)
	if finished.Enabled || finished.State != "disabled" || finished.NextRun != 0 || finished.LastStatus != "done" {
		t.Fatal(finished)
	}
	response = request("GET", "/api/admin/logs?record=all", nil, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "日志过期清理") {
		t.Fatal("summary name missing", response.Code, response.Body.String())
	}
}
