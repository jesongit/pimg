package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func importServer(t *testing.T) *Server {
	t.Helper()
	s := interactionServer(t)
	id, err := s.DB.EnsureHomeAlbum()
	if err != nil {
		t.Fatal(err)
	}
	s.guestAlbumID = id
	return s
}

func uploadPolicyFile(t *testing.T, s *Server, suffix string) *dbx.File {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, err := mw.CreateFormFile("files", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	p.Write(interactionPNG(t))
	mw.Close()
	w := carrierRequest(t, s)("POST", "/api/admin/upload"+suffix, &body, mw.FormDataContentType())
	var res struct {
		Files []struct {
			ID string `json:"id"`
		}
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &res) != nil || len(res.Files) != 1 || res.Files[0].ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	f, err := s.DB.GetFile(res.Files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func archiveImportFile(t *testing.T, s *Server, f *dbx.File) {
	t.Helper()
	var err error
	if f.Storage == "direct" {
		err = s.Svc.RunDirectArchive(t.Context(), f.ID)
	} else {
		err = s.Svc.RunArchive(t.Context(), f.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE tasks SET state='done' WHERE file_id=? AND type IN ('archive','direct_archive')`, f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPrivatePlainUploadGuestVisibilityAndLegacyCompatibility(t *testing.T) {
	s := importServer(t)
	f := uploadPolicyFile(t, s, fmt.Sprintf("?encrypt=false&album_id=%d", s.guestAlbumID))
	archiveImportFile(t, s, f)
	f, _ = s.DB.GetFile(f.ID)
	if f.Storage != "direct" || f.Published {
		t.Fatal("plaintext private upload published", f)
	}
	_, total, err := s.DB.ListHome(s.guestAlbumID, 1, 20)
	if err != nil || total != 0 {
		t.Fatal("unpublished guest file leaked", total, err)
	}
	// Replaying a completed archive task must not publish a private upload.
	archiveImportFile(t, s, f)
	f, _ = s.DB.GetFile(f.ID)
	if f.Published {
		t.Fatal("retry silently published private upload")
	}
	if err = s.Svc.RunPublish(t.Context(), f.ID, false); err != nil {
		t.Fatal(err)
	}
	_, total, err = s.DB.ListHome(s.guestAlbumID, 1, 20)
	if err != nil || total != 1 {
		t.Fatal("explicit publish absent from home", total, err)
	}
	legacy := importServer(t)
	old := uploadPolicyFile(t, legacy, "?mode=direct")
	archiveImportFile(t, legacy, old)
	old, _ = legacy.DB.GetFile(old.ID)
	if !old.Published {
		t.Fatal("legacy explicit direct API changed")
	}
}

func createImportFixture(t *testing.T, s *Server, encrypted bool) (int64, string) {
	t.Helper()
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "photo.png"), interactionPNG(t), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("keep me"), 0400); err != nil {
		t.Fatal(err)
	}
	id, err := s.Svc.CreateLocalImport([]string{"."}, s.guestAlbumID, encrypted, true, "now")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id, root
}

func finishImport(t *testing.T, s *Server, id int64) {
	t.Helper()
	for i := 0; i < 10; i++ {
		err := s.Svc.RunLocalImport(t.Context(), id)
		if err != nil && !errors.Is(err, store.ErrLocalImportPending) {
			t.Fatal(err)
		}
		items, e := s.Svc.LocalImportItems(id, 0)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range items {
			if v.State == "archiving" {
				for _, fileID := range v.FileIDs {
					f, e := s.DB.GetFile(fileID)
					if e != nil {
						t.Fatal(e)
					}
					if f.ArchiveState != "done" {
						archiveImportFile(t, s, f)
					}
				}
			}
		}
		b, e := s.Svc.LocalImportBatch(id)
		if e != nil {
			t.Fatal(e)
		}
		if b.State == "done" || b.State == "done_errors" {
			return
		}
	}
	t.Fatal("batch failed to finish")
}

func TestLocalImportPreservesSourcePrivateDedupAndPause(t *testing.T) {
	s := importServer(t)
	id, root := createImportFixture(t, s, false)
	b, err := s.Svc.LocalImportBatch(id)
	if err != nil || b.State != "ready" || b.Counts["pending"] != 1 || b.Counts["skipped"] != 1 {
		t.Fatal(b, err)
	}
	if err = s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id, "pause"); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImport(t.Context(), id); !errors.Is(err, store.ErrLocalImportWaiting) {
		t.Fatal(err)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	if n != 0 {
		t.Fatal("paused intake accepted a source")
	}
	if err = s.Svc.ChangeLocalImport(id, "resume"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	items, _ := s.Svc.LocalImportItems(id, 0)
	var fileID string
	for _, v := range items {
		if v.State == "done" {
			fileID = v.FileIDs[0]
		}
	}
	f, _ := s.DB.GetFile(fileID)
	if f == nil || f.Published || f.Storage != "direct" {
		t.Fatal(f)
	}
	source, err := os.ReadFile(filepath.Join(root, "nested", "photo.png"))
	if err != nil || !bytes.Equal(source, interactionPNG(t)) {
		t.Fatal("source changed", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatal("import wrote to source", entries, err)
	}
	other, err := s.DB.InsertAlbum("另一个相册")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Svc.CreateLocalImport([]string{"nested/photo.png"}, other, false, false, "now")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id2); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id2, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id2)
	items, _ = s.Svc.LocalImportItems(id2, 0)
	if len(items) != 1 || items[0].State != "duplicate" || items[0].ExistingAlbum != "游客相册" {
		t.Fatal(items)
	}
	f, _ = s.DB.GetFile(fileID)
	if f.AlbumID.Int64 != s.guestAlbumID {
		t.Fatal("dedup moved original")
	}
	// A different encryption policy must produce a separate encrypted copy.
	id3, err := s.Svc.CreateLocalImport([]string{"nested/photo.png"}, other, true, false, "idle")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id3); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id3, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id3)
	items, _ = s.Svc.LocalImportItems(id3, 0)
	f, _ = s.DB.GetFile(items[0].FileIDs[0])
	if items[0].State != "done" || f.Storage == "direct" || f.Published {
		t.Fatal(items, f)
	}
	// Purge only PIMG's managed copies, never the read-only import source.
	// This manual task harness does not run queued replica tasks. Explicitly
	// retire those synthetic references, as the real engine would finish them.
	if _, err = s.DB.Exec(`UPDATE tasks SET state='canceled' WHERE file_id=? AND state IN ('pending','running','failed')`, fileID); err != nil {
		t.Fatal(err)
	}
	purgeTask, err := s.DB.EnqueuePurgeTask(fileID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunPurgeFile(store.WithTaskID(t.Context(), purgeTask), fileID); err != nil {
		t.Fatal(err)
	}
	if source, err = os.ReadFile(filepath.Join(root, "nested", "photo.png")); err != nil || !bytes.Equal(source, interactionPNG(t)) {
		t.Fatal("purge touched source", err)
	}
}

func TestLocalImportRejectsChangesTraversalAndAnonymousAccess(t *testing.T) {
	s := importServer(t)
	id, root := createImportFixture(t, s, true)
	for _, p := range []string{"../secret", "nested\\photo.png"} {
		if _, _, err := s.Svc.BrowseImports(p, 0); err == nil {
			t.Fatal("accepted unsafe path", p)
		}
	}
	if err := os.Chmod(filepath.Join(root, "nested", "photo.png"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "photo.png"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	b, _ := s.Svc.LocalImportBatch(id)
	if b.Counts["skipped"] != 2 || b.Counts["done"] != 0 {
		t.Fatal(b)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/local-imports/browse", nil))
	if w.Code != 401 {
		t.Fatal("anonymous filesystem access", w.Code)
	}
	request := carrierRequest(t, s)
	w = request("POST", "/api/admin/local-imports", strings.NewReader(`{"paths":["../"],"album_id":1}`), "application/json")
	if w.Code < 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBulkPurgeRequiresConfirmationAndReportsPerFileGates(t *testing.T) {
	s := importServer(t)
	request := carrierRequest(t, s)
	seedInteractionFile(t, s, "ready", 1, sql.NullInt64{})
	seedInteractionFile(t, s, "busy", 2, sql.NullInt64{})
	if _, err := s.DB.EnqueueTask(dbx.TaskVerify, "busy", "{}"); err != nil {
		t.Fatal(err)
	}
	w := request("POST", "/api/admin/batch", strings.NewReader(`{"action":"purge","ids":["ready","busy"]}`), "application/json")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/api/admin/batch", strings.NewReader(`{"action":"purge","confirm":"PURGE","ids":["ready","busy","missing","ready"]}`), "application/json")
	var result struct {
		Done, Skipped int
		Results       []map[string]any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Done != 2 || result.Skipped != 1 || len(result.Results) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	f, _ := s.DB.GetFile("ready")
	if f.PurgeState.String != "pending" {
		t.Fatal(f)
	}
	f, _ = s.DB.GetFile("busy")
	if f.PurgeState.String != "pending" {
		t.Fatal("busy file should automatically stop and queue deletion")
	}
}

func TestLocalImportVideoFingerprintAndCrashRecovery(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	s := importServer(t)
	s.Cfg.Upload.AllowedExts = []string{"png", "mp4"}
	s.Svc.SetDynamic(24, 30, 16, s.Cfg.Upload.AllowedExts)
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	input := filepath.Join(root, "clip.mp4")
	if output, e := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x120:d=0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", input).CombinedOutput(); e != nil {
		t.Fatalf("ffmpeg: %v %s", e, output)
	}
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	makeBatch := func() int64 {
		t.Helper()
		id, e := s.Svc.CreateLocalImport([]string{"clip.mp4"}, s.guestAlbumID, false, false, "now")
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Svc.RunLocalImportScan(t.Context(), id); e != nil {
			t.Fatal(e)
		}
		if e = s.Svc.ChangeLocalImport(id, "start"); e != nil {
			t.Fatal(e)
		}
		return id
	}
	id := makeBatch()
	if err = s.Svc.RunLocalImport(t.Context(), id); !errors.Is(err, store.ErrLocalImportPending) {
		t.Fatal(err)
	}
	items, _ := s.Svc.LocalImportItems(id, 0)
	if len(items) != 1 || items[0].State != "archiving" {
		t.Fatal(items)
	}
	first := items[0].FileIDs[0]
	// Simulate losing the post-intake receipt update in a process crash. The
	// transactional source/part mapping must recover the accepted video.
	if _, err = s.DB.Exec(`UPDATE local_import_items SET state='pending',file_ids='[]' WHERE batch_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.QueueLocalImports(); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	id2 := makeBatch()
	finishImport(t, s, id2)
	items, _ = s.Svc.LocalImportItems(id2, 0)
	if len(items) != 1 || items[0].State != "duplicate" || items[0].FileIDs[0] != first {
		t.Fatal("video was reimported", items)
	}
	got, err := os.ReadFile(input)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("video source changed", err)
	}
}

func TestLocalImportRetryPreservesCompletedVideoParts(t *testing.T) {
	s := importServer(t)
	id, _ := createImportFixture(t, s, false)
	seedInteractionFile(t, s, "completed-part", 1, sql.NullInt64{Int64: s.guestAlbumID, Valid: true})
	seedInteractionFile(t, s, "failed-part", 2, sql.NullInt64{Int64: s.guestAlbumID, Valid: true})
	if _, err := s.DB.Exec(`UPDATE files SET archive_state='failed' WHERE id='failed-part'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE local_import_batches SET state='done_errors' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE local_import_items SET state='failed',file_ids='["completed-part","failed-part"]' WHERE batch_id=? AND state='pending'`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.ChangeLocalImport(id, "retry"); err != nil {
		t.Fatal(err)
	}
	done, err := s.DB.GetFile("completed-part")
	if err != nil || done.ArchiveState != "done" {
		t.Fatal("completed segment reset by retry", done, err)
	}
	failed, err := s.DB.GetFile("failed-part")
	if err != nil || failed.ArchiveState != "pending" {
		t.Fatal("failed segment not retried", failed, err)
	}
}

func TestCancelImportTaskAlsoCancelsItsBatch(t *testing.T) {
	s := importServer(t)
	id, _ := createImportFixture(t, s, false)
	if err := s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := s.DB.QueryRow(`SELECT id FROM tasks WHERE type=? AND payload=? ORDER BY id DESC LIMIT 1`, dbx.TaskLocalImport, dbx.JSONValue(map[string]any{"batch_id": id})).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	response := carrierRequest(t, s)("POST", fmt.Sprintf("/api/admin/tasks/%d/cancel", taskID), nil, "")
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	b, err := s.Svc.LocalImportBatch(id)
	if err != nil || b.State != "canceled" {
		t.Fatal(b, err)
	}
	if err = s.Svc.QueueLocalImports(); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImport(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&count); err != nil || count != 0 {
		t.Fatal("canceled batch resurrected", count, err)
	}
}
