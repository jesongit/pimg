package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func localHTTP(t *testing.T, s *Service) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /media/{scope}/{key...}", s.ServeLocalMedia)
	h := httptest.NewServer(mux)
	t.Cleanup(h.Close)
	s.SetPublicBaseURL(h.URL)
	return h
}
func checkHTTP(t *testing.T, address string, code int, want []byte) {
	t.Helper()
	r, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != code {
		t.Fatalf("status %d want %d: %s", r.StatusCode, code, b)
	}
	if want != nil && !bytes.Equal(b, want) {
		t.Fatalf("HTTP body differs: %d != %d", len(b), len(want))
	}
}

func TestStorageCombinations(t *testing.T) {
	plain := bigPNG(t)
	for _, cold := range []string{"local", "telegram"} {
		for _, hot := range []string{"local", "r2"} {
			for _, direct := range []bool{false, true} {
				name := cold + "/" + hot
				if direct {
					name += "/direct"
				}
				t.Run(name, func(t *testing.T) {
					s, _, _ := newTestEnv(t)
					localHTTP(t, s)
					if err := s.SetStorage(cold, hot); err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					f, err := s.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "photo.png", "png", sql.NullInt64{}, nil, 0, direct)
					if err != nil {
						t.Fatal(err)
					}
					if f.ColdBackend != cold {
						t.Fatalf("wrong archive provider: %s", f.ColdBackend)
					}
					// A queued upload keeps its original cold provider across a switch.
					otherCold := "local"
					if cold == "local" {
						otherCold = "telegram"
					}
					if err := s.SetStorage(otherCold, hot); err != nil {
						t.Fatal(err)
					}
					if direct {
						err = s.RunDirectArchive(ctx, f.ID)
					} else {
						err = s.RunArchive(ctx, f.ID)
					}
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Verify(ctx, f.ID); err != nil {
						t.Fatal(err)
					}
					f, _ = s.DB.GetFile(f.ID)
					if !f.HasThumb || f.HasBThumb {
						t.Fatal("local preview missing or standalone blur still exists")
					}
					for _, kind := range []string{"original"} {
						if err := s.RunTempMaterialize(ctx, f.ID, kind); err != nil {
							t.Fatal(err)
						}
						tv, err := s.GetTempView(ctx, f, kind)
						if err != nil {
							t.Fatal(err)
						}
						var expected []byte
						if kind == "original" {
							expected = plain
						}
						checkHTTP(t, tv.URL, 200, expected)
					}
					if !direct {
						if err := s.RunPublish(ctx, f.ID, false); !errors.Is(err, dbx.ErrPermanentEncrypted) {
							t.Fatal("encrypted archive must refuse permanent publication", err)
						}
						if latest, err := s.DB.GetFile(f.ID); err != nil || latest.Published {
							t.Fatal("encrypted archive was published", err)
						}
					} else {
						if err := s.RunPublish(ctx, f.ID, false); err != nil {
							t.Fatal(err)
						}
						f, _ = s.DB.GetFile(f.ID)
						if f.HotBackend != hot {
							t.Fatal("wrong publication provider")
						}
						url := s.PublicURL(f)
						checkHTTP(t, url, 200, plain)
						if preview, e := s.PreviewThumbnail(ctx, f); e != nil || len(preview) == 0 {
							t.Fatal("local published preview", e)
						}
						if s.BThumbURL(f) != "" {
							t.Fatal("standalone blur URL retained")
						}
						// Switching the default cannot redirect an existing publication.
						otherHot := "local"
						if hot == "local" {
							otherHot = "r2"
						}
						if err := s.SetStorage(otherCold, otherHot); err != nil {
							t.Fatal(err)
						}
						if s.PublicURL(f) != url {
							t.Fatal("existing URL changed")
						}
						checkHTTP(t, url, 200, plain)
						if err := s.RunUnpublish(ctx, f.ID); err != nil {
							t.Fatal(err)
						}
						checkHTTP(t, url, 404, nil)
					}
					if _, err := s.Verify(ctx, f.ID); err != nil {
						t.Fatal("revoke removed archive", err)
					}
					cancelFixtureReferences(t, s, f.ID)
					if err := s.RunPurgeFile(ctx, f.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.DB.GetFile(f.ID); !errors.Is(err, dbx.ErrNotFound) {
						t.Fatal("purge retained index", err)
					}
				})
			}
		}
	}
}

