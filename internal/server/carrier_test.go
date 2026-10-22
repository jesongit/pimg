package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
)

func carrierRequest(t *testing.T, s *Server) func(string, string, io.Reader, string) *httptest.ResponseRecorder {
	t.Helper()
	const session = "carrier-test-session"
	hash := sha256.Sum256([]byte(session))
	if err := s.DB.InsertSession(hex.EncodeToString(hash[:]), time.Now().Add(time.Hour).Unix(), "test", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	return func(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
}

// The removed web tools must neither create archive records nor return a PNG.
func TestCarrierWebToolsRemoved(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	f, err := s.Svc.ReceiveUpload(t.Context(), bytes.NewReader(interactionPNG(t)), "original.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/admin/import"},
		{"GET", "/api/admin/files/" + f.ID + "/export"},
		{"HEAD", "/api/admin/files/" + f.ID + "/export"},
	} {
		response := request(endpoint.method, endpoint.path, strings.NewReader("removed carrier"), "image/png")
		if response.Code != http.StatusNotFound || response.Header().Get("Content-Disposition") != "" {
			t.Fatalf("removed endpoint %s: status=%d disposition=%q", endpoint.path, response.Code, response.Header().Get("Content-Disposition"))
		}
	}
	_, total, err := s.DB.ListFiles(dbx.FileFilter{})
	if err != nil || total != 1 {
		t.Fatalf("removed import changed the library: total=%d err=%v", total, err)
	}
}
