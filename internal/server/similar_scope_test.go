package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"pimg/internal/dbx"
)

func TestFilteredDedupeRechecksIndependentAlbumSnapshots(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, scenario := range []string{"image", "video", "legacy-null", "moved-keep", "moved-remove", "null-changed", "foreign-owner", "hash-changed", "keep-hidden", "keep-deleting", "busy", "video-size-changed", "mixed-kind", "missing-confirm", "missing-snapshot", "negative-snapshot", "ambiguous-scope"} {
			t.Run(fmt.Sprintf("admin=%t/%s", admin, scenario), func(t *testing.T) {
				s := interactionServer(t)
				owner, token := "user:filtered-dedupe", "filtered-dedupe-token"
				if err := s.DB.CreateAccount(owner, "filtered-dedupe", "hash"); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(token))
				if err := s.DB.InsertToken(&dbx.APIToken{ID: "filtered-dedupe", Name: "test", Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
					t.Fatal(err)
				}
				left, _ := s.DB.InsertOwnerAlbum(owner, "left")
				right, _ := s.DB.InsertOwnerAlbum(owner, "right")
				other, _ := s.DB.InsertOwnerAlbum(owner, "other")
				exec := func(query string, args ...any) {
					t.Helper()
					if _, err := s.DB.Exec(query, args...); err != nil {
						t.Fatal(err)
					}
				}
				for index, id := range []string{"scope-keep", "scope-remove"} {
					album := left
					if index == 1 {
						album = right
					}
					seedInteractionFile(t, s, id, 1, sql.NullInt64{Int64: album, Valid: true})
					exec(`UPDATE files SET owner_id=?,kind='image',size=12345 WHERE id=?`, owner, id)
				}
				body := map[string]any{"keep_id": "scope-keep", "remove_id": "scope-remove", "keep_album_id": left, "remove_album_id": right, "keep_sha256": "hash", "remove_sha256": "hash", "comparison": "image", "confirm": "PURGE"}
				want := 200
				switch scenario {
				case "video", "video-size-changed":
					exec(`UPDATE files SET kind='video'`)
					body["comparison"] = "video_size"
					if scenario == "video-size-changed" {
						exec(`UPDATE files SET size=size+1 WHERE id='scope-remove'`)
						want = 409
					}
				case "legacy-null":
					exec(`UPDATE files SET album_id=NULL WHERE id='scope-remove'`)
					body["remove_album_id"] = 0
				case "moved-keep":
					exec(`UPDATE files SET album_id=? WHERE id='scope-keep'`, other)
					want = 409
				case "moved-remove":
					exec(`UPDATE files SET album_id=? WHERE id='scope-remove'`, other)
					want = 409
				case "null-changed":
					body["remove_album_id"] = 0
					want = 409
				case "foreign-owner":
					seedInteractionFile(t, s, "foreign-scope-file", 1, sql.NullInt64{})
					body["remove_id"] = "foreign-scope-file"
					want = 404
				case "hash-changed":
					exec(`UPDATE files SET sha256='new-private-hash' WHERE id='scope-remove'`)
					want = 409
				case "keep-hidden":
					exec(`UPDATE files SET hidden=1 WHERE id='scope-keep'`)
					want = 404
				case "keep-deleting":
					exec(`UPDATE files SET purge_state='pending' WHERE id='scope-keep'`)
					want = 409
				case "busy":
					if _, err := s.DB.EnqueueTask(dbx.TaskVerify, "scope-keep", "{}"); err != nil {
						t.Fatal(err)
					}
					want = 409
				case "mixed-kind":
					exec(`UPDATE files SET kind='video' WHERE id='scope-remove'`)
					want = 409
				case "missing-confirm":
					delete(body, "confirm")
					want = 400
				case "missing-snapshot":
					delete(body, "remove_album_id")
					want = 400
				case "negative-snapshot":
					body["keep_album_id"] = -1
					want = 400
				case "ambiguous-scope":
					body["album_id"] = left
					want = 400
				}
				var code int
				var response string
				if admin {
					encoded, _ := json.Marshal(body)
					w := adminCall(s, adminCookie(t, s, "filtered-dedupe-admin"), "POST", "/api/admin/files/similar-purge?owner_id="+owner, string(encoded))
					code = w.Code
					response = w.Body.String()
				} else {
					w := uploaderCall(s, "POST", "/api/account/files/similar-purge", token, body)
					code = w.Code
					response = w.Body.String()
				}
				if code != want {
					t.Fatalf("want %d got %d: %s", want, code, response)
				}
				var count int
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=?`, dbx.TaskPurgeFile).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if want == 200 {
					if count != 1 {
						t.Fatal("expected one explicit purge", count)
					}
					keep, _ := s.DB.GetFile("scope-keep")
					remove, _ := s.DB.GetFile("scope-remove")
					if keep.PurgeState.String != "" || remove.PurgeState.String != "pending" {
						t.Fatal("wrong side queued")
					}
				} else if count != 0 {
					t.Fatal("rejected pair queued destructive task", count)
				}
			})
		}
	}
}
