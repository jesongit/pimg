package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"pimg/internal/dbx"
	"strings"
	"testing"
)

func TestWebZeroLimitIgnoresRetiredConfigAndAppliesToHTTPUpload(t *testing.T) {
	s := interactionServer(t)
	s.Cfg.Upload.MaxSizeMB = 1
	s.Cfg.Upload.AllowedExts = []string{"jpg"}
	request := carrierRequest(t, s)
	w := request("PATCH", "/api/admin/settings", strings.NewReader(`{"upload_max_mb":0,"allowed_exts":"png"}`), "application/json")
	if w.Code != 200 || s.Svc.MaxSizeMB() != 0 {
		t.Fatal("zero not authoritative", w.Code, w.Body.String(), s.Svc.MaxSizeMB())
	}
	// Above the previous hidden 32 MiB multipart cap, streamed from disk.
	p := filepath.Join(t.TempDir(), "upload.png")
	file, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	file.Write(interactionPNG(t))
	file.Truncate(33 << 20)
	file.Close()
	body := filepath.Join(t.TempDir(), "multipart")
	output, e := os.Create(body)
	if e != nil {
		t.Fatal(e)
	}
	mw := multipart.NewWriter(output)
	part, _ := mw.CreateFormFile("files", "large.png")
	source, _ := os.Open(p)
	io.Copy(part, source)
	source.Close()
	mw.Close()
	output.Close()
	input, _ := os.Open(body)
	defer input.Close()
	w = request("POST", "/api/admin/upload?encrypt=false", input, mw.FormDataContentType())
	var response struct{ Files []struct{ ID, Error string } }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Files) != 1 || response.Files[0].ID == "" || response.Files[0].Error != "" {
		t.Fatal("unlimited HTTP intake rejected", w.Code, w.Body.String())
	}
	w = request("PATCH", "/api/admin/settings", strings.NewReader(`{"upload_max_mb":1}`), "application/json")
	if w.Code != 200 || s.Svc.MaxSizeMB() != 1 {
		t.Fatal("positive web limit ignored")
	}
	input.Seek(0, 0)
	w = request("POST", "/api/admin/upload?encrypt=false", input, mw.FormDataContentType())
	if w.Code != 201 || !bytes.Contains(w.Body.Bytes(), []byte("大小限制 1 MB")) {
		t.Fatal("positive web limit not enforced", w.Code, w.Body.String())
	}
}

