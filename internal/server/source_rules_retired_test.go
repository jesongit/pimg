package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceRuleEndpointsRetiredAndStorageKeepsCoreFields(t *testing.T) {
	s := importServer(t)
	request := carrierRequest(t, s)
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/admin/storage/import-rules"},
		{"PATCH", "/api/admin/storage/import-rules/1"},
		{"POST", "/api/admin/storage/import-rules/1/scan"},
		{"GET", "/api/admin/storage/receipts"},
	} {
		response := request(endpoint.method, endpoint.path, strings.NewReader(`{}`), "application/json")
		if response.Code != 410 {
			t.Fatalf("%s %s: %d %s", endpoint.method, endpoint.path, response.Code, response.Body.String())
		}
	}
	response := request("GET", "/api/admin/storage", nil, "")
	var storage map[string]json.RawMessage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &storage) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, field := range []string{"instances", "drivers", "policy", "connection"} {
		if len(storage[field]) == 0 {
			t.Fatal("core storage field missing", field)
		}
	}
	if _, exists := storage["rules"]; exists {
		t.Fatal("retired rule configuration still exposed")
	}
	response = request("PATCH", "/api/admin/settings", strings.NewReader(`{"theme":"light"}`), "application/json")
	if response.Code != 400 {
		t.Fatal("removed theme setting remains writable", response.Code)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM import_rules`).Scan(&count); err != nil || count != 0 {
		t.Fatal("retired endpoint wrote a rule", count, err)
	}
}
