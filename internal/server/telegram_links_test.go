package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/config"
	"pimg/internal/dbx"
	"pimg/internal/devmock"
	"pimg/internal/store"
	"pimg/internal/tgx"
)

const telegramLinkFixtureToken = "telegram-share-fixture-secret"

func telegramLinkServer(t *testing.T) *Server {
	t.Helper()
	s := interactionServer(t)
	mock, err := devmock.StartTG(s.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mock.Close() })
	s.Cfg.Telegram = config.Telegram{APIBase: mock.Base, BotToken: telegramLinkFixtureToken, ChannelID: "-100fixture", ChunkMB: 1}
	svc, err := store.New(s.Cfg, s.DB, s.Svc.Master, tgx.New(mock.Base, telegramLinkFixtureToken, "-100fixture"), nil, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStorage("telegram", "local"); err != nil {
		t.Fatal(err)
	}
	s.Svc = svc
	sum := sha256.Sum256([]byte("telegram-links-auth"))
	if err := s.DB.InsertToken(&dbx.APIToken{ID: "tg-links", Name: "TG links test", TokenHash: fmt.Sprintf("%x", sum)}); err != nil {
		t.Fatal(err)
	}
	return s
}

func archiveTelegramFixture(t *testing.T, s *Server, direct bool) (*dbx.File, []byte) {
	t.Helper()
	plain := interactionPNG(t)
	f, err := legacyUploadFixture(s.Svc,context.Background(), bytes.NewReader(plain), "telegram.png", "png", sql.NullInt64{}, nil, 0, direct)
	if err != nil {
		t.Fatal(err)
	}
	if direct {
		err = s.Svc.RunDirectArchive(context.Background(), f.ID)
	} else {
		err = s.Svc.RunArchive(context.Background(), f.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, plain
}

func storedTelegramBytes(t *testing.T, s *Server, f *dbx.File, kind string) []byte {
	t.Helper()
	chunkKind := map[string]string{"original": "data", "thumb": "thumb", "blur": "bthumb"}[kind]
	chunks, err := s.DB.ListChunks(f.ID, chunkKind)
	if err != nil || len(chunks) == 0 {
		t.Fatal("stored TG chunks missing", kind, err)
	}
	var result bytes.Buffer
	for _, chunk := range chunks {
		stream, err := s.Svc.TG.Download(context.Background(), chunk.TgFileID.String)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		payload := data
		if f.Storage != "direct" {
			_, _, decoded, e := carrier.DecodePart(data)
			if e != nil {
				t.Fatal(e)
			}
			payload = decoded
		}
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(payload)) != chunk.SHA256 {
			t.Fatal("mock TG stored chunk hash differs from its record", err)
		}
		result.Write(data)
	}
	return result.Bytes()
}

func TestTelegramArchiveLinksRequireTokenAndReturnStoredBytes(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "encrypted"
		if direct {
			name = "plaintext"
		}
		t.Run(name, func(t *testing.T) {
			s := telegramLinkServer(t)
			if direct {
				s.Svc.SetPublicBaseURL("https://site.example.invalid/")
			}
			f, plain := archiveTelegramFixture(t, s, direct)
			if s.fileJSON(f)["encrypted"] != !direct {
				t.Fatal("encrypted flag does not follow the archived storage format")
			}
			handler := s.Handler()
			call := func(method, address, rangeHeader string, auth bool) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, address, nil)
				if rangeHeader != "" {
					r.Header.Set("Range", rangeHeader)
				}
				if auth {
					r.Header.Set("Authorization", "Bearer telegram-links-auth")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			counts := func() [2]int {
				t.Helper()
				var result [2]int
				if err := s.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&result[0]); err != nil {
					t.Fatal(err)
				}
				if err := s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects").Scan(&result[1]); err != nil {
					t.Fatal(err)
				}
				return result
			}
			before := counts()
			w := call("GET", "/api/admin/files/"+f.ID+"/links", "", true)
			var links store.AvailableLinks
			if err := json.Unmarshal(w.Body.Bytes(), &links); err != nil || w.Code != 200 {
				t.Fatal("archive links could not be listed", w.Code, w.Body.String(), err)
			}
			if after := counts(); after != before {
				t.Fatal("enumerating Telegram links created hot copies or background tasks", before, after)
			}
			archive := map[string]store.AvailableLink{}
			for _, link := range links.Items {
				if strings.Contains(link.URL, telegramLinkFixtureToken) || strings.Contains(link.URL, "/api/admin/") || strings.Contains(link.URL, "/file/bot") {
					t.Fatal("a Telegram credential or admin content URL leaked")
				}
				if link.Role != "archive" {
					continue
				}
				archive[link.Kind] = link
				parsed, err := url.Parse(link.URL)
				if err != nil || parsed.RawQuery != "" || link.Access != "api_token" || link.Representation != "stored" || link.Backend != "telegram" || link.ExpiresAt != 0 || link.ObjectExpiresAt != 0 {
					t.Fatalf("wrong token-only TG archive metadata: %#v", link)
				}
				if direct && !strings.HasPrefix(link.URL, "https://site.example.invalid/media/tg/") || !direct && !strings.HasPrefix(link.URL, "/media/tg/") {
					t.Fatalf("configured site origin was ignored: %s", link.URL)
				}
			}
			if len(archive) != 1 {
				t.Fatalf("exactly one original archive link expected: %#v", archive)
			}
			original := archive["original"].URL
			cookieValue := "valid-tg-session"
			sessionSum := sha256.Sum256([]byte(cookieValue))
			if err := s.DB.InsertSession(fmt.Sprintf("%x", sessionSum), time.Now().Add(time.Hour).Unix(), "fixture", ""); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "HEAD"} {
				for _, mode := range []string{"none", "wrong-token", "session-only", "query-token", "signature-only"} {
					address := original
					if mode == "query-token" {
						address += "?token=telegram-links-auth"
					} else if mode == "signature-only" {
						address += "?signature=not-an-authorization-token"
					}
					r := httptest.NewRequest(method, address, nil)
					if mode == "wrong-token" {
						r.Header.Set("Authorization", "Bearer incorrect")
					} else if mode == "session-only" {
						r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookieValue})
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != 401 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || w.Header().Get("WWW-Authenticate") == "" {
						t.Fatalf("%s %s bypassed token-only auth or lacked auth/cache headers: %d", method, mode, w.Code)
					}
				}
			}
			adminRequest := httptest.NewRequest("GET", "/api/admin/files/"+f.ID, nil)
			adminRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookieValue})
			adminResponse := httptest.NewRecorder()
			handler.ServeHTTP(adminResponse, adminRequest)
			if adminResponse.Code != 200 {
				t.Fatal("existing cookie-based admin authentication regressed")
			}

			expected := map[string][]byte{}
			for _, kind := range []string{"original"} {
				expected[kind] = storedTelegramBytes(t, s, f, kind)
			}
			if direct && !bytes.Equal(expected["original"], plain) || !direct && (bytes.Equal(expected["original"], plain) || len(expected["original"]) <= len(plain)) {
				t.Fatal("TG test fixture did not store the expected plaintext/ciphertext format")
			}
			// Raw access must work even when file keys are corrupt and the service
			// has no master key. Neither condition permits decryption.
			if _, err := s.DB.Exec("UPDATE files SET key_wrapped=X'00',key_nonce=X'00' WHERE id=?", f.ID); err != nil {
				t.Fatal(err)
			}
			master := s.Svc.Master
			s.Svc.Master = nil
			for _, kind := range []string{"original"} {
				want := expected[kind]
				for _, test := range []struct {
					method, rangeHeader string
					status              int
					expected            []byte
				}{
					{"GET", "", 200, want},
					{"GET", "bytes=0-15", 206, want[:16]},
					{"GET", "bytes=16-31", 206, want[16:32]},
					{"GET", "bytes=-12", 206, want[len(want)-12:]},
					{"HEAD", "", 200, nil},
				} {
					w := call(test.method, archive[kind].URL, test.rangeHeader, true)
					if w.Code != test.status || test.expected != nil && !bytes.Equal(w.Body.Bytes(), test.expected) {
						t.Fatalf("%s %s %q returned wrong stored bytes/status: %d", kind, test.method, test.rangeHeader, w.Code)
					}
					if test.method == "HEAD" && (w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(want))) {
						t.Fatal("raw HEAD used plaintext size or returned a body")
					}
					mime := "image/jpeg"
					if kind == "original" {
						mime = f.Mime
					}
					if !direct {
						mime = "application/octet-stream"
						if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(w.Header().Get("Content-Disposition"), ".enc") {
							t.Fatal("encrypted stored bytes were presented as plaintext media")
						}
					}
					if w.Header().Get("Content-Type") != mime || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
						t.Fatal("wrong raw archive MIME or cache policy")
					}
				}
			}
			stillAvailable, err := s.Svc.FileLinks(context.Background(), f.ID)
			if err != nil || len(stillAvailable.Items) < 1 {
				t.Fatal("raw archive link enumeration incorrectly requires a master key", err)
			}
			s.Svc.Master = master
			if _, err := s.DB.Exec("UPDATE files SET key_wrapped=?,key_nonce=? WHERE id=?", f.KeyWrapped, f.KeyNonce, f.ID); err != nil {
				t.Fatal(err)
			}
			for _, change := range []struct{ sql, restore string }{
				{"UPDATE files SET cold_backend='local' WHERE id=?", "UPDATE files SET cold_backend='telegram' WHERE id=?"},
				{"UPDATE files SET archive_state='failed' WHERE id=?", "UPDATE files SET archive_state='done' WHERE id=?"},
				{"UPDATE files SET archive_state='running' WHERE id=?", "UPDATE files SET archive_state='done' WHERE id=?"},
				{"UPDATE files SET purge_state='pending' WHERE id=?", "UPDATE files SET purge_state=NULL WHERE id=?"},
			} {
				if _, err := s.DB.Exec(change.sql, f.ID); err != nil {
					t.Fatal(err)
				}
				if w := call("GET", original, "", true); w.Code != 404 {
					t.Fatal("unready/non-TG/deleting archive remained readable", w.Code)
				}
				if _, err := s.DB.Exec(change.restore, f.ID); err != nil {
					t.Fatal(err)
				}
			}
			if after := counts(); after != before {
				t.Fatal("reading raw TG bytes created a serving copy or background task")
			}
			fresh, _ := s.DB.GetFile(f.ID)
			if fresh.Published != f.Published {
				t.Fatal("issuing or reading TG links changed publication")
			}
			if !direct {
				if err := s.Svc.RunPublish(context.Background(), f.ID, false); !errors.Is(err, dbx.ErrPermanentEncrypted) {
					t.Fatal("encrypted archives must not become permanent publications", err)
				}
				fresh, _ := s.DB.GetFile(f.ID)
				if s.fileJSON(fresh)["encrypted"] != true || fresh.Published {
					t.Fatal("rejected publication changed the archive encryption or publication state")
				}
			}
			if _, err := s.DB.Exec("DELETE FROM file_chunks WHERE file_id=? AND kind='data'", f.ID); err != nil {
				t.Fatal(err)
			}
			if w := call("GET", original, "", true); w.Code != 404 {
				t.Fatal("missing original Telegram locators remained readable", w.Code)
			}
			if err := s.DB.RevokeToken("tg-links"); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "HEAD"} {
				if w := call(method, original, "", true); w.Code != 401 {
					t.Fatal("a revoked existing API token still read stored bytes", method, w.Code)
				}
			}
		})
	}
}

