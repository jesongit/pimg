package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/store"
	"pimg/internal/tgx"
)

func uploaderServerFixture(t *testing.T) *Server {
	t.Helper()
	s := coldRepositoryServer(t)
	s.Cfg.Telegram.ChatID = 77
	s.Cfg.Telegram.APIHash = "private-application-hash"
	svc, err := store.New(s.Cfg, s.DB, s.Svc.Master, tgx.New("http://invalid.invalid", "fixture", "fixture"), nil, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	s.Svc = svc
	svc.UserTG = &rawUserTG{}
	for _, topic := range []int{10, 20} {
		album, err := s.DB.InsertAlbum(fmt.Sprint("album", topic))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.DB.InsertTopicBinding(&dbx.TopicBinding{AlbumID: album, AccountID: 11, ChatID: 77, TopicID: topic, Name: fmt.Sprint("topic", topic), State: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []struct {
		id, scope, owner string
		topics           []int
	}{{"admin", "admin", "admin:1", nil}, {"tool", "uploader", "admin:1", []int{10}}, {"other", "uploader", "other-owner", []int{10}}, {"rotated", "uploader", "admin:1", []int{10}}, {"empty", "uploader", "admin:1", nil}} {
		sum := sha256.Sum256([]byte(v.id))
		if err = s.DB.InsertToken(&dbx.APIToken{ID: v.id, Name: v.id, TokenHash: hex.EncodeToString(sum[:]), Prefix: v.id, Scope: v.scope, OwnerID: v.owner, TopicIDs: v.topics}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func uploaderCall(s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestRetiredUploaderEndpointsAndTokens(t *testing.T) {
	s := uploaderServerFixture(t)
	for _, token := range []string{"", "admin", "tool"} {
		for _, endpoint := range []struct{ method, path string }{{"GET", "/api/uploader/v1/config"}, {"POST", "/api/uploader/v1/keys"}} {
			w := uploaderCall(s, endpoint.method, endpoint.path, token, map[string]any{})
			if w.Code != 404 {
				t.Fatalf("retired endpoint responded: %s %d", endpoint.path, w.Code)
			}
		}
	}
	w := uploaderCall(s, "POST", "/api/admin/tokens", "admin", map[string]any{"name": "old uploader", "scope": "uploader", "topic_ids": []int{10}})
	if w.Code != 400 {
		t.Fatal("retired tool token can still be created", w.Code)
	}
}
