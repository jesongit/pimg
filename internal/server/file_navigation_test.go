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

func TestGalleryNavigationUsesFullFilteredOrderAndAccountScope(t *testing.T) {
	s := interactionServer(t)
	albums := map[string]int64{}
	for _, owner := range []string{"user:alpha", "user:bravo"} {
		if err := s.DB.CreateAccount(owner, strings.TrimPrefix(owner, "user:"), "hash"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(owner))
		if err := s.DB.InsertToken(&dbx.APIToken{ID: owner, Name: owner, Scope: "account", OwnerID: owner, TokenHash: hex.EncodeToString(sum[:])}); err != nil {
			t.Fatal(err)
		}
		album, err := s.DB.InsertOwnerAlbum(owner, "Gallery")
		if err != nil {
			t.Fatal(err)
		}
		albums[owner] = album
		for i := 0; i < 75; i++ {
			file := &dbx.File{ID: fmt.Sprintf("%s-%03d", owner, i), OwnerID: owner, Name: fmt.Sprintf("capture-%03d", i), OrigName: "source.jpg", Ext: "jpg", Mime: "image/jpeg", Kind: "image", Size: int64(i + 1), SHA256: "hash", KeyWrapped: []byte{}, KeyNonce: []byte{}, AlbumID: sql.NullInt64{Int64: album, Valid: true}, ArchiveState: "done"}
			if err = s.DB.InsertFile(file); err != nil {
				t.Fatal(err)
			}
		}
	}
	query := "sort=random&seed=scope-test&per_page=60&kind=image&state=archived&q=capture&album_id=" + jsonNumber(albums["user:alpha"])
	var all []string
	for page := 1; page <= 2; page++ {
		response := uploaderCall(s, "GET", fmt.Sprintf("/api/account/files?%s&page=%d", query, page), "user:alpha", nil)
		var data struct {
			Items []struct{ ID string }
			Total int
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Total != 75 {
			t.Fatal(response.Code, response.Body.String())
		}
		for _, file := range data.Items {
			if !strings.HasPrefix(file.ID, "user:alpha-") {
				t.Fatal("foreign list entry", file.ID)
			}
			all = append(all, file.ID)
		}
	}
	for _, index := range []int{0, 59, 60, 74} {
		response := uploaderCall(s, "GET", "/api/account/files/navigation?"+query+"&id="+url.QueryEscape(all[index]), "user:alpha", nil)
		var nav dbx.FileNavigation
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &nav) != nil || nav.Total != 75 || nav.Position != index+1 {
			t.Fatal(response.Code, response.Body.String())
		}
		if index > 0 && nav.Previous != all[index-1] || index < 74 && nav.Next != all[index+1] {
			t.Fatal("neighbours differ from filtered paging", index, nav)
		}
	}
	for _, suffix := range []string{
		"&id=user%3Abravo-000",
		"&id=" + url.QueryEscape(all[0]) + "&q=no-match",
		"&id=" + url.QueryEscape(all[0]) + "&album_id=" + jsonNumber(albums["user:bravo"]),
	} {
		// Replace rather than duplicate the tested filter key.
		values, _ := url.ParseQuery(query)
		extra, _ := url.ParseQuery(strings.TrimPrefix(suffix, "&"))
		for key, valuesForKey := range extra {
			values[key] = valuesForKey
		}
		response := uploaderCall(s, "GET", "/api/account/files/navigation?"+values.Encode(), "user:alpha", nil)
		if response.Code != 404 {
			t.Fatal("out-of-scope file remained navigable", response.Code, response.Body.String())
		}
	}
	tooLong := uploaderCall(s, "GET", "/api/account/files?sort=random&seed="+strings.Repeat("x", 129), "user:alpha", nil)
	if tooLong.Code != 400 {
		t.Fatal("unbounded seed accepted", tooLong.Code)
	}
}
