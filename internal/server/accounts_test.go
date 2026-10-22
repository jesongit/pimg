package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"strings"
	"testing"
	"time"
)

func TestAccountManualRetryHonorsTelegramFloodWait(t *testing.T) {
	s := interactionServer(t)
	owner := "user:alpha"
	if err := s.DB.CreateAccount(owner, "alpha", "hash"); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(2 * time.Hour).Unix()
	if _, err := s.DB.Exec(`INSERT INTO account_telegram(owner_id,account_id,create_token,state,last_error,retry_after) VALUES(?,11,'token','failed','TG FLOOD_WAIT',?)`, owner, after); err != nil {
		t.Fatal(err)
	}
	id, err := s.DB.EnqueueUniqueTask(store.TaskAccountSpace, "tg-space:"+owner, dbx.JSONValue(map[string]string{"owner_id": owner}))
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, id)
	req := httptest.NewRequest("POST", "/retry", strings.NewReader("{}"))
	req.SetPathValue("owner", owner)
	response := httptest.NewRecorder()
	s.handleAccountRetry(response, req)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	space, err := s.DB.TelegramSpace(owner)
	if err != nil || space.RetryAfter != after {
		t.Fatal("manual retry cleared provider cooldown", space, err)
	}
	task, err := s.DB.GetTask(id)
	if err != nil || task.RunAfter < after {
		t.Fatal("task scheduled before provider cooldown", task, err)
	}
}

func TestAccountHTTPIsolationAndAdministratorBoundary(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
	alpha, _ := s.DB.EnsureOwnerDefaultAlbum("user:alpha")
	bravo, _ := s.DB.EnsureOwnerDefaultAlbum("user:bravo")
	for _, v := range []struct {
		id, owner string
		album     int64
	}{{"alpha-file", "user:alpha", alpha}, {"bravo-file", "user:bravo", bravo}} {
		if err := s.DB.InsertFile(&dbx.File{ID: v.id, OwnerID: v.owner, Name: v.id, OrigName: v.id + ".png", Kind: "image", Size: 4, SHA256: "hash", AlbumID: sql.NullInt64{Int64: v.album, Valid: true}, KeyWrapped: []byte{}, KeyNonce: []byte{}, ArchiveState: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	own := uploaderCall(s, "GET", "/api/account/files", "user:alpha", nil)
	if own.Code != 200 || !strings.Contains(own.Body.String(), "alpha-file") || strings.Contains(own.Body.String(), "bravo-file") {
		t.Fatal(own.Code, own.Body.String())
	}
	for _, path := range []string{"/api/account/files/bravo-file", "/api/account/files/bravo-file/content?kind=original", "/api/account/files/bravo-file/download", "/api/account/files/bravo-file/links"} {
		w := uploaderCall(s, "GET", path, "user:alpha", nil)
		if w.Code != 404 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, action := range []struct{ method, path string }{{"DELETE", "/api/account/files/bravo-file"}, {"POST", "/api/account/files/bravo-file/publish"}, {"POST", "/api/account/files/bravo-file/temp"}, {"GET", "/api/admin/files"}, {"GET", "/api/admin/storage"}, {"GET", "/api/admin/telegram"}, {"POST", "/api/admin/accounts"}} {
		w := uploaderCall(s, action.method, action.path, "user:alpha", nil)
		if w.Code != 404 && w.Code != 401 {
			t.Fatal(action, w.Code)
		}
	}
	moved := uploaderCall(s, "PATCH", "/api/account/files/alpha-file", "user:alpha", map[string]any{"album_id": bravo})
	if moved.Code != 404 {
		t.Fatal(moved.Code, moved.Body.String())
	}
	albums := uploaderCall(s, "GET", "/api/account/albums", "user:alpha", nil)
	var payload struct {
		Items []struct {
			ID int64 `json:"id"`
		}
	}
	if json.Unmarshal(albums.Body.Bytes(), &payload) != nil || len(payload.Items) != 1 || payload.Items[0].ID != alpha {
		t.Fatal(albums.Body.String())
	}
	// Existing cookies are admin-owned; a regular cookie can never become admin.
	if err := s.DB.InsertOwnerSession("user:alpha", "cookie-hash", 9999999999, "", ""); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("user-cookie"))
	s.DB.Exec(`UPDATE sessions SET token_hash=? WHERE token_hash='cookie-hash'`, hex.EncodeToString(sum[:]))
	req := httptest.NewRequest("GET", "/api/admin/settings", nil)
	req.Header.Set("Cookie", sessionCookie+"=user-cookie")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("regular session reached admin", w.Code)
	}
	s.DB.Exec(`UPDATE accounts SET disabled=1 WHERE id='user:alpha'`)
	disabled := uploaderCall(s, "GET", "/api/account/files", "user:alpha", nil)
	if disabled.Code != 401 {
		t.Fatal("disabled account retained access", disabled.Code)
	}
	if _, err := s.DB.GetFile("bravo-file"); err != nil {
		t.Fatal("foreign file changed", err)
	}
}
