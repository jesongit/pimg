package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/diagnostics"
)

func TestVideoSizePurgeRechecksPairAndPreservesKeptFile(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, scenario := range []string{"valid", "different-content", "legacy-default", "unequal-size", "rounded-equal-size", "zero-size", "mixed-kind", "both-images", "default-image-mode", "invalid-mode", "missing-confirm", "same-file", "moved", "hash-changed", "keep-hidden", "keep-deleting", "not-ready", "busy", "moving", "foreign-owner"} {
			t.Run(fmt.Sprintf("admin=%t/%s", admin, scenario), func(t *testing.T) {
				s := interactionServer(t)
				owner, token := "user:video-size", "video-size-test-token"
				if err := s.DB.CreateAccount(owner, "video-size", "hash"); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(token))
				if err := s.DB.InsertToken(&dbx.APIToken{ID: "video-size", Name: "video-size", Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
					t.Fatal(err)
				}
				album, err := s.DB.EnsureOwnerDefaultAlbum(owner)
				if err != nil {
					t.Fatal(err)
				}
				other, err := s.DB.InsertOwnerAlbum(owner, "elsewhere")
				if err != nil {
					t.Fatal(err)
				}
				exec := func(query string, args ...any) {
					t.Helper()
					if _, err := s.DB.Exec(query, args...); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range []string{"video-keep", "video-remove"} {
					seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
					exec(`UPDATE files SET owner_id=?,kind='video',size=1048576 WHERE id=?`, owner, id)
				}
				body := map[string]any{"album_id": album, "keep_id": "video-keep", "remove_id": "video-remove", "keep_sha256": "hash", "remove_sha256": "hash", "comparison": "video_size", "confirm": "PURGE"}
				switch scenario {
				case "different-content":
					exec(`UPDATE files SET sha256='other-content' WHERE id='video-remove'`)
					body["remove_sha256"] = "other-content"
				case "legacy-default":
					exec(`UPDATE files SET album_id=NULL WHERE id='video-remove'`)
				case "unequal-size":
					exec(`UPDATE files SET size=2048576 WHERE id='video-remove'`)
				case "rounded-equal-size":
					exec(`UPDATE files SET size=size+1 WHERE id='video-remove'`)
				case "zero-size":
					exec(`UPDATE files SET size=0`)
				case "mixed-kind":
					exec(`UPDATE files SET kind='image' WHERE id='video-remove'`)
				case "both-images":
					exec(`UPDATE files SET kind='image'`)
				case "default-image-mode":
					delete(body, "comparison")
				case "invalid-mode":
					body["comparison"] = "any-file"
				case "missing-confirm":
					delete(body, "confirm")
				case "same-file":
					body["remove_id"] = "video-keep"
				case "moved":
					exec(`UPDATE files SET album_id=? WHERE id='video-remove'`, other)
				case "hash-changed":
					exec(`UPDATE files SET sha256='changed' WHERE id='video-remove'`)
				case "keep-hidden":
					exec(`UPDATE files SET hidden=1 WHERE id='video-keep'`)
				case "keep-deleting":
					exec(`UPDATE files SET purge_state='pending' WHERE id='video-keep'`)
				case "not-ready":
					exec(`UPDATE files SET archive_state='pending' WHERE id='video-remove'`)
				case "busy", "moving":
					typ := dbx.TaskVerify
					if scenario == "moving" {
						typ = dbx.TaskTelegramMoveFile
					}
					if _, err = s.DB.EnqueueTask(typ, "video-keep", "{}"); err != nil {
						t.Fatal(err)
					}
				case "foreign-owner":
					seedInteractionFile(t, s, "foreign-video", 1, sql.NullInt64{})
					exec(`UPDATE files SET kind='video',size=1048576 WHERE id='foreign-video'`)
					body["remove_id"] = "foreign-video"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				var w *httptest.ResponseRecorder
				if admin {
					w = adminCall(s, adminCookie(t, s, "video-size-admin"), "POST", "/api/admin/files/similar-purge?owner_id="+owner, string(encoded))
				} else {
					w = uploaderCall(s, "POST", "/api/account/files/similar-purge", token, body)
				}
				success := scenario == "valid" || scenario == "different-content" || scenario == "legacy-default"
				if success && w.Code != 200 || !success && w.Code < 400 {
					t.Fatal(w.Code, w.Body.String())
				}
				var count int
				if err = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=?`, dbx.TaskPurgeFile).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if success {
					want = 1
				}
				if count != want {
					t.Fatalf("purge tasks %d != %d", count, want)
				}
				if success {
					keep, _ := s.DB.GetFile("video-keep")
					remove, _ := s.DB.GetFile("video-remove")
					if keep.PurgeState.String != "" || remove.PurgeState.String != "pending" {
						t.Fatal("wrong side marked for purge")
					}
				}
			})
		}
	}
}

func TestSimilarPurgeRequiresFreshOwnedPairAndExplicitConfirmation(t *testing.T) {
	for _, scenario := range []string{"valid", "legacy-default", "missing-confirm", "same-file", "moved-remove", "moved-keep", "changed-hash", "keep-deleting", "keep-working", "keep-moving", "keep-not-ready", "foreign-owner", "forged-owner", "inverse-delete"} {
		t.Run(scenario, func(t *testing.T) {
			s := interactionServer(t)
			owner, token := "user:similar", "similar-test-token"
			if err := s.DB.CreateAccount(owner, "similar", "hash"); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(token))
			if err := s.DB.InsertToken(&dbx.APIToken{ID: "similar", Name: "similar", Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
				t.Fatal(err)
			}
			album, err := s.DB.EnsureOwnerDefaultAlbum(owner)
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.DB.InsertOwnerAlbum(owner, "other")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"keep", "remove"} {
				seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
				if _, err = s.DB.Exec(`UPDATE files SET owner_id=? WHERE id=?`, owner, id); err != nil {
					t.Fatal(err)
				}
			}
			body := map[string]any{"album_id": album, "keep_id": "keep", "remove_id": "remove", "keep_sha256": "hash", "remove_sha256": "hash", "confirm": "PURGE"}
			suffix := ""
			switch scenario {
			case "legacy-default":
				s.DB.Exec(`UPDATE files SET album_id=NULL WHERE id='remove'`)
			case "missing-confirm":
				delete(body, "confirm")
			case "same-file":
				body["remove_id"] = "keep"
			case "moved-remove":
				s.DB.Exec(`UPDATE files SET album_id=? WHERE id='remove'`, other)
			case "moved-keep":
				s.DB.Exec(`UPDATE files SET album_id=? WHERE id='keep'`, other)
			case "changed-hash":
				s.DB.Exec(`UPDATE files SET sha256='changed' WHERE id='remove'`)
			case "keep-deleting":
				s.DB.Exec(`UPDATE files SET purge_state='pending' WHERE id='keep'`)
			case "keep-working":
				if _, err = s.DB.EnqueueTask(dbx.TaskVerify, "keep", "{}"); err != nil {
					t.Fatal(err)
				}
			case "keep-moving":
				if _, err = s.DB.EnqueueTask(dbx.TaskTelegramMoveFile, "keep", "{}"); err != nil {
					t.Fatal(err)
				}
			case "keep-not-ready":
				s.DB.Exec(`UPDATE files SET archive_state='pending' WHERE id='keep'`)
			case "foreign-owner", "forged-owner":
				seedInteractionFile(t, s, "foreign-remove", 1, sql.NullInt64{})
				body["remove_id"] = "foreign-remove"
				if scenario == "forged-owner" {
					suffix = "?owner_id=" + dbx.AdminOwner
				}
			}
			var before int
			s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=?`, dbx.TaskPurgeFile).Scan(&before)
			w := uploaderCall(s, "POST", "/api/account/files/similar-purge"+suffix, token, body)
			success := scenario == "valid" || scenario == "legacy-default" || scenario == "inverse-delete"
			if success && w.Code != 200 || !success && w.Code < 400 {
				t.Fatal(w.Code, w.Body.String())
			}
			var after int
			s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=?`, dbx.TaskPurgeFile).Scan(&after)
			want := before
			if success {
				want++
			}
			if after != want {
				t.Fatalf("unexpected purge tasks %d != %d", after, want)
			}
			keep, _ := s.DB.GetFile("keep")
			remove, _ := s.DB.GetFile("remove")
			if success && (keep.PurgeState.String != "" || remove.PurgeState.String != "pending") {
				t.Fatal("retained side changed", keep.PurgeState, remove.PurgeState)
			}
			if scenario == "inverse-delete" {
				body["keep_id"], body["remove_id"] = "remove", "keep"
				w = uploaderCall(s, "POST", "/api/account/files/similar-purge", token, body)
				if w.Code != 409 {
					t.Fatal("inverse deletion accepted", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestSimilarPurgeConflictIsAvailableToReadOnlyDiagnostics(t *testing.T) {
	s := interactionServer(t)
	s.Log = slog.New(diagnostics.NewHandler(slog.NewTextHandler(io.Discard, nil), s.Diagnostics))
	diagnosticToken(t, s, "similar-diagnostic", "diagnostics", dbx.AdminOwner)
	album, err := s.DB.InsertAlbum("diagnostic pair")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"diagnostic-keep", "diagnostic-remove"} {
		seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
	}
	if _, err = s.DB.Exec(`UPDATE files SET sha256='changed-private-content-hash' WHERE id='diagnostic-remove'`); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"album_id":%d,"keep_id":"diagnostic-keep","remove_id":"diagnostic-remove","keep_sha256":"hash","remove_sha256":"hash","confirm":"PURGE"}`, album)
	w := adminCall(s, adminCookie(t, s, "diagnostic-admin"), "POST", "/api/admin/files/similar-purge", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "图片内容已改变") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = diagnosticCall(s, "/api/diagnostics/logs?q=similar_purge", "198.51.100.10:9999", "similar-diagnostic", "")
	text := w.Body.String()
	if w.Code != 200 || !strings.Contains(text, `"reason":"content_changed"`) || !strings.Contains(text, `"remove_id":"diagnostic-remove"`) || strings.Contains(text, "changed-private-content-hash") {
		t.Fatal("read-only diagnosis lacks sanitized conflict context", w.Code, text)
	}
	var tasks int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=?`, dbx.TaskPurgeFile).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatal("stale content created a destructive task", tasks, err)
	}
}

func TestAdminSimilarPurgeCanManageAnotherOwnersAlbum(t *testing.T) {
	s := interactionServer(t)
	owner := "user:managed-similar"
	if err := s.DB.CreateAccount(owner, "managed", "hash"); err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.InsertOwnerAlbum(owner, "managed album")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"managed-keep", "managed-remove"} {
		seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
		s.DB.Exec(`UPDATE files SET owner_id=? WHERE id=?`, owner, id)
	}
	json := fmt.Sprintf(`{"album_id":%d,"keep_id":"managed-keep","remove_id":"managed-remove","keep_sha256":"hash","remove_sha256":"hash","confirm":"PURGE"}`, album)
	w := adminCall(s, adminCookie(t, s, "similar-admin"), "POST", "/api/admin/files/similar-purge?owner_id="+owner, json)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSimilarPurgeAllowsReplicaSyncAndSerializesRemoval(t *testing.T) {
	for _, typ := range []string{dbx.TaskArchiveSync, dbx.TaskColdSync} {
		for _, state := range []string{"pending", "running"} {
			t.Run(typ+"-"+state, func(t *testing.T) {
				s := interactionServer(t)
				album, err := s.DB.InsertAlbum("replica sync")
				if err != nil {
					t.Fatal(err)
				}
				jobs := map[string]int64{}
				for _, id := range []string{"sync-keep", "sync-remove"} {
					seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
					jobs[id], err = s.DB.EnqueueTask(typ, id, "{}")
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.DB.Exec(`UPDATE tasks SET state=?,lease_until=unixepoch()+300 WHERE id=?`, state, jobs["sync-remove"]); err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"album_id":%d,"keep_id":"sync-keep","remove_id":"sync-remove","keep_sha256":"hash","remove_sha256":"hash","confirm":"PURGE"}`, album)
				w := adminCall(s, adminCookie(t, s, "sync-admin"), "POST", "/api/admin/files/similar-purge", body)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				keep, _ := s.DB.GetFile("sync-keep")
				remove, _ := s.DB.GetFile("sync-remove")
				if keep.PurgeState.String != "" || remove.PurgeState.String != "pending" {
					t.Fatal("wrong file marked for deletion", keep.PurgeState, remove.PurgeState)
				}
				for id, taskID := range jobs {
					var actual string
					var canceled bool
					if err = s.DB.QueryRow(`SELECT state,cancel_requested FROM tasks WHERE id=?`, taskID).Scan(&actual, &canceled); err != nil {
						t.Fatal(err)
					}
					if id == "sync-keep" && (actual != "pending" || canceled) {
						t.Fatal("retained image sync changed", actual, canceled)
					}
					if id == "sync-remove" && (!canceled || state == "running" && actual != "running" || state == "pending" && actual != "canceled") {
						t.Fatal("removed image sync not stopped safely", actual, canceled)
					}
				}
				if state == "running" {
					if task, claimErr := s.DB.ClaimIdleTask(1<<40, 30); claimErr != dbx.ErrNotFound || task != nil {
						t.Fatal("purge overlapped a running replica writer", task, claimErr)
					}
					if err = s.DB.FinishTask(jobs["sync-remove"], "{}"); err != nil {
						t.Fatal(err)
					}
				}
				task, err := s.DB.ClaimIdleTask(1<<40, 30)
				if err != nil || task.Type != dbx.TaskPurgeFile || task.FileID.String != "sync-remove" {
					t.Fatal("confirmed removal did not take priority over pending replica sync", task, err)
				}
			})
		}
	}
}
