package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

func TestMigrationAdminBoundaryAndExistingDataProtection(t *testing.T) {
	s := interactionServer(t)
	if e := s.DB.EnsureAdminWithSetup("owner", adminPasswordHash(t, "long-password"), false); e != nil {
		t.Fatal(e)
	}
	cookie := adminCookie(t, s, "migration-admin-test")
	for _, p := range []string{"/api/admin/migration", "/api/admin/migration/export/" + strings.Repeat("a", 48)} {
		if w := adminCall(s, nil, http.MethodGet, p, ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", p, w.Code)
		}
	}
	if w := adminCall(s, cookie, http.MethodGet, "/api/admin/migration", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"can_restore":true`) {
		t.Fatal(w.Code, w.Body)
	}
	if e := s.DB.CreateAccount("user:restore-test", "existing", "password-hash"); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"/api/admin/migration/import", "/api/admin/migration/apply"} {
		w := adminCall(s, cookie, http.MethodPost, p, `{"id":"bad"}`)
		if w.Code != 409 {
			t.Fatal(p, w.Code, w.Body)
		}
	}
	if !s.DB.AccountActive("user:restore-test") {
		t.Fatal("existing account changed")
	}
	if e := s.DB.InsertOwnerSession("user:restore-test", "ordinary-migration-session", 9999999999, "test", ""); e != nil {
		t.Fatal(e)
	}
	// A normal account cookie must never authorize migration or expose secrets.
	userCookie := adminCookie(t, s, "unused-admin-cookie")
	userCookie.Value = "ordinary"
	digest := sha256.Sum256([]byte("ordinary"))
	sum := hex.EncodeToString(digest[:])
	if e := s.DB.InsertOwnerSession("user:restore-test", sum, 9999999999, "test", ""); e != nil {
		t.Fatal(e)
	}
	if w := adminCall(s, userCookie, http.MethodGet, "/api/admin/migration", ""); w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
}
