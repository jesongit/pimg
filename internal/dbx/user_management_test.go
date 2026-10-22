package dbx

import (
	"path/filepath"
	"testing"
)

func TestUserManagementPreservesIdentityAndRevokesCredentials(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.EnsureAdmin("admin", "admin-hash")
	if err = db.CreateAccount("user:alpha", "alpha", "old-hash"); err != nil {
		t.Fatal(err)
	}
	db.CreateAccount("user:bravo", "bravo", "bravo-hash")
	album, err := db.EnsureOwnerDefaultAlbum("user:alpha")
	if err != nil {
		t.Fatal(err)
	}
	db.InsertOwnerSession("user:alpha", "alpha-session", 9999999999, "", "")
	db.InsertOwnerSession("user:bravo", "bravo-session", 9999999999, "", "")
	db.InsertToken(&APIToken{ID: "alpha-token", Name: "alpha", TokenHash: "alpha-token", Scope: "account", OwnerID: "user:alpha"})
	name := "renamed"
	if err = db.UpdateManagedAccount("user:alpha", &name, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.AccountByUsername(name); got.ID != "user:alpha" {
		t.Fatal("owner changed", got)
	}
	if got, _ := db.EnsureOwnerDefaultAlbum("user:alpha"); got != album {
		t.Fatal("default album changed")
	}
	if _, err = db.GetSession("alpha-session"); err != nil {
		t.Fatal("username rename logged user out", err)
	}
	if err = db.UpdateManagedAccount("user:alpha", nil, nil, "new-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetSession("alpha-session"); err != ErrNotFound {
		t.Fatal("password reset kept session", err)
	}
	if _, err = db.GetTokenByHash("alpha-token"); err != ErrNotFound {
		t.Fatal("password reset kept token", err)
	}
	if _, err = db.GetSession("bravo-session"); err != nil {
		t.Fatal("other user's session changed", err)
	}
	disable := true
	if err = db.UpdateManagedAccount("user:alpha", nil, &disable, ""); err != nil || db.AccountActive("user:alpha") {
		t.Fatal(err)
	}
	items, err := db.ManagedAccounts()
	if err != nil || len(items) != 3 {
		t.Fatal("disabled user hidden", items, err)
	}
	if err = db.UpdateManagedAccount(AdminOwner, nil, &disable, ""); err == nil {
		t.Fatal("administrator disabled")
	}
	if _, _, err = db.GetAdmin(); err != nil {
		t.Fatal(err)
	}
	name = "admin"
	if err = db.UpdateManagedAccount("user:alpha", &name, nil, ""); err == nil {
		t.Fatal("admin name collision accepted")
	}
}
