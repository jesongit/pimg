package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestAdminUploadOperationBelongsToTargetAlbumOwner(t *testing.T) {
	s := importServer(t)
	albumImportAccounts(t, s)
	call := carrierRequest(t, s)
	album, err := s.DB.InsertOwnerAlbum("user:alpha", "target")
	if err != nil {
		t.Fatal(err)
	}
	w := call("POST", "/api/admin/operations", strings.NewReader(fmt.Sprintf(`{"kind":"upload","key":"admin-on-behalf","total_files":1,"album_id":%d}`, album)), "application/json")
	var op struct {
		ID int64 `json:"operation_id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &op) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	var owner string
	s.DB.QueryRow(`SELECT owner_id FROM task_operations WHERE id=?`, op.ID).Scan(&owner)
	if owner != "user:alpha" {
		t.Fatal("operation owner", owner)
	}
	var data bytes.Buffer
	mw := multipart.NewWriter(&data)
	part, _ := mw.CreateFormFile("files", "owned.png")
	part.Write(interactionPNG(t))
	mw.Close()
	w = call("POST", fmt.Sprintf("/api/admin/upload?album_id=%d&operation_id=%d&item_key=one&encrypt=false", album, op.ID), &data, mw.FormDataContentType())
	var res struct {
		Files []struct {
			ID string `json:"id"`
		}
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &res) != nil || len(res.Files) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	f, err := s.DB.OwnedFile("user:alpha", res.Files[0].ID)
	if err != nil || f.AlbumID.Int64 != album {
		t.Fatal(f, err)
	}
	if _, err = s.DB.OwnedFile(dbx.AdminOwner, f.ID); err == nil {
		t.Fatal("uploaded file assigned to admin")
	}
	if w = call("POST", fmt.Sprintf("/api/admin/operations/%d/seal", op.ID), strings.NewReader("{}"), "application/json"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = uploaderCall(s, "POST", "/api/account/operations", "user:bravo", map[string]any{"kind": "upload", "key": "cross-owner", "total_files": 1, "album_id": album}); w.Code < 400 {
		t.Fatal("ordinary user chose foreign album", w.Code, w.Body.String())
	}
	disabled := true
	if err = s.DB.UpdateManagedAccount("user:alpha", nil, &disabled, ""); err != nil {
		t.Fatal(err)
	}
	if w = call("POST", "/api/admin/operations", strings.NewReader(fmt.Sprintf(`{"kind":"upload","key":"disabled-owner","total_files":1,"album_id":%d}`, album)), "application/json"); w.Code != 400 {
		t.Fatal("disabled owner accepted", w.Code, w.Body.String())
	}
}
