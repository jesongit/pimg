package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"pimg/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestPrivateContentAuthenticationRangesAndNoPublicCopies(t *testing.T) {
	s := interactionServer(t)
	plain := interactionPNG(t)
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(plain), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	const session = "content-test-session"
	sum := sha256.Sum256([]byte(session))
	s.DB.InsertSession(hex.EncodeToString(sum[:]), time.Now().Add(time.Hour).Unix(), "test", "")
	call := func(method, address, rangeHeader string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, address, nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	address := privateContentURL(f.ID, "original")
	if w := call("GET", address, "", false); w.Code != 401 {
		t.Fatal("private content was served without authentication", w.Code)
	}
	for _, tc := range []struct {
		header string
		status int
		data   []byte
	}{{"", 200, plain}, {"bytes=0-15", 206, plain[:16]}, {"bytes=16-31", 206, plain[16:32]}, {"bytes=-12", 206, plain[len(plain)-12:]}, {"bytes=999999-", 416, nil}} {
		w := call("GET", address, tc.header, true)
		if w.Code != tc.status {
			t.Fatalf("range %q: %d", tc.header, w.Code)
		}
		if tc.data != nil && !bytes.Equal(w.Body.Bytes(), tc.data) {
			t.Fatal("original/range differs")
		}
		if tc.status < 400 && !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("private response is cacheable")
		}
	}
	w := call("GET", privateContentURL(f.ID, "thumb"), "", true)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || w.Body.Len() == 0 {
		t.Fatal("thumbnail failed", w.Code)
	}
	if w := call("GET", privateContentURL(f.ID, "thumb"), "", false); w.Code != 401 {
		t.Fatal("cached thumbnail bypassed authentication")
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects").Scan(&count)
	if count != 0 {
		t.Fatal("streamed reads created plaintext hot copies")
	}
	entries, _ := os.ReadDir(s.Cfg.Paths.TempDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spill-") {
			t.Fatal("streamed read spilled plaintext")
		}
	}
	if err := s.DB.SetPurgeState(f.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", privateContentURL(f.ID, "thumb"), "", true); w.Code != 409 {
		t.Fatal("cached preview ignored deletion state")
	}
}

func TestPrivateContentRejectsTamperedCiphertext(t *testing.T) {
	s := interactionServer(t)
	plain := interactionPNG(t)
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(plain), "tamper.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	s.Svc.ForgetMediaViews(f) // Exercise integrity verification on a cache miss.
	replicas, _ := s.DB.MediaReplicas(f.ID)
	var ref storage.Reference
	_ = json.Unmarshal([]byte(replicas[0].Parts[0].Reference), &ref)
	filename := filepath.Join(s.Cfg.Storage.LocalColdDir, filepath.FromSlash(ref.Key))
	data, err := os.ReadFile(filename)
	if err != nil || len(data) < 2 {
		t.Fatal("missing ciphertext fixture", err)
	}
	data[len(data)-2] ^= 255
	os.WriteFile(filename, data, 0600)
	r := httptest.NewRequest("GET", privateContentURL(f.ID, "original"), nil)
	r.SetPathValue("id", f.ID)
	w := httptest.NewRecorder()
	s.handleContent(w, r)
	if w.Code < 400 {
		t.Fatal("corrupted original was accepted", w.Code)
	}
}

func TestPreviewLinksDoNotEnqueueBehindArchiveTasks(t *testing.T) {
	s := interactionServer(t)
	seedInteractionFile(t, s, "private-file", 1, sql.NullInt64{})
	s.DB.SetThumbInfo("private-file", true, 1, 1, "image/jpeg", 100, false)
	s.DB.EnqueueTask("verify", "private-file", "{}")
	w := httptest.NewRecorder()
	s.handlePreviews(w, httptest.NewRequest("GET", "/api/admin/files/previews?ids=private-file", nil))
	var body struct {
		Previews map[string]struct{ Status, URL string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if p := body.Previews["private-file"]; p.Status != "ready" || p.URL != privateContentURL("private-file", "thumb") {
		t.Fatal("preview waits for background work", body)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE type='temp_materialize'").Scan(&count)
	if count != 0 {
		t.Fatal("preview enqueued a materialization task")
	}
}

func interactionPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 96, 48))
	for i := range img.Pix {
		img.Pix[i] = byte(i*31 + i/251)
	}
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestArchiveReaderStreamingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &archiveReader{ctx: ctx, size: 1024, open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(make([]byte, 1024))), nil }}
	if err := reader.probe(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.Read(make([]byte, 10)); err != context.Canceled {
		t.Fatal("cancelled stream kept reading", err)
	}
	reader.Close()
}

func TestPrivateOriginalAcrossEncryptedSegments(t *testing.T) {
	s := interactionServer(t)
	imageData := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	rand.New(rand.NewSource(7)).Read(imageData.Pix)
	for i := 3; i < len(imageData.Pix); i += 4 {
		imageData.Pix[i] = 255
	}
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&encoded, imageData); err != nil {
		t.Fatal(err)
	}
	plain := encoded.Bytes()
	f, err := s.Svc.ReceiveUpload(context.Background(), bytes.NewReader(plain), "segments.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, (1 << 20) - 60, 2 << 20, len(plain) - 128} {
		r := httptest.NewRequest("GET", privateContentURL(f.ID, "original"), nil)
		r.SetPathValue("id", f.ID)
		r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+127))
		w := httptest.NewRecorder()
		s.handleContent(w, r)
		if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[offset:offset+128]) {
			t.Fatalf("range at %d differs: status %d", offset, w.Code)
		}
	}
	replicas, _ := s.DB.MediaReplicas(f.ID)
	var ref storage.Reference
	_ = json.Unmarshal([]byte(replicas[0].Parts[0].Reference), &ref)
	filename := filepath.Join(s.Cfg.Storage.LocalColdDir, filepath.FromSlash(ref.Key))
	ciphertext, err := os.ReadFile(filename)
	if err != nil || len(ciphertext) < 4 {
		t.Fatal("missing ciphertext fixture", err)
	}
	ciphertext[len(ciphertext)-4] ^= 255
	os.WriteFile(filename, ciphertext, 0600)
	if _, err := s.Svc.Verify(context.Background(), f.ID); err == nil {
		t.Fatal("explicit verification trusted the local cache")
	}
	s.Svc.ForgetMediaViews(f)
	r := httptest.NewRequest("GET", privateContentURL(f.ID, "original"), nil)
	r.SetPathValue("id", f.ID)
	w := httptest.NewRecorder()
	s.handleContent(w, r)
	if w.Body.Len() >= len(plain) {
		t.Fatal("unauthenticated final segment was emitted")
	}
}

func TestVideoOriginalUsesPrivateCacheURL(t *testing.T) {
	s := interactionServer(t)
	seedInteractionFile(t, s, "private-video", 1, sql.NullInt64{})
	s.DB.Exec("UPDATE files SET kind='video' WHERE id='private-video'")
	r := httptest.NewRequest("POST", "/api/admin/files/private-video/temp", strings.NewReader(`{"kind":"original"}`))
	r.SetPathValue("id", "private-video")
	w := httptest.NewRecorder()
	s.handleTempView(w, r)
	var result struct{ Status, URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Status != "ready" || result.URL != privateContentURL("private-video", "original") {
		t.Fatal("video lost its seekable cache path", w.Code, w.Body.String())
	}
}
