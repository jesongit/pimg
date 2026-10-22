package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/r2x"
	"pimg/internal/store"
	"pimg/internal/tasks"
	"pimg/internal/tgx"
)

func interactionServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, R2: config.R2{RetentionHours: 24, PresignMinutes: 30}, Upload: config.Upload{MaxSizeMB: 16, AllowedExts: []string{"png"}, MaxPixels: 40000000}}
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	svc, err := store.New(cfg, db, master, tgx.New("", "", ""), r2x.New("", "", "", "", "", "", "", ""), log)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, db, svc, tasks.New(db, svc, 1, log), log)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func seedInteractionFile(t *testing.T, s *Server, id string, created int64, albumID sql.NullInt64) {
	t.Helper()
	if err := s.DB.InsertFile(&dbx.File{ID: id, Name: id, OrigName: id + ".png", Ext: "png", Mime: "image/png", Size: created + 10, SHA256: "hash", Kind: "image", KeyWrapped: []byte{1}, KeyNonce: []byte{2}, AlbumID: albumID, ArchiveState: "done", Storage: "tg", ColdBackend: "local", CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryPaginationFiltersAndOperations(t *testing.T) {
	s := interactionServer(t)
	album, err := s.DB.InsertAlbum("旅行")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 72; i++ {
		a := sql.NullInt64{}
		if i%2 == 0 {
			a = sql.NullInt64{Int64: album, Valid: true}
		}
		id := fmt.Sprintf("file-%03d", i)
		seedInteractionFile(t, s, id, int64(i+1), a)
		if i%3 == 0 {
			if _, err := s.DB.Exec("UPDATE files SET storage='direct' WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.SetPublished(id, true, "path", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	list := func(query string) ([]map[string]any, int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleListFiles(w, httptest.NewRequest("GET", "/api/admin/files?"+query, nil))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var body struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Items, body.Total
	}
	first, total := list("per_page=60&page=1")
	second, _ := list("per_page=60&page=2")
	if total != 72 || len(first) != 60 || len(second) != 12 || first[0]["id"] == second[0]["id"] {
		t.Fatal("pagination loses or duplicates files")
	}
	private, n := list("state=unpublished&album_id=-1&sort=oldest")
	if n != 24 || len(private) != 24 {
		t.Fatalf("unpublished/ungrouped filter: %d", n)
	}
	for _, f := range private {
		if f["published"] != false || f["album_id"] != nil {
			t.Fatal("filter includes wrong files")
		}
	}
	bySize, _ := list("sort=size")
	if bySize[0]["id"] != "file-071" {
		t.Fatal("size order incorrect")
	}
	tid, err := s.DB.EnqueueTask(dbx.TaskPublish, "file-001", "{}")
	if err != nil {
		t.Fatal(err)
	}
	active, _ := list("q=file-001")
	op := active[0]["operation"].(map[string]any)
	if op["state"] != "pending" || int64(op["id"].(float64)) != tid {
		t.Fatal("pending operation not exposed")
	}
	if err := s.DB.FinishTask(tid, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/admin/files/file-001", nil)
	req.SetPathValue("id", "file-001")
	w := httptest.NewRecorder()
	s.handleGetFile(w, req)
	if !strings.Contains(w.Body.String(), `"state":"done"`) {
		t.Fatal("detail does not expose completed operation")
	}
}

func TestBatchMoveOutAndPartialResults(t *testing.T) {
	s := interactionServer(t)
	album, err := s.DB.InsertAlbum("旅行")
	if err != nil {
		t.Fatal(err)
	}
	seedInteractionFile(t, s, "private", 1, sql.NullInt64{Int64: album, Valid: true})
	seedInteractionFile(t, s, "public", 2, sql.NullInt64{})
	// This batch fixture tests unpublished versus published plaintext files;
	// encrypted archives are covered by the publication-policy integration test.
	if _, err := s.DB.Exec("UPDATE files SET storage='direct' WHERE id IN ('private','public')"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetPublished("public", true, "path", ""); err != nil {
		t.Fatal(err)
	}
	call := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleBatch(w, httptest.NewRequest("POST", "/api/admin/batch", strings.NewReader(body)))
		return w
	}
	moved := call(`{"action":"move_album","album_id":0,"ids":["private"]}`)
	f, _ := s.DB.GetFile("private")
	if moved.Code != 200 || f.AlbumID.Valid {
		t.Fatal("batch move-out did not clear album", moved.Body.String())
	}
	result := call(`{"action":"publish","ids":["private","public","missing"]}`)
	var body struct {
		Done, Skipped int
		Results       []struct{ Status, Reason string }
	}
	if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Done != 1 || body.Skipped != 2 || len(body.Results) != 3 || body.Results[0].Status != "submitted" || body.Results[1].Reason == "" || body.Results[2].Reason == "" {
		t.Fatal("partial result is not actionable", result.Body.String())
	}
	duplicate := call(`{"action":"publish","ids":["private"]}`)
	if !strings.Contains(duplicate.Body.String(), "发布任务正在执行") {
		t.Fatal("duplicate operation accepted")
	}
	if w := call(`{"action":"move_album","album_id":99999,"ids":["private"]}`); w.Code != 400 {
		t.Fatal("missing album accepted")
	}
}

func TestOriginalDownloadUsesArchiveAndRequiresAuthentication(t *testing.T) {
	s := interactionServer(t)
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(original.Bytes()), "旅行照片.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunTempMaterialize(context.Background(), f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	view, err := s.Svc.GetTempView(context.Background(), f, "original")
	if err != nil || view.ExpiresAt > time.Now().Add(30*time.Minute).Unix() || view.ExpiresAt < time.Now().Add(29*time.Minute).Unix() {
		t.Fatal("preview reports the copy retention instead of signed-link expiry", view, err)
	}
	req := httptest.NewRequest("GET", "/api/admin/files/"+f.ID+"/download", nil)
	req.SetPathValue("id", f.ID)
	w := httptest.NewRecorder()
	s.handleDownload(w, req)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), original.Bytes()) || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("download is not the archived original", w.Code, w.Header())
	}
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", "/api/admin/files/"+f.ID+"/download", nil))
	if unauth.Code != 401 {
		t.Fatal("download did not require authentication")
	}
}
