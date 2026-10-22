package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"pimg/internal/dbx"
)

func TestAccountDeleteRequiresAdminConfirmationAndRevokesAccess(t *testing.T) {
	s := interactionServer(t)
	if err := s.DB.CreateAccount("user:delete-api", "delete-api", "hash"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []struct{ value, scope, owner string }{{"delete-admin", "admin", dbx.AdminOwner}, {"delete-user", "account", "user:delete-api"}} {
		sum := sha256.Sum256([]byte(token.value))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: token.value, Name: token.value, TokenHash: hex.EncodeToString(sum[:]), Scope: token.scope, OwnerID: token.owner}); err != nil {
			t.Fatal(err)
		}
	}
	route := "/api/admin/accounts/user:delete-api"
	for _, token := range []string{"", "delete-user"} {
		if w := uploaderCall(s, "DELETE", route, token, map[string]string{"confirm": "DELETE"}); w.Code != 401 {
			t.Fatal("unauthorized delete", w.Code)
		}
	}
	if w := uploaderCall(s, "DELETE", route, "delete-admin", map[string]string{}); w.Code != 400 {
		t.Fatal("missing confirmation accepted", w.Code)
	}
	if w := uploaderCall(s, "DELETE", "/api/admin/accounts/admin:1", "delete-admin", map[string]string{"confirm": "DELETE"}); w.Code != 400 {
		t.Fatal("administrator deletion accepted", w.Code)
	}
	w := uploaderCall(s, "DELETE", route, "delete-admin", map[string]string{"confirm": "DELETE"})
	var response struct {
		TaskID int64 `json:"task_id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.TaskID == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := uploaderCall(s, "GET", "/api/account/me", "delete-user", nil); w.Code != 401 {
		t.Fatal("old token still authorized", w.Code)
	}
	if w := uploaderCall(s, "PATCH", route, "delete-admin", map[string]bool{"disabled": false}); w.Code != 400 {
		t.Fatal("deleting user can be enabled", w.Code)
	}
	if err := s.Svc.RunAccountDelete(t.Context(), "user:delete-api"); err != nil {
		t.Fatal(err)
	}
}
