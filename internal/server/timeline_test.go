package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"pimg/internal/dbx"
)

func TestTimelineAPIAuthScopeSeekAndValidation(t *testing.T) {
	s := interactionServer(t)
	album, err := s.DB.InsertAlbum("Timeline scope")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, date string
		group    bool
	}{{"scope-new", "2026-10-18", true}, {"scope-old", "2023-01-01", true}, {"outside", "2020-01-01", false}} {
		assigned := sql.NullInt64{}
		if item.group {
			assigned = sql.NullInt64{Int64: album, Valid: true}
		}
		seedInteractionFile(t, s, item.id, 1, assigned)
		stamp, _ := time.Parse("2006-01-02", item.date)
		if _, err := s.DB.Exec("UPDATE files SET created_at=? WHERE id=?", stamp.Unix(), item.id); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte("timeline-fixture"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "timeline-fixture", Name: "test", TokenHash: fmt.Sprintf("%x", sum)}); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(address string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", address, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer timeline-fixture")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("/api/admin/files/timeline", false); w.Code != 401 {
		t.Fatalf("date index leaked without auth: %d %s", w.Code, w.Body)
	}
	address := fmt.Sprintf("/api/admin/files/timeline?album_id=%d&kind=image&tz_offset=480", album)
	w := request(address, true)
	var timeline struct {
		Months []dbx.FileMonth `json:"months"`
		Groups []dbx.FileMonth `json:"groups"`
		Group  string          `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &timeline); err != nil || w.Code != 200 || len(timeline.Months) != 2 || timeline.Months[1].Key != "2023-01" {
		t.Fatalf("wrong all-record scope: %d %s %v", w.Code, w.Body, err)
	}
	if timeline.Group != "month" || len(timeline.Groups) != len(timeline.Months) || timeline.Groups[1] != timeline.Months[1] {
		t.Fatalf("default month compatibility changed: %s", w.Body)
	}
	w = request(fmt.Sprintf("/api/admin/files?album_id=%d&before=%d&per_page=1", album, timeline.Months[1].End), true)
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 || list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != "scope-old" {
		t.Fatalf("date seek failed: %d %s %v", w.Code, w.Body, err)
	}
	for _, test := range []struct{ group, newest, oldest string }{
		{"day", "2026-10-18", "2023-01-01"},
		{"week", "2026-10-12", "2022-12-26"},
		{"month", "2026-10", "2023-01"},
		{"year", "2026", "2023"},
	} {
		w = request(address+"&group="+test.group, true)
		if err := json.Unmarshal(w.Body.Bytes(), &timeline); err != nil || w.Code != 200 || timeline.Group != test.group || len(timeline.Groups) != 2 || len(timeline.Months) != 2 || timeline.Groups[0].Key != test.newest || timeline.Groups[1].Key != test.oldest || timeline.Groups[0].Count != 1 || timeline.Groups[1].Count != 1 || timeline.Groups[0] != timeline.Months[0] {
			t.Errorf("wrong %s response: %d %s %v", test.group, w.Code, w.Body, err)
		}
	}
	for _, address := range []string{"/api/admin/files?before=-2208988801", "/api/admin/files?before=abc", "/api/admin/files?before=2&after=2", "/api/admin/files/timeline?tz_offset=999", "/api/admin/files/timeline?tz_offset=abc", "/api/admin/files/timeline?group=decade"} {
		if w := request(address, true); w.Code != 400 {
			t.Errorf("invalid parameter accepted: %s %d", address, w.Code)
		}
	}
}
