package server

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConditionalJSONAuthenticatesAndReturnsOnlyChangedRepresentation(t *testing.T) {
	s := interactionServer(t)
	call := carrierRequest(t, s)
	seedInteractionFile(t, s, "retained", 1, sql.NullInt64{})
	first := call("GET", "/api/admin/files?per_page=60", nil, "")
	tag := first.Header().Get("ETag")
	if first.Code != 200 || tag == "" || first.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(first.Code, first.Header())
	}
	cookie := call("GET", "/api/auth/me", nil, "") // independently protected auth stays uncached
	if cookie.Header().Get("ETag") != "" {
		t.Fatal("authentication cached")
	}
	// carrierRequest uses this fixture's named session; no production credentials.
	request := func(auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/admin/files?per_page=60", nil)
		r.Header.Set("If-None-Match", tag)
		if auth {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "carrier-test-session"})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	same := request(true)
	if same.Code != 304 || same.Body.Len() != 0 {
		t.Fatal(same.Code, same.Body.String())
	}
	denied := request(false)
	if denied.Code != 401 || denied.Header().Get("ETag") != "" {
		t.Fatal("ETag bypassed auth", denied.Code)
	}
	if _, err := s.DB.Exec(`UPDATE files SET name='changed' WHERE id='retained'`); err != nil {
		t.Fatal(err)
	}
	changed := request(true)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag || !bytes.Contains(changed.Body.Bytes(), []byte("changed")) {
		t.Fatal(changed.Code, changed.Body.String())
	}
}
func TestConditionalJSONDoesNotBufferLargeOrMediaResponses(t *testing.T) {
	value := bytes.Repeat([]byte("x"), 5<<20)
	h := conditionalJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(value)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/files", nil))
	if w.Header().Get("ETag") != "" || !bytes.Equal(w.Body.Bytes(), value) {
		t.Fatal("large fallback corrupted")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/files/id/content", nil))
	if w.Header().Get("ETag") != "" || !bytes.Equal(w.Body.Bytes(), value) {
		t.Fatal("media was intercepted")
	}
}
