package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"pimg/internal/dbx"
)

func TestUserManagementAdminRoutesAndImmediateSessionRevocation(t *testing.T) {
	s := interactionServer(t)
	s.DB.EnsureAdmin("admin", "admin-hash")
	for _, v := range []struct{ token, scope, owner string }{{"admin-token", "admin", dbx.AdminOwner}, {"alpha-token", "account", "user:alpha"}} {
		sum := sha256.Sum256([]byte(v.token))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: v.token, Name: v.token, TokenHash: hex.EncodeToString(sum[:]), Scope: v.scope, OwnerID: v.owner}); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	s.DB.CreateAccount("user:alpha", "alpha", string(hash))
	if w := uploaderCall(s, "PATCH", "/api/admin/accounts/user:alpha", "alpha-token", map[string]any{"disabled": true}); w.Code != 401 {
		t.Fatal("ordinary user changed access", w.Code)
	}
	if w := uploaderCall(s, "PATCH", "/api/admin/accounts/user:alpha", "admin-token", map[string]any{"username": "renamed"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	list := uploaderCall(s, "GET", "/api/admin/accounts", "admin-token", nil)
	var payload struct {
		Items []dbx.ManagedAccount `json:"items"`
	}
	if json.Unmarshal(list.Body.Bytes(), &payload) != nil || len(payload.Items) != 2 || strings.Contains(list.Body.String(), "password_hash") {
		t.Fatal("unsafe user list", list.Body.String())
	}
	if w := uploaderCall(s, "POST", "/api/admin/accounts/user:alpha/password", "admin-token", map[string]any{"password": "new-password"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := uploaderCall(s, "GET", "/api/account/me", "alpha-token", nil); w.Code != 401 {
		t.Fatal("reset kept old account token", w.Code)
	}
	if w := uploaderCall(s, "POST", "/api/auth/login", "", map[string]any{"username": "renamed", "password": "new-password"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := uploaderCall(s, "PATCH", "/api/admin/accounts/user:alpha", "admin-token", map[string]any{"disabled": true}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := uploaderCall(s, "POST", "/api/auth/login", "", map[string]any{"username": "renamed", "password": "new-password"}); w.Code != 401 {
		t.Fatal("disabled user logged in", w.Code)
	}
	list = uploaderCall(s, "GET", "/api/admin/accounts", "admin-token", nil)
	if !strings.Contains(list.Body.String(), `"disabled":true`) {
		t.Fatal("disabled account hidden")
	}
	if w := uploaderCall(s, "PATCH", "/api/admin/accounts/admin:1", "admin-token", map[string]any{"disabled": true}); w.Code != 400 {
		t.Fatal("admin disabled", w.Code)
	}
}
