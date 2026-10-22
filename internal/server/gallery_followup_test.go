package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestModernConversionHTTPAndAdminLogNewLocation(t *testing.T) {
	s := interactionServer(t)
	cookie := adminCookie(t, s, "modern-conversion-http")
	response := adminCall(s, cookie, "GET", "/api/admin/storage", "")
	var configuration struct {
		Instances []map[string]any `json:"instances"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &configuration) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, instance := range configuration.Instances {
		if instance["id"] == "local" {
			instance["config"].(map[string]any)["encrypt"] = true
			body, _ := json.Marshal(instance)
			r := adminCall(s, cookie, "PATCH", "/api/admin/storage/instances/local", string(body))
			if r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
		}
	}
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "original.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/admin/files/" + f.ID + "/convert"
	if r := adminCall(s, cookie, "POST", path, `{"encrypted":true}`); r.Code != 400 {
		t.Fatal("confirmation bypass", r.Code, r.Body.String())
	}
	r := adminCall(s, cookie, "POST", path, `{"encrypted":true,"confirm":"CONVERT"}`)
	if r.Code != 202 {
		t.Fatal(r.Code, r.Body.String())
	}
	c, err := s.DB.GetConversion(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.Svc.RunConversion(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = adminCall(s, cookie, "GET", fmt.Sprintf("/api/admin/logs/tasks/%d/files", c.TaskID), "")
	var files struct{ Items []dbx.TaskOperationItem }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &files) != nil || len(files.Items) != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	item := files.Items[0]
	if item.FileID != f.ID || item.FileExists || item.FileSuccessor == nil || item.FileSuccessor.ID != target || item.FileSuccessor.Kind != "converted" {
		t.Fatal("audit identity or current location lost", r.Body.String())
	}
	for _, state := range []string{"done", "issues", "canceled"} {
		if r = adminCall(s, cookie, "GET", "/api/admin/logs?record=all&state="+state, ""); r.Code != 200 {
			t.Fatal(state, r.Code, r.Body.String())
		}
	}
	if r = adminCall(s, cookie, "GET", "/api/admin/logs?record=all&state=invalid", ""); r.Code != 400 {
		t.Fatal("invalid state accepted", r.Code)
	}
	if strings.Contains(r.Body.String(), target) {
		t.Fatal("error exposed unrelated target")
	}
}

func TestAccountOperationDoesNotExposeCrossOwnerSuccessor(t *testing.T) {
	s := interactionServer(t)
	for _, owner := range []string{"user:original", "user:recipient"} {
		if err := s.DB.CreateAccount(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := dbx.WithOwner(context.Background(), "user:original")
	f, err := s.Svc.ReceiveUpload(ctx, bytes.NewReader(interactionPNG(t)), "private.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	op, err := s.DB.BeginTaskOperation(f.OwnerID, "upload", "original-operation", "album:0;encrypted:false", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.RecordTaskOperationItem(op.ID, f.OwnerID, f.ID, f.ID, f.Name, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SealTaskOperation(op.ID, f.OwnerID); err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.InsertOwnerAlbum("user:recipient", "Private destination")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Svc.QueueOwnerTransfer(context.Background(), f.ID, album, 0); err != nil {
		t.Fatal(err)
	}
	target, err := s.Svc.RunOwnerTransfer(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := uploaderCall(s, "GET", fmt.Sprintf("/api/account/operations/%d/items", op.ID), f.OwnerID, nil)
	if r.Code != 200 || strings.Contains(r.Body.String(), "file_successor") || strings.Contains(r.Body.String(), target) || strings.Contains(r.Body.String(), "user:recipient") {
		t.Fatal("private destination leaked to former owner", r.Code, r.Body.String())
	}
}
