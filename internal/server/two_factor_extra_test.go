package server

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"pimg/internal/totp"
)

func TestTwoFactorTOTPLoginAndAccountIsolation(t *testing.T) {
	s := interactionServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	s.DB.EnsureAdmin("admin", string(hash))
	s.DB.CreateAccount("user:alpha", "alpha", string(hash))
	s.DB.CreateAccount("user:bravo", "bravo", string(hash))
	factorSession(t, s, "user:alpha", "alpha-current")
	factorSession(t, s, "user:bravo", "bravo-current")
	secret, _ := enrollFactor(t, s, "alpha-current", "password")
	// Simulate the next time step after enrollment without sleeping.
	s.DB.Exec(`UPDATE account_two_factor SET last_step=? WHERE owner_id='user:alpha'`, time.Now().Unix()/30-1)
	w := factorCall(s, "/api/auth/login", "", map[string]string{"username": "alpha", "password": "password", "code": totp.Code(secret, time.Now().Unix()/30)})
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal("TOTP login", w.Code, w.Body.String())
	}
	w = factorCall(s, "/api/auth/login", "", map[string]string{"username": "alpha", "password": "password", "code": totp.Code(secret, time.Now().Unix()/30)})
	if w.Code != 400 || len(w.Result().Cookies()) != 0 {
		t.Fatal("TOTP replay", w.Code)
	}
	w = factorCall(s, "/api/auth/login", "", map[string]string{"username": "bravo", "password": "password"})
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal("unrelated account requires factor", w.Code)
	}
	w = factorCall(s, "/api/auth/2fa", "bravo-current", nil)
	if w.Code != 200 || w.Body.String() != "{\"enabled\":false,\"recovery_remaining\":0}\n" {
		t.Fatal("cross-account factor state", w.Code, w.Body.String())
	}
	if _, err := s.DB.Exec(`DELETE FROM accounts WHERE id='user:alpha'`); err != nil {
		t.Fatal(err)
	}
	if f, err := s.DB.TwoFactor("user:alpha"); f != nil || err != nil {
		t.Fatal("deleted account factor survived", err)
	}
	if n, _ := s.DB.RecoveryCount("user:alpha"); n != 0 {
		t.Fatal("deleted account recovery survived")
	}
	h := sha256.Sum256([]byte("bravo-current"))
	if _, err := s.DB.GetSession(hex.EncodeToString(h[:])); err != nil {
		t.Fatal("unrelated session revoked", err)
	}
}
