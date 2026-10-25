package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"pimg/internal/proxytraffic"
)

func TestStorageProxyTrafficHTTPValidationAndPersistence(t *testing.T) {
	s := interactionServer(t)
	patch := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleStorageConnection(w, httptest.NewRequest("PATCH", "/api/admin/storage/connection", bytes.NewBufferString(body)))
		if w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	patch(`{"proxy_traffic":{"daily_limit_bytes":0}}`, 400)
	patch(`{"proxy_traffic":{"monthly_reset_day":32}}`, 400)
	patch(`{"proxy_traffic":{"monthly_reset_day":1.5}}`, 400)
	patch(`{"proxy_traffic":{"daily_reset_time":"24:00"}}`, 400)
	patch(`{"proxy_traffic":{"daily_used_bytes":1}}`, 400)
	patch(`{"proxy_url":"http://127.0.0.1:7890","proxy_traffic":{}}`, 400)
	v := s.Svc.ProxyTrafficView()
	patch(fmt.Sprintf(`{"proxy_traffic":{"daily_limit_bytes":20000000000,"monthly_limit_bytes":600000000000,"daily_reset_time":"07:45","monthly_reset_day":31,"daily_used_bytes":1234,"day_start":%d}}`, v.DayStart), 200)
	v = s.Svc.ProxyTrafficView()
	if v.DailyLimit != 20*proxytraffic.GB || v.MonthlyLimit != 600*proxytraffic.GB || v.DailyUsed != 1234 || v.DailyReset != "07:45" || v.MonthlyResetDay != 31 {
		t.Fatal("API did not apply settings", v)
	}
	w := httptest.NewRecorder()
	s.handleStorage(w, httptest.NewRequest("GET", "/api/admin/storage", nil))
	var response struct {
		Connection struct {
			Traffic proxytraffic.View `json:"proxy_traffic"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Connection.Traffic.DailyUsed != 1234 {
		t.Fatal("traffic missing from storage response", w.Body.String())
	}
}