type telegramLinkTransport func(*http.Request) (*http.Response, error)

func (f telegramLinkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramArchiveRequestCancellationReleasesFileLock(t *testing.T) {
	s := telegramLinkServer(t)
	f, _ := archiveTelegramFixture(t, s, false)
	address := "/media/tg/" + f.ID + "/original"
	started := make(chan struct{}, 1)
	s.Svc.TG.SetHTTPClient(&http.Client{Transport: telegramLinkTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/file/") {
			started <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return http.DefaultTransport.RoundTrip(r)
	})})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest("GET", address, nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer telegram-links-auth")
		s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("archive request did not begin its source read")
	}
	blocked, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := s.Svc.RunPurgeFile(blocked, f.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deletion did not wait for the active raw response", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled archive response did not close its reader")
	}
	available, finish := context.WithTimeout(context.Background(), time.Second)
	defer finish()
	if _, err := s.Svc.FileLinks(available, f.ID); err != nil {
		t.Fatal("cancelled response left the file locked", err)
	}
	refs, err := s.DB.FileTaskReferences(f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range refs {
		if err = s.DB.RequestTaskCancel(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Svc.RunPurgeFile(available, f.ID); err != nil {
		t.Fatal("deletion did not resume after response cancellation", err)
	}
}

func TestTelegramArchiveUpstreamErrorsDoNotDiscloseProviderDetails(t *testing.T) {
	s := telegramLinkServer(t)
	f, _ := archiveTelegramFixture(t, s, false)
	s.Svc.TG.SetHTTPClient(&http.Client{Transport: telegramLinkTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://telegram.invalid/file/bot" + telegramLinkFixtureToken + "/private")
	})})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/media/tg/"+f.ID+"/original", nil)
	r.Header.Set("Authorization", "Bearer telegram-links-auth")
	s.Handler().ServeHTTP(w, r)
	if w.Code != 500 || strings.Contains(w.Body.String(), telegramLinkFixtureToken) || strings.Contains(w.Body.String(), "telegram.invalid") {
		t.Fatal("raw archive failure exposed upstream credentials or endpoint", w.Code, w.Body.String())
	}
}

