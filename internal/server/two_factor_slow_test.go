package server

import (
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pimg/internal/dbx"
)

type stalledAuthBody struct{ started, release chan struct{} }

func (b *stalledAuthBody) Read([]byte) (int, error) { close(b.started); <-b.release; return 0, io.EOF }
func (b *stalledAuthBody) Close() error             { return nil }

func TestTwoFactorIncompleteBodiesDoNotHoldAuthenticationLock(t *testing.T) {
	for _, path := range []string{"/api/auth/login", "/api/auth/account", "/api/auth/2fa/setup"} {
		t.Run(path, func(t *testing.T) {
			s := interactionServer(t)
			factorSession(t, s, dbx.AdminOwner, "current")
			body := &stalledAuthBody{make(chan struct{}), make(chan struct{})}
			r := httptest.NewRequest("POST", path, nil)
			r.Body = body
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "current"})
			finished := make(chan struct{})
			go func() { defer close(finished); s.Handler().ServeHTTP(httptest.NewRecorder(), r) }()
			select {
			case <-body.started:
			case <-time.After(time.Second):
				close(body.release)
				t.Fatal("request did not start reading")
			}
			healthy := make(chan int, 1)
			go func() { healthy <- factorCall(s, "/api/auth/2fa", "", nil).Code }()
			select {
			case status := <-healthy:
				if status != 401 {
					t.Error("unexpected concurrent status", status)
				}
			case <-time.After(time.Second):
				t.Error("incomplete request blocked authentication")
			}
			close(body.release)
			<-finished
		})
	}
}

type gatedAuthBody struct {
	reader  io.Reader
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *gatedAuthBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{}; <-b.release })
	return b.reader.Read(p)
}
func (b *gatedAuthBody) Close() error { return nil }

func TestTwoFactorConcurrentPasswordBudgetAfterBodyRead(t *testing.T) {
	s := interactionServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err := s.DB.EnsureAdmin("admin", string(hash)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	statuses := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.Body = &gatedAuthBody{reader: strings.NewReader(`{"username":"admin","password":"wrong-password"}`), started: started, release: release}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			statuses <- w.Code
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("concurrent bodies did not reach reader")
		}
	}
	close(release)
	unauthorized, limited := 0, 0
	for i := 0; i < 8; i++ {
		switch status := <-statuses; status {
		case 401:
			unauthorized++
		case 429:
			limited++
		default:
			t.Fatal("unexpected status", status)
		}
	}
	if unauthorized != 5 || limited != 3 {
		t.Fatal("password attempt budget bypass", unauthorized, limited)
	}
}
