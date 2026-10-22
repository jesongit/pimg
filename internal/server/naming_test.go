package server

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"testing"

	"pimg/internal/filenames"
)

func TestUploadCreatedTimeIsPerFileAndOriginalNamesRemain(t *testing.T) {
	s := interactionServer(t)
	request := carrierRequest(t, s)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("file_created_at", "2000-01-01T09:00:00+08:00"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"camera-random.png", "second.png"} {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		// Distinct originals: this test checks per-file time capture, not dedup.
		if _, err := part.Write(append(interactionPNG(t), []byte(name)...)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := request("POST", "/api/admin/upload?encrypt=true", &body, writer.FormDataContentType())
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Files []struct {
			ID, Name string
			OrigName string `json:"orig_name"`
			Naming   filenames.Metadata
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Files) != 2 {
		t.Fatal("incomplete upload", err, response.Body.String())
	}
	first, second := result.Files[0], result.Files[1]
	if first.Name != "PE_20000101_0001" || first.Naming.TimeSource != "file_created" || first.OrigName != "camera-random.png" {
		t.Fatal("explicit creation time ignored", first)
	}
	if second.Naming.TimeSource != "ingested" {
		t.Fatal("first file creation time leaked into next file", second)
	}
	// Download is a restored image with an R marker and the original extension.
	if err := s.Svc.RunArchive(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	got := request("GET", "/api/admin/files/"+first.ID+"/download", nil, "")
	_, params, _ := mime.ParseMediaType(got.Header().Get("Content-Disposition"))
	if got.Code != 200 || params["filename"] != "PR_20000101_0001.png" {
		t.Fatal("download", got.Code, params)
	}
}
