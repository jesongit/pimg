package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/devmock"
	"pimg/internal/r2x"
	"pimg/internal/store"
)

func TestPrivatePreviewsStayLocalAndExplicitSharesUsePublicTempDomain(t *testing.T) {
	s := interactionServer(t)
	mock, err := devmock.StartS3()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	r2 := &s.Cfg.R2
	r2.Endpoint = mock.Base
	r2.AccessKeyID = "k"
	r2.SecretAccessKey = "s"
	r2.TempBucket = "temp"
	r2.PermBucket = "perm"
	r2.PublicBaseURL = "https://perm.example.com"
	r2.TempPublicBaseURL = "https://temp.example.com"
	r2.TempCleanupBucket = "control"
	r2.CDNZoneID = "zone"
	r2.CDNAPIToken = "token"
	s.Svc.R2 = r2x.New(mock.Base, "k", "s", "temp", "perm", r2.PublicBaseURL, "zone", "token", r2x.WithPublicTemp(r2.TempPublicBaseURL, "control", "", 600))
	for id, c := range map[string]store.InstanceConfig{
		"r2":      {Endpoint: mock.Base, AccessKey: "k", SecretKey: "s", Bucket: "perm", PublicBase: r2.PublicBaseURL},
		"r2-temp": {Endpoint: mock.Base, AccessKey: "k", SecretKey: "s", Bucket: "temp", TempPublicBase: r2.TempPublicBaseURL},
	} {
		if _, err = s.Svc.SaveStorageInstance(id, id, "r2", c, true); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Svc.SetStorage("local", "r2"); err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if err = png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	f, err := legacyUploadFixture(s.Svc,context.Background(), bytes.NewReader(original.Bytes()), "photo.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("fixture-token"))
	if err = s.DB.InsertToken(&dbx.APIToken{ID: "fixture", Name: "fixture", TokenHash: fmt.Sprintf("%x", sum)}); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer fixture-token")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/api/admin/files/" + f.ID + "/temp"
	if w := request("POST", path, "{}", false); w.Code != 401 {
		t.Fatal("temporary links require authentication", w.Code)
	}
	if w := request("GET", "/api/admin/files/previews?ids="+f.ID, "", false); w.Code != 401 {
		t.Fatal("preview lookup requires authentication")
	}
	var pending store.TempViewResult
	w := request("POST", path, `{"share":true}`, true)
	if err = json.Unmarshal(w.Body.Bytes(), &pending); err != nil || w.Code != 200 || pending.Status != "pending" {
		t.Fatal("image materialization must be queued", w.Body.String(), err)
	}
	for _, kind := range []string{"original"} {
		if err = s.Svc.RunTempMaterialize(context.Background(), f.ID, kind); err != nil {
			t.Fatal(err)
		}
		w = request("POST", path, `{"kind":"`+kind+`","share":true}`, true)
		var result store.TempViewResult
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Status != "ready" || !strings.HasPrefix(result.URL, "https://temp.example.com/tmp/v1/") || strings.Contains(result.URL, "?") || result.ExpiresAt <= time.Now().Unix() {
			t.Fatal("public temporary image URL missing", w.Body.String(), err)
		}
	}
	w = request("GET", "/api/admin/files/previews?ids="+f.ID, "", true)
	var response struct {
		Previews map[string]store.TempViewResult `json:"previews"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Previews[f.ID].Status != "ready" || response.Previews[f.ID].URL != privateContentURL(f.ID, "thumb") {
		t.Fatal("library preview must use its private local compressed image", w.Body.String(), err)
	}
}
