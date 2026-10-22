package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"os"
	"path/filepath"
	"pimg/internal/mediax"
	"testing"
)

func TestGalleryAPIDatesUseOriginalPhotoAndRetainIntakeTime(t *testing.T) {
	s := importServer(t)
	s.Svc.SetDynamic(-1, -1, -1, []string{"png", "jpg"})
	request := carrierRequest(t, s)
	plain, err := os.ReadFile(filepath.Join("..", "mediax", "testdata", "taken-20000101.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("files", "camera.jpg")
	part.Write(plain)
	w.Close()
	response := request("POST", "/api/admin/upload?encrypt=false", &body, w.FormDataContentType())
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var uploaded struct {
		Files []struct {
			ID        string `json:"id"`
			MediaAt   int64  `json:"media_at"`
			CreatedAt int64  `json:"created_at"`
			Source    string `json:"media_time_source"`
		}
	}
	if err = json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil || len(uploaded.Files) != 1 {
		t.Fatal(err)
	}
	f := uploaded.Files[0]
	if f.MediaAt != 946688887 || f.Source != "exif" || f.CreatedAt <= f.MediaAt {
		t.Fatal(f, response.Body.String())
	}
	calendar := request("GET", "/api/admin/files/timeline?group=day&tz_offset=480", nil, "")
	var index struct {
		Groups []struct {
			Key string `json:"key"`
		}
	}
	json.Unmarshal(calendar.Body.Bytes(), &index)
	if calendar.Code != 200 || len(index.Groups) != 1 || index.Groups[0].Key != "2000-01-01" {
		t.Fatal(calendar.Code, calendar.Body.String())
	}
}

func TestLocalImportReadsSourceBirthTimeAndMetadataWins(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	path := filepath.Join(root, "photo.png")
	if err := os.WriteFile(path, interactionPNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	birth, ok := mediax.FileCreatedTime(f)
	f.Close()
	if !ok {
		t.Skip("filesystem does not expose birth time")
	}
	id, err := s.Svc.CreateLocalImport([]string{root}, 0, false, true, "now")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	items, err := s.Svc.LocalImportItems(id, 0)
	if err != nil || len(items) != 1 || len(items[0].FileIDs) != 1 {
		t.Fatal(items, err)
	}
	record, err := s.DB.GetFile(items[0].FileIDs[0])
	if err != nil || record.Naming.TimeSource != "file_created" || record.MediaAt != birth.Unix() {
		t.Fatal(record, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, interactionPNG(t)) {
		t.Fatal("source changed", err)
	}
}
