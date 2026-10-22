package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"pimg/internal/dbx"
	"pimg/internal/totp"
)

func factorCall(s *Server, path, cookie string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	method := "GET"
	if body != nil {
		method = "POST"
		json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.RemoteAddr = "127.0.0.1:1234"
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func factorSession(t *testing.T, s *Server, owner, token string) {
	t.Helper()
	h := sha256.Sum256([]byte(token))
	if err := s.DB.InsertOwnerSession(owner, hex.EncodeToString(h[:]), time.Now().Add(time.Hour).Unix(), "test", ""); err != nil {
		t.Fatal(err)
	}
}

func enrollFactor(t *testing.T, s *Server, cookie, password string) ([]byte, []string) {
	t.Helper()
	w := factorCall(s, "/api/auth/2fa/setup", cookie, map[string]string{"password": password})
	var setup struct {
		Secret string `json:"secret"`
		QR     string `json:"qr_image"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &setup) != nil || !strings.HasPrefix(setup.QR, "data:image/png;base64,") {
		t.Fatal("setup", w.Code, w.Body.String())
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	w = factorCall(s, "/api/auth/2fa/enable", cookie, map[string]string{"password": password, "code": totp.Code(secret, time.Now().Unix()/30)})
	var result struct {
		Codes []string `json:"recovery_codes"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Codes) != 10 {
		t.Fatal("enable", w.Code, w.Body.String())
	}
	return secret, result.Codes
}

func TestTwoFactorAccountLifecycle(t *testing.T) {
	for _, owner := range []string{dbx.AdminOwner, "user:alpha"} {
		t.Run(owner, func(t *testing.T) {
			s := interactionServer(t)
			password := "test-password"
			hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
			if err := s.DB.EnsureAdmin("admin", string(hash)); err != nil {
				t.Fatal(err)
			}
			username := "admin"
			if owner != dbx.AdminOwner {
				username = "alpha"
				if err := s.DB.CreateAccount(owner, username, string(hash)); err != nil {
					t.Fatal(err)
				}
			}
			factorSession(t, s, owner, "current")
			factorSession(t, s, owner, "old")
			factorSession(t, s, dbx.AdminOwner, "other-owner")
			secret, codes := enrollFactor(t, s, "current", password)
			old := sha256.Sum256([]byte("old"))
			if _, err := s.DB.GetSession(hex.EncodeToString(old[:])); err != dbx.ErrNotFound {
				t.Fatal("other session survived enrollment", err)
			}
			if w := factorCall(s, "/api/auth/2fa", "current", nil); w.Code != 200 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), codes[0]) {
				t.Fatal("status leaked", w.Code, w.Body.String())
			}
			f, err := s.DB.TwoFactor(owner)
			if err != nil || bytes.Contains(f.Secret, secret) {
				t.Fatal("secret not encrypted", err)
			}
			if _, err = s.factorSecret("user:other", f); err == nil {
				t.Fatal("secret not owner-bound")
			}
			login := func(code string) *httptest.ResponseRecorder {
				return factorCall(s, "/api/auth/login", "", map[string]string{"username": username, "password": password, "code": code})
			}
			w := login("")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "two_factor_required") || len(w.Result().Cookies()) != 0 {
				t.Fatal("password issued session", w.Code, w.Body.String())
			}
			w = login(totp.Code(secret, time.Now().Unix()/30))
			if w.Code != 400 || len(w.Result().Cookies()) != 0 {
				t.Fatal("enrollment code replay", w.Code)
			}
			w = login(codes[0])
			if w.Code != 200 || len(w.Result().Cookies()) != 1 {
				t.Fatal("recovery login", w.Code, w.Body.String())
			}
			if w = login(codes[0]); w.Code != 400 || len(w.Result().Cookies()) != 0 {
				t.Fatal("recovery replay", w.Code)
			}
			if w = factorCall(s, "/api/auth/2fa/setup", "current", map[string]string{"password": password}); w.Code != 409 {
				t.Fatal("replaced live factor", w.Code)
			}
			if owner == dbx.AdminOwner {
				w = factorCall(s, "/api/auth/account", "current", map[string]string{"username": "new-admin", "old_password": password})
				if w.Code != 400 {
					t.Fatal("changed credentials without factor", w.Code)
				}
			}
			factorSession(t, s, owner, "before-rotate")
			w = factorCall(s, "/api/auth/2fa/recovery", "current", map[string]string{"password": password, "code": codes[1]})
			var rotated struct {
				Codes []string `json:"recovery_codes"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rotated) != nil || len(rotated.Codes) != 10 {
				t.Fatal("rotate", w.Code, w.Body.String())
			}
			if w = login(codes[2]); w.Code != 400 {
				t.Fatal("old recovery after rotation", w.Code)
			}
			w = factorCall(s, "/api/auth/2fa/disable", "current", map[string]string{"password": password, "code": rotated.Codes[0]})
			if w.Code != 200 {
				t.Fatal("disable", w.Code, w.Body.String())
			}
			if w = login(""); w.Code != 200 || len(w.Result().Cookies()) != 1 {
				t.Fatal("password login after disable", w.Code, w.Body.String())
			}
			if n, _ := s.DB.RecoveryCount(owner); n != 0 {
				t.Fatal("recovery survived disable")
			}
		})
	}
}

func TestTwoFactorExpiryBearerCSRFAndRateLimit(t *testing.T) {
	s := interactionServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	s.DB.EnsureAdmin("admin", string(hash))
	factorSession(t, s, dbx.AdminOwner, "current")
	w := factorCall(s, "/api/auth/2fa/setup", "current", map[string]string{"password": "password"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s.DB.Exec(`UPDATE account_two_factor SET expires_at=0`)
	w = factorCall(s, "/api/auth/2fa/enable", "current", map[string]string{"password": "password", "code": "123456"})
	if w.Code != 409 {
		t.Fatal("expired setup", w.Code)
	}
	_, codes := enrollFactor(t, s, "current", "password")
	h := sha256.Sum256([]byte("token"))
	s.DB.InsertToken(&dbx.APIToken{ID: "factor-token", Name: "factor", Scope: "admin", OwnerID: dbx.AdminOwner, TokenHash: hex.EncodeToString(h[:])})
	w = uploaderCall(s, "POST", "/api/auth/2fa/disable", "token", map[string]string{"password": "password", "code": codes[0]})
	if w.Code != 403 {
		t.Fatal("bearer changed factor", w.Code)
	}
	r := httptest.NewRequest("POST", "/api/auth/2fa/disable", strings.NewReader(`{"password":"password"}`))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "current"})
	r.Header.Set("Origin", "https://foreign.example")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin factor change", w.Code)
	}
	for i := 0; i < 5; i++ {
		w = factorCall(s, "/api/auth/login", "", map[string]string{"username": "admin", "password": "password", "code": "invalid"})
		if w.Code != 400 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	w = factorCall(s, "/api/auth/login", "", map[string]string{"username": "admin", "password": "password", "code": codes[0]})
	if w.Code != 429 {
		t.Fatal("missing factor throttling", w.Code)
	}
	// Correct-password first steps must not clear the factor account's budget.
	w = factorCall(s, "/api/auth/2fa/disable", "current", map[string]string{"password": "password", "code": codes[0]})
	if w.Code != 429 {
		t.Fatal("management bypassed throttling", w.Code)
	}
}

func TestTwoFactorConcurrentAndPersistentReplayProtection(t *testing.T) {
	s := interactionServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	s.DB.EnsureAdmin("admin", string(hash))
	factorSession(t, s, dbx.AdminOwner, "current")
	secret, codes := enrollFactor(t, s, "current", "password")
	f, _ := s.DB.TwoFactor(dbx.AdminOwner)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if s.DB.ConsumeRecovery(dbx.AdminOwner, recoveryHash(dbx.AdminOwner, codes[0])) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("concurrent recovery", accepted.Load())
	}
	accepted.Store(0)
	step := time.Now().Unix()/30 + 1
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if s.DB.ConsumeTwoFactorStep(dbx.AdminOwner, f.Nonce, step) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("concurrent TOTP", accepted.Load())
	}
	// A new Server (and new in-memory guard) still rejects persisted used codes.
	fresh, err := New(s.Cfg, s.DB, s.Svc, s.Eng, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.verifyFactor(dbx.AdminOwner, codes[0], f) || fresh.verifyFactor(dbx.AdminOwner, totp.Code(secret, step), f) {
		t.Fatal("server restart lost replay state")
	}
}
