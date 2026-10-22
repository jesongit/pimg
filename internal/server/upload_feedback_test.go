package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"testing"

	"pimg/internal/dbx"
)

func TestUploadResponseReportsDuplicateWithoutReplacingArchive(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	upload := func(name, suffix string) map[string]any {
		t.Helper()
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(interactionPNG(t)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r := request("POST", "/api/admin/upload"+suffix, &body, w.FormDataContentType())
		var result struct{ Files []map[string]any }
		if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &result) != nil || len(result.Files) != 1 {
			t.Fatal(r.Code, r.Body.String())
		}
		return result.Files[0]
	}
	first := upload("first.png", "")
	if first["duplicate"] != false || first["id"] == nil {
		t.Fatal("new upload reported as duplicate", first)
	}
	id := first["id"].(string)
	if err := s.Svc.RunArchive(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "?wait=1"} {
		repeated := upload("another-name.png", suffix)
		if repeated["duplicate"] != true || repeated["id"] != id || repeated["orig_name"] != "first.png" {
			t.Fatal("duplicate response must identify the retained file", repeated)
		}
	}
	f, err := s.DB.GetFile(id)
	if err != nil || f.UploadDuplicate {
		t.Fatal("duplicate response flag leaked into stored metadata", err)
	}
	if _, err := s.Svc.Verify(t.Context(), id); err != nil {
		t.Fatal("duplicate changed the encrypted original", err)
	}
	_, total, err := s.DB.ListFiles(dbx.FileFilter{})
	if err != nil || total != 1 {
		t.Fatal("duplicate created another file", total, err)
	}
	rejected := upload("unsupported.exe", "")
	if rejected["error"] == nil || rejected["id"] != nil {
		t.Fatal("per-file rejection must retain its error", rejected)
	}
}
