package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFastConditionalSkipsQueryAfterAuthAndInvalidatesEveryCommit(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	_ = call // Installs only this isolated test session.
	queries := 0
	h := conditionalJSON(s.auth(func(w http.ResponseWriter, r *http.Request) {
		queries++
		var value string
		if err := s.DB.ReadQueryRow(`SELECT v FROM settings WHERE k='server_name'`).Scan(&value); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		writeJSON(w, 200, map[string]any{"value": value, "query": r.URL.RawQuery})
	}))
	request := func(query, tag string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/admin/logs"+query, nil)
		r.Header.Set("If-None-Match", tag)
		if auth {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "carrier-test-session"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	first := request("?record=all", "", true)
	tag := first.Header().Get("ETag")
	if first.Code != 200 || tag == "" || queries != 1 {
		t.Fatal(first.Code, first.Body.String(), queries)
	}
	same := request("?record=all", tag, true)
	if same.Code != 304 || same.Body.Len() != 0 || queries != 1 {
		t.Fatal("unchanged read ran the full query", same.Code, queries)
	}
	reloaded := request("?record=all", "", true)
	if reloaded.Code != 200 || reloaded.Body.String() != first.Body.String() || queries != 1 {
		t.Fatal("reload without ETag rebuilt the same authorized view", reloaded.Code, queries)
	}
	if denied := request("?record=all", tag, false); denied.Code != 401 || queries != 1 {
		t.Fatal("validator bypassed authentication", denied.Code, queries)
	}
	other := request("?record=tasks", tag, true)
	if other.Code != 200 || queries != 2 {
		t.Fatal("validator crossed query scope", other.Code, queries)
	}
	if _, err := s.DB.Exec(`INSERT INTO settings(k,v) VALUES('server_name','changed') ON CONFLICT(k) DO UPDATE SET v=excluded.v`); err != nil {
		t.Fatal(err)
	}
	changed := request("?record=all", tag, true)
	if changed.Code != 200 || queries != 3 || changed.Header().Get("ETag") == tag {
		t.Fatal("committed change reused old validator", changed.Code, queries)
	}
	// Time-based input expiration invalidates even when no writer has committed.
	currentTag := changed.Header().Get("ETag")
	s.queryValidators.mu.Lock()
	for key, entry := range s.queryValidators.entries {
		entry.expiry = time.Now().Unix()
		s.queryValidators.entries[key] = entry
	}
	s.queryValidators.mu.Unlock()
	expired := request("?record=all", currentTag, true)
	if expired.Code != 304 || queries != 4 {
		t.Fatal("deadline skipped a required refresh", expired.Code, queries)
	}
	if _, err := s.DB.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	if denied := request("?record=all", currentTag, true); denied.Code != 401 || queries != 4 {
		t.Fatal("revoked session reused validator", denied.Code, queries)
	}
}
