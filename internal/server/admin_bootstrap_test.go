package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"pimg/internal/dbx"
)

func adminPasswordHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func adminCookie(t *testing.T, s *Server, value string) *http.Cookie {
	t.Helper()
	sum := sha256.Sum256([]byte(value))
	if err := s.DB.InsertOwnerSession(dbx.AdminOwner, hex.EncodeToString(sum[:]), time.Now().Add(time.Hour).Unix(), "test", ""); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: value}
}

func adminCall(s *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestDefaultAdminSetupGateAndCredentialOwnership(t *testing.T) {
	s := interactionServer(t)
	if err := s.DB.EnsureAdminWithSetup("admin", adminPasswordHash(t, "admin"), true); err != nil {
		t.Fatal(err)
	}
	login := adminCall(s, nil, "POST", "/api/auth/login", `{"username":"admin","password":"admin"}`)
	if login.Code != 200 || !strings.Contains(login.Body.String(), `"setup_required":true`) {
		t.Fatal(login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	other := adminCookie(t, s, "default-other-session")
	if err := s.DB.CreateAccount("user:normal", "normal", "hash"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("normal-session"))
	if err := s.DB.InsertOwnerSession("user:normal", hex.EncodeToString(sum[:]), time.Now().Add(time.Hour).Unix(), "test", ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ method, path, body string }{
		{"GET", "/api/admin/settings", ""}, {"GET", "/api/admin/accounts", ""}, {"GET", "/api/account/files", ""},
		{"POST", "/api/account/albums", `{"name":"blocked"}`}, {"POST", "/api/admin/tokens", `{"name":"blocked"}`},
	} {
		w := adminCall(s, cookie, v.method, v.path, v.body)
		if w.Code != 403 {
			t.Fatalf("setup bypass %s: %d %s", v.path, w.Code, w.Body.String())
		}
	}
	me := adminCall(s, cookie, "GET", "/api/auth/me", "")
	if me.Code != 200 || !strings.Contains(me.Body.String(), `"setup_required":true`) {
		t.Fatal(me.Code, me.Body.String())
	}
	// Even a pre-existing token cannot bypass the first-login restriction.
	for _, scope := range []string{"admin", "uploader"} {
		sum := sha256.Sum256([]byte(scope))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: scope, Name: scope, Scope: scope, OwnerID: dbx.AdminOwner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
		path := "/api/admin/settings"
		if scope == "uploader" {
			path = "/api/uploader/v1/config"
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+scope)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if scope == "admin" && w.Code != 403 || scope == "uploader" && w.Code != 404 {
			t.Fatal("token setup bypass", scope, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{
		`{"username":"renamed","old_password":"admin"}`,
		`{"username":"renamed","old_password":"wrong","new_password":"new-password"}`,
		`{"username":"normal","old_password":"admin","new_password":"new-password"}`,
		`{"username":"","old_password":"admin","new_password":"new-password"}`,
		`{"username":"renamed","old_password":"admin","new_password":"short"}`,
	} {
		if w := adminCall(s, cookie, "POST", "/api/auth/account", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	seedInteractionFile(t, s, "owned-before-rename", 10, sql.NullInt64{})
	w := adminCall(s, cookie, "POST", "/api/auth/account", `{"username":"renamed","old_password":"admin","new_password":"new-password"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if required, err := s.DB.AdminSetupRequired(); required || err != nil {
		t.Fatal(required, err)
	}
	if w := adminCall(s, cookie, "GET", "/api/admin/settings", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := adminCall(s, other, "GET", "/api/auth/me", ""); w.Code != 401 {
		t.Fatal("other default session survived", w.Code)
	}
	if w := adminCall(s, &http.Cookie{Name: sessionCookie, Value: "normal-session"}, "GET", "/api/auth/me", ""); w.Code != 200 {
		t.Fatal("ordinary session changed", w.Code)
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE owner_id=?`, dbx.AdminOwner).Scan(&count)
	if count != 0 {
		t.Fatal("admin tokens survived password change")
	}
	f, err := s.DB.GetFile("owned-before-rename")
	if err != nil || f.OwnerID != dbx.AdminOwner {
		t.Fatal("rename changed file owner", f, err)
	}
	if w := adminCall(s, nil, "POST", "/api/auth/login", `{"username":"admin","password":"admin"}`); w.Code != 401 {
		t.Fatal("old login survived")
	}
	if w := adminCall(s, nil, "POST", "/api/auth/login", `{"username":"renamed","password":"new-password"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAdminRenameKeepsPasswordSessionsAndRejectsCrossSite(t *testing.T) {
	s := interactionServer(t)
	hash := adminPasswordHash(t, "existing-password")
	s.DB.EnsureAdmin("existing", hash)
	cookie := adminCookie(t, s, "rename-session")
	other := adminCookie(t, s, "other-rename-session")
	body := `{"username":"新管理员","old_password":"existing-password"}`
	r := httptest.NewRequest("POST", "/api/auth/account", bytes.NewBufferString(body))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-site rename allowed", w.Code)
	}
	w = adminCall(s, cookie, "POST", "/api/auth/account", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	name, after, _ := s.DB.GetAdmin()
	if name != "新管理员" || after != hash {
		t.Fatal("name-only change altered password")
	}
	if w := adminCall(s, other, "GET", "/api/auth/me", ""); w.Code != 200 {
		t.Fatal("name-only change revoked session")
	}
	if w := adminCall(s, cookie, "POST", "/api/auth/password", `{"old_password":"existing-password","new_password":"legacy-endpoint-password"}`); w.Code != 200 {
		t.Fatal("legacy password endpoint", w.Code, w.Body.String())
	}
}

func TestAdminExistingBootstrapNeverResetsCredentials(t *testing.T) {
	s := interactionServer(t)
	s.DB.EnsureAdmin("custom-admin", "existing-hash")
	if err := s.DB.EnsureAdminWithSetup("admin", "default-hash", true); err != nil {
		t.Fatal(err)
	}
	name, hash, _ := s.DB.GetAdmin()
	required, err := s.DB.AdminSetupRequired()
	if name != "custom-admin" || hash != "existing-hash" || required || err != nil {
		t.Fatal("existing account reset", name, required, err)
	}
}
