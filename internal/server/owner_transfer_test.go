package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"pimg/internal/dbx"
)

func TestAdminOwnerTransferHTTPAuthenticationConfirmationAndCutover(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{dbx.AdminOwner, "user:source", "user:target"} {
		if owner != dbx.AdminOwner {
			if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
				t.Fatal(err)
			}
		}
		scope := "account"
		if owner == dbx.AdminOwner {
			scope = "admin"
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, OwnerID: owner, Scope: scope, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.DB.InsertOwnerAlbum("user:target", "Target album")
	if err != nil {
		t.Fatal(err)
	}
	ctx := dbx.WithOwner(context.Background(), "user:source")
	f, err := s.Svc.ReceiveUpload(ctx, bytes.NewReader(interactionPNG(t)), "source.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/admin/files/" + f.ID + "/transfer"
	body := map[string]any{"album_id": a, "confirm": "TRANSFER_OWNER"}
	for _, attempt := range []struct {
		path, token string
		body        any
		want        int
	}{
		{path, "user:source", body, 401},
		{"/api/account/files/" + f.ID + "/transfer", "user:source", body, 404},
		{path, dbx.AdminOwner, map[string]any{"album_id": a}, 400},
		{"/api/account/batch?owner_id=admin:1", "user:source", map[string]any{"action": "move_album", "album_id": a, "confirm": "TRANSFER_OWNER", "ids": []string{f.ID}}, 404},
	} {
		r := uploaderCall(s, "POST", attempt.path, attempt.token, attempt.body)
		if r.Code != attempt.want {
			t.Fatal(attempt.path, r.Code, r.Body.String())
		}
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM file_conversions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected requests staged a transfer", count, err)
	}
	r := uploaderCall(s, "GET", "/api/admin/albums?owner_id=all", dbx.AdminOwner, nil)
	var albums struct {
		Items []struct {
			ID      int64
			OwnerID string `json:"owner_id"`
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &albums) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	found := false
	for _, album := range albums.Items {
		found = found || album.ID == a && album.OwnerID == "user:target"
	}
	if !found {
		t.Fatal("admin all-album list omitted target account")
	}
	r = uploaderCall(s, "POST", path, dbx.AdminOwner, body)
	var accepted struct {
		TaskID   int64  `json:"task_id"`
		TargetID string `json:"target_file_id"`
	}
	if r.Code != 202 || json.Unmarshal(r.Body.Bytes(), &accepted) != nil || accepted.TaskID == 0 || accepted.TargetID == "" {
		t.Fatal(r.Code, r.Body.String())
	}
	r = uploaderCall(s, "GET", "/api/account/files/"+accepted.TargetID, "user:target", nil)
	if r.Code != 404 {
		t.Fatal("unverified target accessible", r.Code, r.Body.String())
	}
	r = uploaderCall(s, "POST", path, dbx.AdminOwner, body)
	if r.Code != 202 {
		t.Fatal("retry did not reuse transfer", r.Code, r.Body.String())
	}
	if _, err = s.Svc.RunOwnerTransfer(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		id, owner string
		code      int
	}{{f.ID, "user:source", 404}, {accepted.TargetID, "user:source", 404}, {accepted.TargetID, "user:target", 200}} {
		r = uploaderCall(s, "GET", "/api/account/files/"+entry.id, entry.owner, nil)
		if r.Code != entry.code {
			t.Fatal("ownership not enforced after cutover", entry, r.Code, r.Body.String())
		}
	}
	r = uploaderCall(s, "GET", "/api/admin/files/"+f.ID, dbx.AdminOwner, nil)
	var receipt struct {
		Conversion struct {
			Kind        string
			TargetOwner string `json:"target_owner_id"`
			Phase       string
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &receipt) != nil || receipt.Conversion.Kind != "transfer_owner" || receipt.Conversion.Phase != "done" || receipt.Conversion.TargetOwner != "user:target" {
		t.Fatal("missing admin transfer receipt", r.Code, r.Body.String())
	}
}

func TestAdminBatchOwnerTransferUsesExplicitOperationAndConfirmation(t *testing.T) {
	s := interactionServer(t)
	if err := s.DB.CreateAccount("user:recipient", "Recipient", "hash"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("administrator"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "admin", Name: "admin", OwnerID: dbx.AdminOwner, Scope: "admin", TokenHash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.InsertOwnerAlbum("user:recipient", "Target")
	if err != nil {
		t.Fatal(err)
	}
	seedInteractionFile(t, s, "batch-transfer", 1709251199, sql.NullInt64{})
	operation, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "move_album", "batch-transfer", "album:"+fmt.Sprint(album), 1)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"action": "move_album", "ids": []string{"batch-transfer"}, "album_id": album, "operation_id": operation.ID}
	r := uploaderCall(s, "POST", "/api/admin/batch", "administrator", body)
	var rejected struct{ Done, Skipped int }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &rejected) != nil || rejected.Done != 0 || rejected.Skipped != 1 {
		t.Fatal("missing confirmation accepted", r.Code, r.Body.String())
	}
	// A failed item can be resubmitted within the same still-open operation.
	body["confirm"] = "TRANSFER_OWNER"
	r = uploaderCall(s, "POST", "/api/admin/batch", "administrator", body)
	var result struct {
		Done, Skipped int
		Results       []struct {
			Status string
			TaskID int64 `json:"task_id"`
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Done != 1 || result.Skipped != 0 || result.Results[0].Status != "submitted" || result.Results[0].TaskID == 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	var linked int64
	if err = s.DB.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, result.Results[0].TaskID).Scan(&linked); err != nil || linked != operation.ID {
		t.Fatal("batch transfer escaped operation", linked, err)
	}
}