func TestLocalPersistenceExpiryAndPrivateBoundary(t *testing.T) {
	s, _, _ := newTestEnv(t)
	localHTTP(t, s)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := bigPNG(t)
	f, err := s.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	ct, err := os.ReadFile(filepath.Join(s.Cfg.Storage.LocalColdDir, filepath.FromSlash(chunks[0].ObjectKey)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain[:32]) {
		t.Fatal("private cold archive contains plaintext")
	}
	if err := s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	tv, err := s.GetTempView(ctx, f, "original")
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	checkHTTP(t, strings.Split(tv.URL, "?")[0], 403, nil)
	checkHTTP(t, tv.URL+"x", 403, nil)
	expired := strings.Split(tv.URL, "?")[0] + "?expires=1&signature=anything"
	checkHTTP(t, expired, 403, nil)
	checkHTTP(t, s.localHot.PublicURL(origKey(f)), 404, nil)
	if err := s.RunPublish(ctx, f.ID, false); !errors.Is(err, dbx.ErrPermanentEncrypted) {
		t.Fatal("encrypted archive must refuse permanent publication", err)
	}
	publicFile := unpublishedPlainArchive(t, s, plain)
	if err := s.RunPublish(ctx, publicFile.ID, false); err != nil {
		t.Fatal(err)
	}
	publicFile, _ = s.DB.GetFile(publicFile.ID)
	// Reconstruct the service from the same disk and index without remote state.
	s.Cfg.Storage.Cold, s.Cfg.Storage.Hot = "local", "local"
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := dbx.Open(filepath.Join(s.Cfg.Paths.DataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted, err := New(s.Cfg, db, s.Master, nil, nil, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Verify(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	restarted.SetPublicBaseURL(s.PublicBaseURL())
	h := localHTTP(t, restarted)
	checkHTTP(t, h.URL+strings.TrimPrefix(tv.URL, strings.Split(tv.URL, "/media/")[0]), 200, plain)
	checkHTTP(t, restarted.PublicURL(publicFile), 200, plain)
	request, _ := http.NewRequest(http.MethodGet, restarted.PublicURL(publicFile), nil)
	request.Header.Set("Range", "bytes=0-31")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	rangeBytes, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusPartialContent || !bytes.Equal(rangeBytes, plain[:32]) {
		t.Fatal("local hot storage does not support range", response.StatusCode, readErr)
	}
	if _, err := db.Exec(`UPDATE temp_objects SET expires_at=1 WHERE id=?`, obj.ID); err != nil {
		t.Fatal(err)
	}
	path := h.URL + "/media/temp/" + obj.ObjectKey
	checkHTTP(t, path+"?"+strings.Split(tv.URL, "?")[1], 404, nil)
	if _, err := restarted.CleanupExpiredTemp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.localHot.HeadSize(ctx, storage.Temporary, obj.ObjectKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired temp still exists", err)
	}
	checkHTTP(t, restarted.PublicURL(publicFile), 200, plain)
	if err := restarted.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	fresh, err := restarted.GetTempView(ctx, f, "original")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fresh.URL, obj.ObjectKey) {
		t.Fatal("expired object path reused")
	}
	checkHTTP(t, fresh.URL, 200, plain)
	checkHTTP(t, path+"?"+strings.Split(tv.URL, "?")[1], 404, nil)
	cancelFixtureReferences(t, restarted, f.ID)
	if err := restarted.RunPurgeFile(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Cfg.Storage.LocalColdDir, filepath.FromSlash(chunks[0].ObjectKey))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive remains after purge", err)
	}
	cancelFixtureReferences(t, restarted, publicFile.ID)
	if err := restarted.RunPurgeFile(ctx, publicFile.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalCarrierImportExport(t *testing.T) {
	s, _, _ := newTestEnv(t)
	ctx := context.Background()
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	f, err := s.receiveLegacyUploadFixture(ctx, bytes.NewReader(bigPNG(t)), "export.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	var buf bytes.Buffer
	if err := s.WriteExport(ctx, f, &buf); err != nil {
		t.Fatal(err)
	}
	_, part, _, err := carrier.DecodePart(buf.Bytes())
	hdr := &carrier.Header{V: 1, File: part.File, Chunks: []carrier.CarrierChunk{{Kind: "data", Idx: 0, Size: part.Size, SHA256: part.SHA256}}}
	if err != nil {
		t.Fatal(err)
	}
	imported, err := s.ImportPNG(buf.Bytes(), sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	if imported.ColdBackend != "local" {
		t.Fatal("import used wrong provider")
	}
	if err := s.RunImport(ctx, imported.ID, hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, imported.ID); err != nil {
		t.Fatal(err)
	}
}