func TestTelegramRawRangesAcrossStoredChunksAndProbeBoundary(t *testing.T) {
	s := telegramLinkServer(t)
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 512))
	rand.New(rand.NewSource(19)).Read(img.Pix)
	for index := 3; index < len(img.Pix); index += 4 {
		img.Pix[index] = 255
	}
	var encoded bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	f, err := legacyUploadFixture(s.Svc,context.Background(), bytes.NewReader(encoded.Bytes()), "segments.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Svc.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := s.DB.ListChunks(f.ID, "data")
	if err != nil || len(chunks) != 1 {
		t.Fatal("fixture must use one complete Telegram original", err)
	}
	stored := storedTelegramBytes(t, s, f, "original")
	if int64(len(stored)) <= f.Size || len(stored) <= 64<<10 {
		t.Fatal("fixture must contain stored cipher overhead and exceed the probe prefix")
	}
	s.Svc.Master = nil
	handler := s.Handler()
	request := func(method, header string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/media/tg/"+f.ID+"/original", nil)
		r.Header.Set("Authorization", "Bearer telegram-links-auth")
		if header != "" {
			r.Header.Set("Range", header)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), stored) {
		t.Fatal("full multi-chunk raw response differs from actual TG stored ciphertext", w.Code)
	}
	if w := request("HEAD", ""); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(stored)) {
		t.Fatal("multi-chunk HEAD used plaintext length", w.Code)
	}
	for _, offset := range []int64{(64 << 10) - 16, chunks[0].Size - 16, int64(len(stored)) - 48} {
		end := offset + 47
		w := request("GET", fmt.Sprintf("bytes=%d-%d", offset, end))
		if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), stored[offset:end+1]) || w.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, end, len(stored)) {
			t.Fatal("raw range crossed a probe/chunk boundary incorrectly", offset, w.Code)
		}
	}
}
