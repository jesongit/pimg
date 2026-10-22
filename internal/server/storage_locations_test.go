package server

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestAdminStorageLocationsFollowRecordedCopies(t *testing.T) {
	s := interactionServer(t) // Current selection is local/local.
	for _, id := range []string{"metadata-mixed", "metadata-private", "metadata-pending", "metadata-no-path", "foreign"} {
		seedInteractionFile(t, s, id, 1, sql.NullInt64{})
	}
	if _, err := s.DB.Exec(`UPDATE files SET cold_backend='telegram',hot_backend='r2' WHERE id='metadata-mixed'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetPublished("metadata-mixed", true, "pimg/metadata-mixed.png", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE files SET hot_backend='r2' WHERE id IN ('metadata-private','metadata-pending','metadata-no-path')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE files SET archive_state='running' WHERE id='metadata-pending'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE files SET published=1 WHERE id='metadata-no-path'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, obj := range []*dbx.TempObject{
		{ID: "local-original", FileID: "metadata-mixed", Backend: "local", Kind: "original", ExpiresAt: now + 7200},
		{ID: "r2-original", FileID: "metadata-mixed", Backend: "r2", Kind: "original", ExpiresAt: now + 3600},
		{ID: "r2-thumb", FileID: "metadata-mixed", Backend: "r2", Kind: "thumb", ExpiresAt: now + 1800},
		{ID: "expired", FileID: "metadata-mixed", Backend: "local", Kind: "thumb", ExpiresAt: now},
		{ID: "deleted", FileID: "metadata-mixed", Backend: "local", Kind: "thumb", ExpiresAt: now + 86400},
		{ID: "foreign-copy", FileID: "foreign", Backend: "local", Kind: "original", ExpiresAt: now + 86400},
	} {
		obj.ObjectKey = "tmp/private-object-key/" + obj.ID
		if err := s.DB.InsertTempObject(obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB.MarkTempDeleted("deleted"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.MarkTempReady("local-original"); err != nil {
		t.Fatal(err)
	}
	confirmed, unconfirmed := true, false

	type fileResponse struct {
		ID        string                `json:"id"`
		Locations []fileStorageLocation `json:"storage_locations"`
		Links     store.AvailableLinks  `json:"available_links"`
	}
	w := httptest.NewRecorder()
	s.handleListFiles(w, httptest.NewRequest("GET", "/api/admin/files?q=metadata-&per_page=200", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var list struct {
		Items []fileResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	want := map[string][]fileStorageLocation{
		"metadata-mixed": {
			{Role: "archive", Backend: "telegram"},
			{Role: "published", Backend: "r2"},
			{Role: "temporary", Backend: "local", Kind: "original", ExpiresAt: now + 7200, Confirmed: &confirmed},
			{Role: "temporary", Backend: "r2", Kind: "original", ExpiresAt: now + 3600, Confirmed: &unconfirmed},
			{Role: "temporary", Backend: "r2", Kind: "thumb", ExpiresAt: now + 1800, Confirmed: &unconfirmed},
		},
		"metadata-private": {{Role: "archive", Backend: "local"}},
		"metadata-pending": {},
		"metadata-no-path": {{Role: "archive", Backend: "local"}},
	}
	if len(list.Items) != len(want) {
		t.Fatalf("wrong number of files: %d", len(list.Items))
	}
	for _, item := range list.Items {
		if !reflect.DeepEqual(item.Locations, want[item.ID]) {
			t.Fatalf("%s: got %#v, want %#v", item.ID, item.Locations, want[item.ID])
		}
		if item.ID == "metadata-mixed" {
			if len(item.Links.Items) != 1 || item.Links.Items[0].ID != "temporary:local-original" || item.Links.Items[0].ExpiresAt <= now || item.Links.Items[0].ExpiresAt >= now+7200 {
				t.Fatalf("list did not include a bounded signature for the confirmed saved copy: %#v", item.Links)
			}
		} else if len(item.Links.Items) != 0 {
			t.Fatalf("local archives or unpublished records gained public links: %#v", item.Links)
		}
	}
	if strings.Contains(w.Body.String(), `"object_key"`) || strings.Contains(w.Body.String(), "private-object-key/r2-original") || strings.Contains(w.Body.String(), "private-object-key/r2-thumb") {
		t.Fatal("storage metadata exposed internal object keys")
	}

	req := httptest.NewRequest("GET", "/api/admin/files/metadata-mixed", nil)
	req.SetPathValue("id", "metadata-mixed")
	w = httptest.NewRecorder()
	s.handleGetFile(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var detail fileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail.Locations, want["metadata-mixed"]) {
		t.Fatalf("detail differs from list: %#v", detail.Locations)
	}
	if len(detail.Links.Items) != 1 || detail.Links.Items[0].ID != "temporary:local-original" {
		t.Fatal("detail did not include recorded links", detail.Links)
	}

	// Revocation clears the permanent copy while retaining archive and temp copies.
	if err := s.DB.SetPublished("metadata-mixed", false, "", ""); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleGetFile(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(detail.Locations) != 4 || detail.Locations[1].Role != "temporary" {
		t.Fatalf("revoked publication still marked permanent: %d %#v", w.Code, detail.Locations)
	}
}
