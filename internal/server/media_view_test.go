package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/store"
)

func TestMediaRoutesKeepCopyEncryptionAndEnforceAccess(t *testing.T) {
	s := interactionServer(t)
	if err := s.DB.EnsureAdminWithSetup("admin", adminPasswordHash(t, "fixture-password"), false); err != nil {
		t.Fatal(err)
	}
	config := store.InstanceConfig{Directory: t.TempDir(), Encrypt: true, VideoPartMB: 256, Priority: 10}
	peer, err := s.Svc.SaveStorageInstance("", "加密副本", "local", config, true)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Svc.StoragePolicy()
	p.Archives = []string{"local", peer}
	if err = s.Svc.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	plain := interactionPNG(t)
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(plain), "source.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	cookie := adminCookie(t, s, "media-session")
	w := adminCall(s, cookie, "GET", "/api/admin/files/"+f.ID, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var detail struct {
		MediaVersion int                   `json:"media_version"`
		Encrypted    *bool                 `json:"encrypted"`
		Locations    []fileStorageLocation `json:"storage_locations"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.MediaVersion != 2 || detail.Encrypted != nil {
		t.Fatal("file-wide encryption state leaked")
	}
	states := map[string]bool{}
	for _, l := range detail.Locations {
		if l.Encrypted == nil {
			t.Fatal("actual encryption missing", l)
		}
		states[l.Backend] = *l.Encrypted
	}
	if states["local"] || !states[peer] {
		t.Fatal("incorrect copy encryption", states)
	}
	path := "/api/admin/files/" + f.ID + "/content?kind=original&part=0"
	w = adminCall(s, nil, "GET", path, "")
	if w.Code != 401 {
		t.Fatal("private original bypassed authentication", w.Code)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(cookie)
	r.Header.Set("Range", "bytes=0-15")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[:16]) {
		t.Fatal("local original Range failed", w.Code, w.Body.String())
	}
	raw := "/media/archive/" + f.ID + "/" + peer + "/0"
	w = adminCall(s, cookie, "GET", raw, "")
	if w.Code != 401 {
		t.Fatal("cookie authorized raw archive")
	}
	sum := sha256.Sum256([]byte("media-token"))
	if err = s.DB.InsertToken(&dbx.APIToken{ID: "media-token", Name: "test", Prefix: "test", TokenHash: hex.EncodeToString(sum[:]), Scope: "admin", OwnerID: dbx.AdminOwner}); err != nil {
		t.Fatal(err)
	}
	w = uploaderCall(s, "GET", raw, "media-token", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatal("raw encryption representation changed", w.Code)
	}
	replicas, _ := s.DB.MediaReplicas(f.ID)
	for _, replica := range replicas {
		if replica.InstanceID == peer && int64(w.Body.Len()) != replica.Parts[0].StoredSize {
			t.Fatal("raw response truncated carrier")
		}
	}
	if err = s.DB.SetPurgeState(f.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	w = uploaderCall(s, "GET", raw, "media-token", nil)
	if w.Code != 404 {
		t.Fatal("raw bytes ignored deletion", w.Code)
	}
}

// Explicitly opt in to an isolated browser fixture. It never reads user data,
// credentials or real provider sessions, and is absent from normal test runs.
func TestMediaBrowserFixture(t *testing.T) {
	if os.Getenv("PIMG_MEDIA_BROWSER_FIXTURE") != "1" {
		t.Skip("isolated browser fixture")
	}
	s := interactionServer(t)
	if err := s.DB.EnsureAdminWithSetup("qa", adminPasswordHash(t, "media-qa-fixture"), false); err != nil {
		t.Fatal(err)
	}
	config := store.InstanceConfig{Directory: filepath.Join(t.TempDir(), "secondary"), Encrypt: true, VideoPartMB: 256, Priority: 20}
	peer, err := s.Svc.SaveStorageInstance("", "加密备用归档", "local", config, true)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Svc.StoragePolicy()
	p.Archives = []string{"local", peer}
	if err = s.Svc.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(interactionPNG(t)), "预览验证.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}

	if sample := os.Getenv("PIMG_MEDIA_HEIC_FIXTURE"); sample != "" {
		data, err := os.ReadFile(sample)
		if err != nil {
			t.Fatal(err)
		}
		s.Svc.SetDynamic(-1, -1, -1, []string{"png", "heic", "mp4", "mov", "ts", "avi", "flv", "m2ts", "jpeg"})
		f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(data), "HEIC 查看验证.heic", "heic", sql.NullInt64{}, nil, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Svc.RunArchive(context.Background(), f.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.Svc.RunArchiveSync(context.Background(), f.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Eng.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Eng.Stop()
	listener, err := net.Listen("tcp", "127.0.0.1:29019")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__test/shutdown", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stopped:
		default:
			close(stopped)
		}
		w.WriteHeader(204)
	})
	mux.Handle("/", s.Handler())
	server := &http.Server{Handler: mux}
	defer server.Close()
	go server.Serve(listener)
	t.Log("browser fixture http://127.0.0.1:29019 login qa / media-qa-fixture")
	select {
	case <-stopped:
	case <-time.After(15 * time.Minute):
		t.Fatal("browser fixture timed out")
	}
	s.Svc.StopMediaLoads()
}
