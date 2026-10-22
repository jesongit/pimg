package server

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestTelegramWebConfigRejectsIncompleteApplicationCredentials(t *testing.T) {
	s := coldRepositoryServer(t)
	for _, body := range []string{`{"api_id":123}`, `{"api_id":0}`, `{"api_hash":"fixture"}`, `{"api_hash":""}`} {
		w := httptest.NewRecorder()
		s.handleTelegramConfig(w, httptest.NewRequest("POST", "/", bytes.NewBufferString(body)))
		if w.Code < 400 {
			t.Fatal("application credentials were accepted by web configuration", w.Code)
		}
	}
	values, err := s.DB.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	if values["telegram.api_id"] != "" || values["telegram.api_hash"] != "" {
		t.Fatal("web application credentials were persisted")
	}
}

func TestTelegramWebConfigAcceptsCompleteCredentials(t *testing.T) {
	s := coldRepositoryServer(t)
	w := httptest.NewRecorder()
	s.handleTelegramConfig(w, httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"api_id":123,"api_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	values, err := s.DB.AllSettings()
	if err != nil || values["storage.connection.v1"] == "" {
		t.Fatal("encrypted connection config missing", err)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")) {
		t.Fatal("secret reflected")
	}
}