func TestImportLogShowsAlbumAndRejectedFilesWithoutGalleryIDs(t *testing.T) {
	s := importServer(t)
	album, _ := s.DB.InsertAlbum("长视频相册")
	request := carrierRequest(t, s)
	if e := s.DB.SetSetting("site.upload_max_mb", "1"); e != nil {
		t.Fatal(e)
	}
	s.applyDynamicFromSettings()
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	f, e := os.Create(filepath.Join(root, "oversize.mp4"))
	if e != nil {
		t.Fatal(e)
	}
	f.Truncate(2 << 20)
	f.Close()
	id, e := s.Svc.CreateLocalImport([]string{"oversize.mp4"}, album, false, false, "now")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Svc.RunLocalImportScan(t.Context(), id); e != nil {
		t.Fatal(e)
	}
	b, e := s.Svc.LocalImportBatch(id)
	if e != nil || b.Counts["failed"] != 1 {
		t.Fatal("scan did not expose limit failure", b, e)
	}
	if e = s.Svc.ChangeLocalImport(id, "start"); e != nil {
		t.Fatal(e)
	}
	if e = s.Svc.RunLocalImport(t.Context(), id); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.FinishTask(b.TaskID, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(`UPDATE tasks SET state='done' WHERE type='local_import'`); e != nil {
		t.Fatal(e)
	}
	w := request("GET", fmt.Sprintf("/api/admin/logs?record=all&group_id=%d", b.TaskID), nil, "")
	var logs struct {
		Items []struct {
			Context   dbx.ActivityContext
			LastError string `json:"last_error"`
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &logs) != nil || len(logs.Items) != 1 || logs.Items[0].Context.ToAlbum == nil || logs.Items[0].Context.ToAlbum.Name != "长视频相册" || !strings.Contains(logs.Items[0].LastError, "1 MB") {
		t.Fatal("outer log is vague", w.Code, w.Body.String())
	}
	w = request("GET", fmt.Sprintf("/api/admin/logs/tasks/%d/files", b.TaskID), nil, "")
	var detail struct {
		Items []dbx.TaskOperationItem
		Total int
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Total != 1 || detail.Items[0].FileName != "oversize.mp4" || detail.Items[0].FileID != "" || detail.Items[0].State != "failed" || detail.Items[0].Error == "" {
		t.Fatal("rejected source omitted", w.Code, w.Body.String())
	}
}

func TestImportRecordReadErrorsDoNotBecomeDeletedFileFailures(t *testing.T) {
	s := importServer(t)
	id, _ := createImportFixture(t, s, false)
	seedInteractionFile(t, s, "record-read-error", 1, sql.NullInt64{})
	if _, e := s.DB.Exec(`UPDATE local_import_items SET state='archiving',file_ids='["record-read-error"]' WHERE batch_id=? AND state='pending'`, id); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(`ALTER TABLE files RENAME COLUMN archive_state TO fixture_hidden_archive_state`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Svc.LocalImportBatch(id); e == nil {
		t.Fatal("database read error was swallowed")
	}
	var state, message string
	if e := s.DB.QueryRow(`SELECT state,error FROM local_import_items WHERE batch_id=? AND file_ids!='[]'`, id).Scan(&state, &message); e != nil || state != "archiving" || message != "" {
		t.Fatal("read error corrupted import state", state, message, e)
	}
	if _, e := s.DB.Exec(`ALTER TABLE files RENAME COLUMN fixture_hidden_archive_state TO archive_state`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Svc.LocalImportBatch(id); e != nil {
		t.Fatal("transient record error did not recover", e)
	}
}

// Opt-in actual GiB input; never generated or loaded into memory by ordinary CI.
func TestLargeServerImportZeroLimitKeepsOriginal(t *testing.T) {
	p := os.Getenv("PIMG_TEST_LARGE_VIDEO")
	if p == "" {
		t.Skip("set PIMG_TEST_LARGE_VIDEO to a valid >1GiB video")
	}
	st, e := os.Stat(p)
	if e != nil || st.Size() < 1<<30 {
		t.Fatal("large fixture unavailable", e)
	}
	hash := func(p string) string {
		f, e := os.Open(p)
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		h := sha256.New()
		if _, e = io.Copy(h, f); e != nil {
			t.Fatal(e)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	before := hash(p)
	s := importServer(t)
	s.Cfg.Paths.ImportDir = filepath.Dir(p)
	s.Cfg.Upload.MaxSizeMB = 1
	s.Cfg.Upload.AllowedExts = []string{"png"}
	if e = s.DB.SetSetting("site.upload_max_mb", "0"); e != nil {
		t.Fatal(e)
	}
	s.applyDynamicFromSettings()
	id, e := s.Svc.CreateLocalImport([]string{filepath.Base(p)}, 0, false, false, "now")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Svc.RunLocalImportScan(t.Context(), id); e != nil {
		t.Fatal(e)
	}
	if e = s.Svc.ChangeLocalImport(id, "start"); e != nil {
		t.Fatal(e)
	}
	finishImport(t, s, id)
	items, e := s.Svc.LocalImportItems(id, 0)
	if e != nil || len(items) != 1 || items[0].State != "done" || len(items[0].FileIDs) != 1 {
		t.Fatal("GiB import incomplete", items, e)
	}
	f, e := s.DB.GetFile(items[0].FileIDs[0])
	if e != nil || f.Size != st.Size() || f.SHA256 != before || f.AlbumID == (sql.NullInt64{}) {
		t.Fatal("GiB metadata wrong", f, e)
	}
	original, e := s.Svc.ViewOriginal(t.Context(), f)
	if e != nil {
		t.Fatal(e)
	}
	original.Close()
	if hash(original.Name()) != before || hash(p) != before {
		t.Fatal("GiB original changed")
	}
	t.Logf("Actual %d-byte video imported with web limit=0; source and local original SHA256=%s", st.Size(), before)
}
