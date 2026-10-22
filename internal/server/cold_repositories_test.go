package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

func coldRepositoryServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, Upload: config.Upload{MaxSizeMB: 16}}
	svc, err := store.New(cfg, db, master, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, db, svc, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestColdRepositoryPrimaryRequiredAndSecondaryOptIn(t *testing.T) {
	s := coldRepositoryServer(t)
	for _, body := range []string{`{"name":"bad","driver":"local","role":"primary","config":{"directory":"replicas/a"}}`, `{"name":"bad","driver":"unknown","config":{}}`, `{"name":"bad","driver":"local","config":{"directory":"../outside"}}`} {
		w := httptest.NewRecorder()
		s.handleAddColdRepository(w, httptest.NewRequest("POST", "/", bytes.NewBufferString(body)))
		if w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.handleAddColdRepository(w, httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"name":"backup","driver":"local","config":{"directory":"replicas/a"}}`)))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleColdRepositories(w, httptest.NewRequest("GET", "/", nil))
	var result struct {
		Primary struct {
			ID, Driver string
			Enabled    bool
		}
		Items []struct {
			ID      int64
			Enabled bool
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Primary.ID != "primary" || result.Primary.Driver != "local" || !result.Primary.Enabled || len(result.Items) != 1 || result.Items[0].Enabled {
		t.Fatal("missing primary or automatically selected secondary", w.Body.String())
	}
	for _, id := range []string{"primary", "0", "-1"} {
		req := httptest.NewRequest("DELETE", "/", nil)
		req.SetPathValue("id", id)
		w = httptest.NewRecorder()
		s.handleRemoveColdRepository(w, req)
		if w.Code != 400 {
			t.Fatal("primary removal allowed", w.Code)
		}
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/cold-repositories", nil))
	if w.Code != 401 {
		t.Fatal("repository configuration was not authenticated", w.Code)
	}
}

func TestFileDeleteAutomaticallyCancelsQueuedTask(t *testing.T) {
	s := coldRepositoryServer(t)
	f := &dbx.File{ID: "referenced", Name: "test", OrigName: "test.png", Kind: "image", Ext: "png", Mime: "image/png", KeyWrapped: []byte{1}, KeyNonce: []byte{1}, ArchiveState: "done", ColdBackend: "local"}
	if err := s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	task, err := s.DB.EnqueueTask(dbx.TaskColdSync, f.ID, `{"repository_id":1}`)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("DELETE", "/api/admin/files/referenced?confirm=PURGE", nil)
	request.SetPathValue("id", f.ID)
	w := httptest.NewRecorder()
	s.handleDeleteFile(w, request)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	stored, _ := s.DB.GetFile(f.ID)
	if stored.PurgeState.String != "pending" {
		t.Fatal("deletion was not queued")
	}
	state, _ := s.DB.GetTask(task)
	if state.State != "canceled" {
		t.Fatal("deletion did not cancel queued work")
	}
	if _, err = s.DB.EnqueuePurgeTask(f.ID); err != nil {
		t.Fatal("repeat deletion should reuse its task", err)
	}
	if err = s.DB.DeleteFile(f.ID); !errors.Is(err, dbx.ErrTaskReferenced) {
		t.Fatal("direct database delete bypassed the deletion task", err)
	}
}
