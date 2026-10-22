package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestEncryptionFilterUsesFileSettingAcrossPagesTimelineAndNavigation(t *testing.T) {
	s := interactionServer(t)
	owner := "user:encryption-filter"
	if err := s.DB.CreateAccount(owner, "encryption-filter", "hash"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(owner))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
	album, err := s.DB.InsertOwnerAlbum(owner, "Filter album")
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{owner, dbx.AdminOwner} {
		for index := 0; index < 74; index++ {
			storage := "direct"
			if index%2 == 0 {
				storage = "tg"
			}
			file := &dbx.File{ID: fmt.Sprintf("%s-%03d", account, index), OwnerID: account, Name: fmt.Sprintf("image-%03d", index), OrigName: "source.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 100, SHA256: "test-hash", Storage: storage, KeyWrapped: []byte{}, KeyNonce: []byte{}, ArchiveState: "done"}
			if account == owner {
				file.AlbumID = sql.NullInt64{Int64: album, Valid: true}
			}
			if err = s.DB.InsertFile(file); err != nil {
				t.Fatal(err)
			}
			// Exercise both legacy and modern metadata, with no encrypted replicas.
			if index%3 != 0 {
				if _, err = s.DB.Exec(`UPDATE files SET media_version=2 WHERE id=?`, file.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cookie := adminCookie(t, s, "encryption-filter-admin")
	for _, mode := range []string{"", "encrypted", "plaintext"} {
		query := url.Values{"kind": {"image"}, "state": {"archived"}, "q": {"image"}, "album_id": {jsonNumber(album)}, "sort": {"random"}, "seed": {"encryption-filter"}, "per_page": {"20"}, "encryption": {mode}}
		expected := 37
		if mode == "" {
			expected = 74
		}
		var ids []string
		for page := 1; len(ids) < expected; page++ {
			query.Set("page", jsonNumber(int64(page)))
			response := uploaderCall(s, "GET", "/api/account/files?"+query.Encode(), owner, nil)
			var body struct {
				Items []struct {
					ID        string `json:"id"`
					Encrypted *bool  `json:"encrypted"`
					Requested *bool  `json:"encryption_requested"`
				}
				Total int
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Total != expected || len(body.Items) == 0 {
				t.Fatal(mode, page, response.Code, response.Body.String())
			}
			for _, file := range body.Items {
				setting := file.Requested
				if setting == nil {
					setting = file.Encrypted
				}
				if !strings.HasPrefix(file.ID, owner+"-") || setting == nil || mode != "" && *setting != (mode == "encrypted") {
					t.Fatal("filter leaked owner or used actual replicas", mode, file)
				}
				ids = append(ids, file.ID)
			}
		}
		query.Del("page")
		for _, index := range []int{0, 19, 20, len(ids) - 1} {
			query.Set("id", ids[index])
			response := uploaderCall(s, "GET", "/api/account/files/navigation?"+query.Encode(), owner, nil)
			var nav dbx.FileNavigation
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &nav) != nil || nav.Total != expected || nav.Position != index+1 || index > 0 && nav.Previous != ids[index-1] || index+1 < len(ids) && nav.Next != ids[index+1] {
				t.Fatal("navigation ignored encryption scope", mode, response.Code, response.Body.String())
			}
		}
		query.Del("id")
		response := uploaderCall(s, "GET", "/api/account/files/timeline?group=day&"+query.Encode(), owner, nil)
		var timeline struct{ Groups []dbx.FileMonth }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &timeline) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		total := 0
		for _, group := range timeline.Groups {
			total += group.Count
		}
		if total != expected {
			t.Fatal("timeline ignored encryption filter", mode, total)
		}
		response = adminCall(s, cookie, "GET", "/api/admin/files?owner_id="+url.QueryEscape(owner)+"&"+query.Encode(), "")
		var adminList struct{ Total int }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &adminList) != nil || adminList.Total != expected {
			t.Fatal("administrator owner filter differs", mode, response.Code, response.Body.String())
		}
	}
	for _, endpoint := range []string{"files", "files/timeline", "files/navigation"} {
		response := uploaderCall(s, "GET", "/api/account/"+endpoint+"?encryption=invalid&id="+url.QueryEscape(owner+"-000"), owner, nil)
		if response.Code != 400 {
			t.Fatal("invalid encryption filter accepted", endpoint, response.Code)
		}
	}
}
