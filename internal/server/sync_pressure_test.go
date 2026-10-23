package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pimg/internal/dbx"
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
	pressureAuth(t, s, dbx.AdminOwner)
	plain := interactionPNG(t)
	f, err := legacyUploadFixture(s.Svc, context.Background(), bytes.NewReader(plain), "slow.png", "png", sql.NullInt64{}, nil, 0, false)
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
	req.Header.Set("Authorization", "Bearer pressure-test")
	go func() { defer close(done); s.Handler().ServeHTTP(w, req) }()
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

func pressureAuth(t *testing.T, s *Server, owner string) {
	t.Helper()
	if err := s.DB.EnsureAdmin("admin", "hash"); err != nil {
		t.Fatal(err)
	}
	scope := "admin"
	if owner != dbx.AdminOwner {
		scope = "account"
		if err := s.DB.CreateAccount(owner, "pressure-user", "hash"); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte("pressure-test"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "pressure", Name: "pressure", Scope: scope, OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
}

func TestPressureAuthenticatedPreviewsDoNotPreemptIdle(t *testing.T) {
	for _, owner := range []string{dbx.AdminOwner, "user:pressure"} {
		t.Run(owner, func(t *testing.T) {
			s := interactionServer(t)
			pressureAuth(t, s, owner)
			f, err := legacyUploadFixture(s.Svc, dbx.WithOwner(context.Background(), owner), bytes.NewReader(interactionPNG(t)), "preview.png", "png", sql.NullInt64{}, nil, 0, false)
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
			prefix := "/api/admin/files"
			if owner != dbx.AdminOwner {
				prefix = "/api/account/files"
			}
			paths := []string{prefix + "/" + f.ID + "/content?kind=thumb", prefix + "/previews?ids=" + f.ID, prefix + "/" + f.ID + "/content?kind=original&cached=1"}
			for i := 0; i < 6; i++ {
				for _, path := range paths {
					w := uploaderCall(s, "GET", path, "pressure-test", nil)
					if w.Code != http.StatusOK || w.Body.Len() == 0 {
						t.Fatal(path, w.Code, w.Body.String())
					}
				}
			}
			if idle.Err() != nil {
				t.Fatal("cached previews canceled idle archive", idle.Err())
			}
			// Authenticated mutations must still yield immediately, before IO.
			r := httptest.NewRequest("DELETE", prefix+"/"+f.ID, nil)
			r.Header.Set("Authorization", "Bearer pressure-test")
			check := func(w http.ResponseWriter, _ *http.Request) {
				if idle.Err() == nil {
					t.Error("mutation did not immediately preempt idle archive")
				}
			}
			if owner == dbx.AdminOwner {
				s.auth(check)(httptest.NewRecorder(), r)
			} else {
				r.SetPathValue("id", f.ID)
				s.ownFile(check)(httptest.NewRecorder(), r)
			}
		})
	}
}
