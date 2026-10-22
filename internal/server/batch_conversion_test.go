package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"pimg/internal/dbx"
)

func conversionTestStorage(t *testing.T, s *Server) {
	t.Helper()
	for _, instance := range s.Svc.StorageInstances() {
		if instance.ID == "local" {
			cfg := instance.Config
			cfg.Encrypt = true
			if _, err := s.Svc.SaveStorageInstance(instance.ID, instance.Name, instance.Driver, cfg, true); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBatchConversionChecksSkipNoopsAndRoundTrip(t *testing.T) {
	s := interactionServer(t)
	conversionTestStorage(t, s)
	cookie := adminCookie(t, s, "batch-conversion")
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		return adminCall(s, cookie, method, path, string(encoded))
	}
	files := []*dbx.File{}
	for _, direct := range []bool{true, false} {
		f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "original.png", "png", sql.NullInt64{}, nil, 0, direct)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	seedInteractionFile(t, s, "not-ready", 1700000000, sql.NullInt64{})
	s.DB.SetArchiveState("not-ready", "pending", "")
	ids := []string{files[0].ID, files[1].ID, "not-ready"}
	counts := func() string {
		var tasks, files, conversions int
		s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM files),(SELECT COUNT(*) FROM file_conversions)`).Scan(&tasks, &files, &conversions)
		return fmt.Sprint(tasks, files, conversions)
	}
	before := counts()
	r := call("POST", "/api/admin/files/conversion-check", map[string]any{"ids": ids, "encrypted": true})
	var checked struct{ Items []struct{ ID, State string } }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &checked) != nil || len(checked.Items) != 3 || checked.Items[0].State != "ready" || checked.Items[1].State != "skipped" || checked.Items[2].State != "blocked" {
		t.Fatal("incorrect conversion decisions", r.Code, r.Body.String())
	}
	if counts() != before {
		t.Fatal("preview wrote conversion data")
	}
	for _, body := range []map[string]any{{"action": "convert", "ids": ids, "encrypted": true}, {"action": "convert", "ids": ids, "confirm": "CONVERT"}} {
		if r = call("POST", "/api/admin/batch", body); r.Code != 400 || counts() != before {
			t.Fatal("unconfirmed/unspecified conversion changed files", r.Code, r.Body.String())
		}
	}
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "convert", "encrypt-batch", dbx.ConversionScope(true), 3)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"action": "convert", "ids": ids, "encrypted": true, "confirm": "CONVERT", "operation_id": op.ID}
	r = call("POST", "/api/admin/batch", body)
	var submitted struct {
		Results []struct{ ID, Status, Reason string }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &submitted) != nil || submitted.Results[0].Status != "submitted" || submitted.Results[1].Status != "skipped" || submitted.Results[2].Status != "failed" {
		t.Fatal("batch must independently check each input", r.Code, r.Body.String())
	}
	c, err := s.DB.GetConversion(files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	after := counts()
	r = call("POST", "/api/admin/batch", body)
	if r.Code != 200 || counts() != after {
		t.Fatal("replayed chunk duplicated conversion", r.Code, r.Body.String())
	}
	other, _ := s.DB.BeginTaskOperation(dbx.AdminOwner, "convert", "other-batch", dbx.ConversionScope(true), 1)
	body["operation_id"] = other.ID
	body["ids"] = ids[:1]
	r = call("POST", "/api/admin/batch", body)
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"status":"failed"`)) {
		t.Fatal("another operation absorbed existing conversion", r.Code, r.Body.String())
	}
	current, _ := s.DB.GetConversion(files[0].ID)
	if current.TaskID != c.TaskID || current.TargetID != c.TargetID {
		t.Fatal("conversion identity changed")
	}
	if err = s.DB.SealTaskOperation(op.ID, dbx.AdminOwner); err != nil {
		t.Fatal(err)
	}
	target, err := s.Svc.RunConversion(context.Background(), files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunConvertedCleanup(context.Background(), files[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, original := range []string{target, files[1].ID} {
		r = call("POST", "/api/admin/batch", map[string]any{"action": "convert", "ids": []string{original}, "encrypted": false, "confirm": "CONVERT"})
		if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"status":"submitted"`)) {
			t.Fatal("decryption batch unavailable", r.Code, r.Body.String())
		}
		plainID, err := s.Svc.RunConversion(context.Background(), original)
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := s.DB.GetVisibleFile(plainID)
		if plain.Storage != "direct" || plain.SHA256 != files[0].SHA256 || plain.Published {
			t.Fatal("decryption changed original or publication", plain)
		}
		for _, replica := range mustConversionReplicas(t, s, plainID) {
			if replica.Encrypted {
				t.Fatal("decryption left an encrypted replica")
			}
		}
		if err := s.Svc.CheckConversion(plainID, false); err != dbx.ErrConversionNotNeeded {
			t.Fatal("already-plaintext file needs conversion", err)
		}
	}
}

func mustConversionReplicas(t *testing.T, s *Server, id string) []dbx.MediaReplica {
	t.Helper()
	r, err := s.DB.MediaReplicas(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBatchConversionRechecksAfterPreviewAndRejectsClosedIntake(t *testing.T) {
	s := interactionServer(t)
	conversionTestStorage(t, s)
	cookie := adminCookie(t, s, "recheck-conversion")
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "plain.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.CheckConversion(f.ID, true); err != nil {
		t.Fatal(err)
	}
	// A setting can change after preview. Submission must check the new state.
	for _, instance := range s.Svc.StorageInstances() {
		if instance.ID == "local" {
			cfg := instance.Config
			cfg.Encrypt = false
			if _, err = s.Svc.SaveStorageInstance(instance.ID, instance.Name, instance.Driver, cfg, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	body := map[string]any{"action": "convert", "ids": []string{f.ID}, "encrypted": true, "confirm": "CONVERT"}
	encoded, _ := json.Marshal(body)
	r := adminCall(s, cookie, "POST", "/api/admin/batch", string(encoded))
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM file_conversions`).Scan(&n)
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"status":"submitted"`)) || n != 1 {
		t.Fatal("current plaintext policy must still permit the file setting", r.Code, r.Body.String(), n)
	}
	target, err := s.Svc.RunConversion(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetVisibleFile(target)
	if err != nil || f.Storage == "direct" {
		t.Fatal("conversion lost requested encryption", f, err)
	}
	for _, replica := range mustConversionReplicas(t, s, target) {
		if replica.Encrypted {
			t.Fatal("submission failed to capture changed plaintext storage policy")
		}
	}
	body["ids"], body["encrypted"] = []string{target}, false
	op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "convert", "sealed-conversion", dbx.ConversionScope(false), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SealTaskOperation(op.ID, dbx.AdminOwner); err != nil {
		t.Fatal(err)
	}
	body["operation_id"] = op.ID
	encoded, _ = json.Marshal(body)
	r = adminCall(s, cookie, "POST", "/api/admin/batch", string(encoded))
	s.DB.QueryRow(`SELECT COUNT(*) FROM file_conversions`).Scan(&n)
	if r.Code != 409 || n != 1 {
		t.Fatal("closed operation accepted conversion", r.Code, r.Body.String())
	}
	if _, err = s.Svc.QueueConversion(context.Background(), f.ID, false, op.ID); err == nil {
		t.Fatal("service bypassed transaction boundary")
	}
	s.DB.QueryRow(`SELECT COUNT(*) FROM file_conversions`).Scan(&n)
	if n != 1 {
		t.Fatal("rejected transaction left replacement state")
	}
}

func TestAccountBatchConversionScopeAndPreviewIsolation(t *testing.T) {
	s := interactionServer(t)
	conversionTestStorage(t, s)
	owner := "user:batch-owner"
	s.DB.CreateAccount(owner, owner, "hash")
	sum := sha256.Sum256([]byte(owner))
	s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])})
	f, err := s.Svc.ReceiveUpload(dbx.WithOwner(context.Background(), owner), bytes.NewReader(interactionPNG(t)), "private.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	seedInteractionFile(t, s, "admin-private", 1700000000, sql.NullInt64{})
	for _, path := range []string{"/api/account/files/conversion-check", "/api/account/batch"} {
		r := uploaderCall(s, "POST", path, owner, map[string]any{"action": "convert", "ids": []string{f.ID, "admin-private"}, "encrypted": true, "confirm": "CONVERT"})
		if r.Code != 404 {
			t.Fatal("account accessed another owner's conversion", path, r.Code, r.Body.String())
		}
	}
	op, err := s.DB.BeginTaskOperation(owner, "convert", "frozen-false", dbx.ConversionScope(false), 1)
	if err != nil {
		t.Fatal(err)
	}
	r := uploaderCall(s, "POST", "/api/account/batch", owner, map[string]any{"action": "convert", "ids": []string{f.ID}, "encrypted": true, "confirm": "CONVERT", "operation_id": op.ID})
	if r.Code != 409 {
		t.Fatal("conversion target changed after operation creation", r.Code, r.Body.String())
	}
	r = uploaderCall(s, "POST", "/api/account/batch", owner, map[string]any{"action": "convert", "ids": []string{f.ID}, "encrypted": true, "confirm": "CONVERT"})
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"status":"submitted"`)) {
		t.Fatal("account cannot convert own files", r.Code, r.Body.String())
	}
}
