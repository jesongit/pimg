package server

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"pimg/internal/dbx"
)

func TestFilePageMetadataMatchesDetailsAndIsolatesMalformedRecord(t *testing.T) {
	s := interactionServer(t)
	for _, id := range []string{"modern-good", "modern-bad", "legacy-good"} {
		seedInteractionFile(t, s, id, 1, sql.NullInt64{})
		if id == "legacy-good" {
			continue
		}
		if _, err := s.DB.Exec(`UPDATE files SET media_version=2 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.EnsureMediaReplica(dbx.MediaReplica{FileID: id, InstanceID: "local", PartMB: 256}); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.SaveMediaPart(id, "local", dbx.MediaPart{Count: 1, Ext: "png", MIME: "image/png", Size: 11, SHA256: "hash", Reference: `{"key":"synthetic/` + id + `"}`, Original: true}); err != nil {
			t.Fatal(err)
		}
	}
	list := func() map[string]map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleListFiles(w, httptest.NewRequest("GET", "/api/admin/files", nil))
		var data struct {
			Items []map[string]any
			Total int
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || data.Total != 3 {
			t.Fatal(w.Code, w.Body.String())
		}
		out := map[string]map[string]any{}
		for _, item := range data.Items {
			out[item["id"].(string)] = item
		}
		return out
	}
	initial := list()
	for _, id := range []string{"modern-good", "modern-bad", "legacy-good"} {
		f, err := s.DB.GetFile(id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(s.fileJSON(f))
		if err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err = json.Unmarshal(raw, &detail); err != nil || !reflect.DeepEqual(detail, initial[id]) {
			t.Fatal("batched metadata changed file semantics", id, err, string(raw), initial[id])
		}
	}
	if _, err := s.DB.Exec(`UPDATE media_parts SET metadata='{"reference":{}}' WHERE file_id='modern-bad'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.MediaReplicas("modern-bad"); err == nil {
		t.Fatal("strict archive loader accepted malformed metadata")
	}
	after := list()
	for _, id := range []string{"modern-good", "legacy-good"} {
		if !reflect.DeepEqual(initial[id], after[id]) {
			t.Fatal("one malformed record changed another file", id)
		}
	}
	var stored string
	if err := s.DB.QueryRow(`SELECT metadata FROM media_parts WHERE file_id='modern-bad'`).Scan(&stored); err != nil || stored != `{"reference":{}}` {
		t.Fatal("list silently rewrote a historical record", stored, err)
	}
}
