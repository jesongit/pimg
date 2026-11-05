package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pimg/internal/dbx"
)

const loginBudgetPassword = "test-login-password"

func loginBudgetServer(t *testing.T) *Server {
	t.Helper()
	s := interactionServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(loginBudgetPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.EnsureAdmin("admin", string(hash)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"victim", "attacker", "Admin"} {
		if err := s.DB.CreateAccount("user:"+name, name, string(hash)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func loginBudgetCall(s *Server, ip, username, password, code string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(loginReq{Username: username, Password: password, Code: code})
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", &body)
	r.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestLoginBudgetUnrelatedSuccessDoesNotReset(t *testing.T) {
	for _, target := range []string{"admin", "victim"} {
		t.Run(target, func(t *testing.T) {
			s := loginBudgetServer(t)
			for i := 0; i < 4; i++ {
				if w := loginBudgetCall(s, "192.0.2.1", target, "wrong", ""); w.Code != 401 {
					t.Fatalf("guess %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if w := loginBudgetCall(s, "192.0.2.1", "attacker", loginBudgetPassword, ""); w.Code != 200 || len(w.Result().Cookies()) != 1 {
				t.Fatalf("own login: %d %s", w.Code, w.Body.String())
			}
			if w := loginBudgetCall(s, "192.0.2.1", target, "wrong", ""); w.Code != 401 {
				t.Fatalf("fifth guess: %d", w.Code)
			}
			// Check both budgets separately: a fresh source cannot attack the
			// target, and this source cannot log in to an unrelated account.
			if w := loginBudgetCall(s, "192.0.2.2", target, "wrong", ""); w.Code != 429 {
				t.Fatalf("target budget reset by unrelated login: %d", w.Code)
			}
			if w := loginBudgetCall(s, "192.0.2.1", "attacker", loginBudgetPassword, ""); w.Code != 429 {
				t.Fatalf("source budget reset by successful login: %d", w.Code)
			}
		})
	}
}

func TestLoginBudgetTargetAcrossIPsAndExactUsernames(t *testing.T) {
	for _, target := range []string{"admin", "victim", "unknown"} {
		t.Run(target, func(t *testing.T) {
			s := loginBudgetServer(t)
			for i := 1; i <= 5; i++ {
				if w := loginBudgetCall(s, fmt.Sprintf("192.0.2.%d", i), target, "wrong", ""); w.Code != 401 {
					t.Fatalf("guess %d: %d", i, w.Code)
				}
			}
			for _, password := range []string{"wrong", loginBudgetPassword} {
				if w := loginBudgetCall(s, "198.51.100.1", target, password, ""); w.Code != 429 {
					t.Fatalf("target budget bypass from fresh IP: %d", w.Code)
				}
			}
			// Username variants do not authenticate as the target. Case-distinct
			// real accounts retain their own independent budget.
			for _, variant := range []string{" " + target, target + " "} {
				if w := loginBudgetCall(s, "198.51.100.2", variant, loginBudgetPassword, ""); w.Code != 401 || len(w.Result().Cookies()) != 0 {
					t.Fatalf("variant %q: %d %s", variant, w.Code, w.Body.String())
				}
			}
			if target == "admin" {
				if w := loginBudgetCall(s, "198.51.100.3", "Admin", loginBudgetPassword, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"owner_id":"user:Admin"`) {
					t.Fatalf("case-distinct account: %d %s", w.Code, w.Body.String())
				}
				if w := loginBudgetCall(s, "198.51.100.4", target, "wrong", ""); w.Code != 429 {
					t.Fatalf("case-distinct success reset target: %d", w.Code)
				}
			}
		})
	}
}

func TestLoginBudgetSourceAcrossUsernames(t *testing.T) {
	s := loginBudgetServer(t)
	for i := 0; i < 5; i++ {
		if w := loginBudgetCall(s, "192.0.2.1", fmt.Sprintf("unknown-%d", i), "wrong", ""); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := loginBudgetCall(s, "192.0.2.1", "attacker", loginBudgetPassword, ""); w.Code != 429 {
		t.Fatalf("source budget bypass by changing usernames: %d", w.Code)
	}
	if w := loginBudgetCall(s, "192.0.2.2", "attacker", loginBudgetPassword, ""); w.Code != 200 {
		t.Fatalf("unrelated source/account blocked: %d", w.Code)
	}
}

func TestLoginBudgetCompleteSuccessResetsOwnUsername(t *testing.T) {
	s := loginBudgetServer(t)
	for i := 1; i <= 4; i++ {
		if w := loginBudgetCall(s, fmt.Sprintf("192.0.2.%d", i), "victim", "wrong", ""); w.Code != 401 {
			t.Fatalf("initial guess %d: %d", i, w.Code)
		}
	}
	if w := loginBudgetCall(s, "198.51.100.1", "victim", loginBudgetPassword, ""); w.Code != 200 {
		t.Fatalf("complete login: %d", w.Code)
	}
	for i := 1; i <= 5; i++ {
		if w := loginBudgetCall(s, fmt.Sprintf("203.0.113.%d", i), "victim", "wrong", ""); w.Code != 401 {
			t.Fatalf("guess after own login %d: %d", i, w.Code)
		}
	}
	if w := loginBudgetCall(s, "203.0.113.6", "victim", "wrong", ""); w.Code != 429 {
		t.Fatalf("missing renewed username limit: %d", w.Code)
	}
}

func TestLoginBudgetFactorChallengeDoesNotResetUsername(t *testing.T) {
	s := loginBudgetServer(t)
	factorSession(t, s, dbx.AdminOwner, "current")
	_, codes := enrollFactor(t, s, "current", loginBudgetPassword)
	for i := 1; i <= 4; i++ {
		if w := loginBudgetCall(s, fmt.Sprintf("192.0.2.%d", i), "admin", "wrong", ""); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := loginBudgetCall(s, "198.51.100.1", "admin", loginBudgetPassword, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "two_factor_required") || len(w.Result().Cookies()) != 0 {
		t.Fatalf("factor challenge: %d %s", w.Code, w.Body.String())
	}
	if w := loginBudgetCall(s, "192.0.2.5", "admin", "wrong", ""); w.Code != 401 {
		t.Fatalf("fifth guess: %d", w.Code)
	}
	if w := loginBudgetCall(s, "198.51.100.2", "admin", loginBudgetPassword, codes[0]); w.Code != 429 {
		t.Fatalf("factor challenge reset username budget: %d", w.Code)
	}
}

func TestLoginBudgetConcurrentFailures(t *testing.T) {
	for _, mode := range []string{"target-across-ips", "source-across-usernames"} {
		t.Run(mode, func(t *testing.T) {
			s := loginBudgetServer(t)
			const requests = 8
			started := make(chan struct{}, requests)
			release := make(chan struct{})
			statuses := make(chan int, requests)
			for i := 1; i <= requests; i++ {
				ip, username := fmt.Sprintf("192.0.2.%d", i), "admin"
				if mode == "source-across-usernames" {
					ip, username = "192.0.2.1", fmt.Sprintf("unknown-%d", i)
				}
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.RemoteAddr = ip + ":1234"
				payload, err := json.Marshal(loginReq{Username: username, Password: "wrong"})
				if err != nil {
					t.Fatal(err)
				}
				r.Body = &gatedAuthBody{reader: bytes.NewReader(payload), started: started, release: release}
				go func() {
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					statuses <- w.Code
				}()
			}
			for i := 0; i < requests; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("concurrent bodies did not reach reader")
				}
			}
			close(release)
			counts := map[int]int{}
			for i := 0; i < requests; i++ {
				select {
				case status := <-statuses:
					counts[status]++
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent login did not finish")
				}
			}
			if counts[401] != 5 || counts[429] != 3 {
				t.Fatalf("concurrent failure budget: %v", counts)
			}
		})
	}
}

func TestLoginGuardReclaimsExpiredBudgets(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for _, key := range []string{"192.0.2.1", loginUsernameKey("stale"), "factor-owner:stale"} {
		g.attempts[key] = &attempt{count: 5, until: now.Add(-time.Second)}
	}
	g.attempts["live"] = &attempt{count: 5, until: now.Add(time.Hour)}
	if !g.allow("fresh") || len(g.attempts) != 1 || g.allow("live") {
		t.Fatalf("expired entries not reclaimed or live budget changed: %v", g.attempts)
	}
	// Directly touched entries must expire even between full map sweeps.
	g.attempts["expired"] = &attempt{count: 5, until: now.Add(-time.Second)}
	if !g.allow("expired") {
		t.Fatal("expired budget still blocks")
	}
	g.fail("expired")
	if a := g.attempts["expired"]; a.count != 1 || !a.until.After(now.Add(14*time.Minute)) {
		t.Fatalf("expired budget was not renewed: %+v", a)
	}
	g.reset("expired")
	if _, ok := g.attempts["expired"]; ok {
		t.Fatal("reset did not remove budget")
	}
}
