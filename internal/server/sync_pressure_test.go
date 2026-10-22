package server

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"
)

type pressureSlowWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *pressureSlowWriter) Write(b []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestPressureSlowCachedDownloadPreemptsIdle(t *testing.T) {
	s := interactionServer(t)
	plain := interactionPNG(t)
	f, err := legacyUploadFixture(s.Svc,context.Background(), bytes.NewReader(plain), "slow.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='done' WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	idle, finish, err := s.Svc.BeginIdle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	w := &pressureSlowWriter{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	req := httptest.NewRequest("GET", "/api/admin/files/"+f.ID+"/content?kind=original&cached=1", nil)
	req.SetPathValue("id", f.ID)
	done := make(chan struct{})
	go func() { defer close(done); s.handleContent(w, req) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	select {
	case <-idle.Done():
	case <-time.After(750 * time.Millisecond):
		t.Error("slow cached download did not preempt idle transfer")
	}
	close(w.release)
	<-done
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatal("download changed", w.Code)
	}
	finish()
	next, stop, err := s.Svc.BeginIdle(context.Background())
	if err != nil {
		t.Fatal("download leaked foreground activity", err)
	}
	stop()
	_ = next
}
