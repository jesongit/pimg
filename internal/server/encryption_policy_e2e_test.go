package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/filenames"
	"pimg/internal/storage"
	"pimg/internal/tasks"
)

func policyRequest(t *testing.T, s *Server, method, address, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, address, bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer policy-e2e-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func policyServer(t *testing.T) *Server {
	t.Helper()
	s := interactionServer(t)
	hash := sha256.Sum256([]byte("policy-e2e-token"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "policy-e2e", Name: "policy fixture", TokenHash: fmt.Sprintf("%x", hash)}); err != nil {
		t.Fatal(err)
	}
	return s
}

func finishPolicyArchiveFixture(t *testing.T, s *Server, id, taskType string) {
	t.Helper()
	// The fixture ran its archive synchronously. Complete that queue entry
	// before starting workers so it cannot race the conversion request.
	task, err := s.DB.FindActiveTask(taskType, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.FinishTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func waitPolicyTask(t *testing.T, s *Server, id int64) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.DB.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == "failed" {
			t.Fatalf("policy task failed: %s", task.LastError.String)
		}
		if task.State == "done" {
			var result map[string]any
			if err := json.Unmarshal([]byte(task.Result.String), &result); err != nil {
				t.Fatalf("conversion task has no usable result: %v", err)
			}
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := s.DB.GetTask(id)
	t.Fatalf("policy task did not complete: %#v", task)
	return nil
}

func TestEncryptionPolicyHTTPConversionRoundTrip(t *testing.T) {
	s := policyServer(t)
	ctx := context.Background()
	plain := interactionPNG(t)
	album, err := s.DB.InsertAlbum("conversion album")
	if err != nil {
		t.Fatal(err)
	}
	f, err := legacyUploadFixture(s.Svc, ctx, bytes.NewReader(plain), "round-trip.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunDirectArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	finishPolicyArchiveFixture(t, s, f.ID, dbx.TaskDirectArchive)
	if err := s.Svc.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	links, err := s.Svc.FileLinks(ctx, f.ID)
	if err != nil || len(links.Items) < 2 {
		t.Fatal("fixture needs permanent and temporary serving copies", err)
	}
	oldLinks := make([]string, 0, len(links.Items))
	for _, link := range links.Items {
		if w := policyRequest(t, s, "GET", link.URL, ""); w.Code != 200 {
			t.Fatalf("old serving link was not initially readable: %d", w.Code)
		}
		oldLinks = append(oldLinks, link.URL)
	}
	if err := s.Eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Eng.Stop)
	originalID := f.ID
	for _, encrypted := range []bool{true, false} {
		response := policyRequest(t, s, "POST", "/api/admin/files/"+f.ID+"/convert", fmt.Sprintf(`{"encrypted":%t,"confirm":"CONVERT"}`, encrypted))
		// Reading a temporary link can enqueue verification. Let an already
		// running verifier finish before retrying the deliberately guarded API.
		deadline := time.Now().Add(15 * time.Second)
		for response.Code == 409 && strings.Contains(response.Body.String(), "该文件另有任务正在执行") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			response = policyRequest(t, s, "POST", "/api/admin/files/"+f.ID+"/convert", fmt.Sprintf(`{"encrypted":%t,"confirm":"CONVERT"}`, encrypted))
		}
		if response.Code != 202 {
			t.Fatalf("conversion did not enqueue: %d %s", response.Code, response.Body.String())
		}
		var queued struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil || queued.TaskID == 0 {
			t.Fatal("missing conversion task ID", err)
		}
		result := waitPolicyTask(t, s, queued.TaskID)
		newID, _ := result["new_file_id"].(string)
		if newID == "" || newID == f.ID || newID == originalID {
			t.Fatal("conversion reused a retired file ID")
		}
		converted, err := s.DB.GetFile(newID)
		if err != nil || (converted.Storage == "direct") == encrypted || converted.Published || converted.ShowHome || converted.ColdBackend != f.ColdBackend || converted.Name != filenames.Stem(f.Kind, encrypted, f.Naming) || converted.Naming != f.Naming || converted.AlbumID != f.AlbumID || converted.CreatedAt != f.CreatedAt {
			t.Fatalf("conversion lost metadata, changed providers, or auto-published: %#v, %v", converted, err)
		}
		raw, err := s.Svc.SourceStream(ctx, converted)
		if err != nil {
			t.Fatal(err)
		}
		recovered, readErr := io.ReadAll(raw)
		raw.Close()
		if readErr != nil || !bytes.Equal(recovered, plain) {
			t.Fatal("converted archive does not recover the original bytes", readErr)
		}
		var chunks, temps int
		cleanupDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(cleanupDeadline) {
			if err := s.DB.QueryRow("SELECT COUNT(*) FROM file_chunks WHERE file_id=?", f.ID).Scan(&chunks); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects WHERE file_id=? AND deleted_at IS NULL", f.ID).Scan(&temps); err != nil {
				t.Fatal(err)
			}
			if chunks == 0 && temps == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if chunks != 0 || temps != 0 {
			t.Fatal("retired archive still has live old chunks or temporary objects", chunks, temps)
		}
		listed, total, err := s.DB.ListFiles(dbx.FileFilter{})
		if err != nil || total != 1 || len(listed) != 1 || listed[0].ID != newID {
			t.Fatal("conversion staging or retired source leaked into the gallery", total, err)
		}
		for _, address := range oldLinks {
			if w := policyRequest(t, s, "GET", address, ""); w.Code != 404 {
				t.Fatalf("old serving link survived conversion: %d", w.Code)
			}
		}
		publish := policyRequest(t, s, "POST", "/api/admin/files/"+newID+"/publish", "{}")
		if encrypted && publish.Code != 409 || !encrypted && publish.Code != 200 {
			t.Fatalf("permanent publishing violated the archive format policy: %t -> %d %s", encrypted, publish.Code, publish.Body.String())
		}
		if encrypted {
			if w := policyRequest(t, s, "POST", "/api/admin/files/"+newID+"/temp", `{"kind":"original","share":true}`); w.Code != 200 {
				t.Fatal("encrypted files lost temporary publication", w.Code, w.Body.String())
			}
		} else {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				fresh, err := s.DB.GetFile(newID)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.Published {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			fresh, _ := s.DB.GetFile(newID)
			if !fresh.Published {
				t.Fatal("converted plaintext archive could not be permanently published")
			}
			for _, address := range oldLinks {
				if w := policyRequest(t, s, "GET", address, ""); w.Code != 404 {
					t.Fatal("publishing the converted plaintext archive revived an old URL", w.Code)
				}
			}
		}
		f = converted
	}
}

func TestEncryptionPolicyStartupRevokesLegacyPermanentCopiesOnly(t *testing.T) {
	s := policyServer(t)
	ctx := context.Background()
	plain := interactionPNG(t)
	f, err := legacyUploadFixture(s.Svc, ctx, bytes.NewReader(plain), "legacy.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	finishPolicyArchiveFixture(t, s, f.ID, dbx.TaskArchive)
	if err := s.Svc.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	// Reproduce a pre-policy permanent publication without bypassing the new
	// service guard: only this isolated fixture writes a legacy hot copy.
	hot, err := storage.NewLocalHot(s.Cfg.Storage.LocalHotDir, "")
	if err != nil {
		t.Fatal(err)
	}
	key := "pimg/" + f.ID + ".png"
	if err := hot.PutObject(ctx, storage.Permanent, key, bytes.NewReader(plain), int64(len(plain)), "image/png", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE files SET hot_backend='local' WHERE id=?", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetPublished(f.ID, true, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Eng.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f, err = s.DB.GetFile(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !f.Published {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.Published || f.ArchiveState != "done" {
		t.Fatal("legacy encrypted publication was not automatically revoked")
	}
	if _, err := hot.HeadSize(ctx, storage.Permanent, key); err == nil {
		t.Fatal("legacy permanent object survived startup cleanup")
	}
	if _, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix()); err != nil {
		t.Fatal("startup cleanup removed a permitted temporary publication", err)
	}
	stream, err := s.Svc.SourceStream(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(stream)
	stream.Close()
	if readErr != nil || !bytes.Equal(data, plain) {
		t.Fatal("startup cleanup damaged the encrypted archive", readErr)
	}
	if w := policyRequest(t, s, http.MethodPost, "/api/admin/files/"+f.ID+"/publish", "{}"); w.Code != 409 {
		t.Fatal("legacy file can be permanently published again", w.Code)
	}
}

func TestEncryptionPolicyRetiredTGIsUnreadableWhenCleanupFails(t *testing.T) {
	s := telegramLinkServer(t)
	hash := sha256.Sum256([]byte("policy-e2e-token"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "policy-retired-tg", Name: "policy fixture", TokenHash: fmt.Sprintf("%x", hash)}); err != nil {
		t.Fatal(err)
	}
	f, plain := archiveTelegramFixture(t, s, false)
	finishPolicyArchiveFixture(t, s, f.ID, dbx.TaskArchive)
	upstream, err := url.Parse(s.Cfg.Telegram.APIBase)
	if err != nil {
		t.Fatal(err)
	}
	s.Svc.TG.SetHTTPClient(&http.Client{Transport: telegramLinkTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != upstream.Host {
			return nil, fmt.Errorf("fixture refused a non-mock destination")
		}
		if strings.HasSuffix(r.URL.Path, "/deleteMessage") {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"message cannot be deleted"}`)), Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})})
	s.Eng = tasks.New(s.DB, s.Svc, 1, s.Log)
	if err := s.Eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Eng.Stop)
	w := policyRequest(t, s, "POST", "/api/admin/files/"+f.ID+"/convert", `{"encrypted":false,"confirm":"CONVERT"}`)
	if w.Code != 202 {
		t.Fatal("conversion did not enqueue", w.Code, w.Body.String())
	}
	var queued struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	result := waitPolicyTask(t, s, queued.TaskID)
	newID, _ := result["new_file_id"].(string)
	if newID == "" || newID == f.ID {
		t.Fatal("conversion did not switch to a new file ID")
	}
	// Cleanup cannot erase the old mock Telegram chunks. Even a valid admin
	// token must only read the verified replacement through any Pimg data route.
	deadline := time.Now().Add(10 * time.Second)
	var attempted int
	for time.Now().Before(deadline) {
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE file_id=? AND type='cleanup_converted' AND attempts>0 AND COALESCE(last_error,'')!=''", f.ID).Scan(&attempted); err != nil {
			t.Fatal(err)
		}
		if attempted > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if attempted == 0 {
		t.Fatal("cleanup failure was not recorded separately after successful conversion")
	}
	var remaining int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM file_chunks WHERE file_id=?", f.ID).Scan(&remaining); err != nil || remaining == 0 {
		t.Fatal("failed cleanup must retain the old remote locations", remaining, err)
	}
	for _, address := range []string{
		"/media/tg/" + f.ID + "/original",
		"/media/tg/" + f.ID + "/thumb",
		"/media/tg/" + f.ID + "/blur",
		"/api/admin/files/" + f.ID + "/download",
		"/api/admin/files/" + f.ID + "/content?kind=original",
		"/api/admin/files/" + f.ID + "/content?kind=thumb",
		"/api/admin/files/" + f.ID + "/export",
		"/api/admin/files/" + f.ID + "/links",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			if w := policyRequest(t, s, method, address, ""); w.Code != 404 {
				t.Fatalf("retired data remains accessible through %s %s: %d", method, address, w.Code)
			}
		}
	}
	for _, action := range []string{"temp", "publish", "verify", "retry"} {
		if w := policyRequest(t, s, "POST", "/api/admin/files/"+f.ID+"/"+action, "{}"); w.Code < 400 {
			t.Fatal("retired source accepted a new data-producing operation", action, w.Code)
		}
	}
	receipt := policyRequest(t, s, "GET", "/api/admin/files/"+f.ID, "")
	var retired map[string]json.RawMessage
	if receipt.Code != 200 || json.Unmarshal(receipt.Body.Bytes(), &retired) != nil || retired["conversion"] == nil {
		t.Fatal("retired source lost its conversion receipt", receipt.Code)
	}
	for _, key := range []string{"url", "thumb_url", "blur_url", "storage_locations", "chunks"} {
		if _, exists := retired[key]; exists {
			t.Fatal("conversion receipt exposed retired data locations", key)
		}
	}
	previews := policyRequest(t, s, "GET", "/api/admin/files/previews?ids="+f.ID, "")
	var listed struct {
		Previews map[string]json.RawMessage `json:"previews"`
	}
	if previews.Code != 200 || json.Unmarshal(previews.Body.Bytes(), &listed) != nil || listed.Previews[f.ID] != nil {
		t.Fatal("batch preview exposed a retired source", previews.Code, previews.Body.String())
	}
	if w := policyRequest(t, s, "GET", "/media/tg/"+newID+"/original", ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatal("old cleanup failure blocked or corrupted the new raw plaintext archive", w.Code)
	}
}
