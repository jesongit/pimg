package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/r2x"
	"pimg/internal/storage"
)

func insertLinkTemp(t *testing.T, s *Service, fileID, id, backend, kind string, expires int64, confirmed, deleted bool) *dbx.TempObject {
	t.Helper()
	plain := []byte("temporary bytes for " + id)
	obj := &dbx.TempObject{ID: id, FileID: fileID, Kind: kind, Backend: backend, ObjectKey: "tmp/links/" + id, Size: sql.NullInt64{Int64: int64(len(plain)), Valid: true}, ExpiresAt: expires}
	b, err := s.hot(backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PutObject(context.Background(), storage.Temporary, obj.ObjectKey, bytes.NewReader(plain), int64(len(plain)), "image/png", "no-store"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.InsertTempObject(obj); err != nil {
		t.Fatal(err)
	}
	if confirmed {
		if err := s.DB.MarkTempReady(obj.ID); err != nil {
			t.Fatal(err)
		}
	}
	if deleted {
		if err := s.DB.MarkTempDeleted(obj.ID); err != nil {
			t.Fatal(err)
		}
	}
	return obj
}

func assertLinkExpiry(t *testing.T, link AvailableLink) {
	t.Helper()
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	var signedExpiry int64
	if link.Backend == "local" {
		signedExpiry, err = strconv.ParseInt(u.Query().Get("expires"), 10, 64)
	} else {
		var signedAt time.Time
		signedAt, err = time.Parse("20060102T150405Z", u.Query().Get("X-Amz-Date"))
		if err == nil {
			var seconds int64
			seconds, err = strconv.ParseInt(u.Query().Get("X-Amz-Expires"), 10, 64)
			signedExpiry = signedAt.Unix() + seconds
		}
	}
	if err != nil || signedExpiry != link.ExpiresAt || link.ExpiresAt >= link.ObjectExpiresAt || link.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiry disagrees with signed URL or retention: %#v, %v", link, err)
	}
}

type linkSigningFailure struct{ storage.Hot }

type linkHeadCounter struct {
	storage.Hot
	calls *int
}

func (b linkHeadCounter) HeadSize(context.Context, string, string) (int64, error) {
	*b.calls++
	return 0, errors.New("remote HEAD must not run when reading recorded links")
}

func TestRecordedFileLinksReuseDurableCopiesWithoutRemoteChecks(t *testing.T) {
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	f := unpublishedPlainArchive(t, s, plain.Bytes())
	ctx := context.Background()
	if err := s.RunPublish(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	insertLinkTemp(t, s, f.ID, "recorded-ready", "local", "original", now+7200, true, false)
	insertLinkTemp(t, s, f.ID, "recorded-pending", "local", "thumb", now+7200, false, false)
	insertLinkTemp(t, s, f.ID, "recorded-expired", "local", "thumb", now, true, false)
	insertLinkTemp(t, s, f.ID, "recorded-deleted", "local", "thumb", now+7200, true, true)
	objects, err := s.DB.LiveTempObjects([]string{f.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Switching current providers must retain all addresses on recorded local.
	if err := s.SetStorage("local", "r2"); err != nil {
		t.Fatal(err)
	}
	heads := 0
	s.hotBackends["local"] = linkHeadCounter{Hot: s.localHot, calls: &heads}
	for i := 0; i < 2; i++ {
		links := s.RecordedFileLinks(ctx, f, objects[f.ID]...)
		if len(links.Items) != 3 || len(links.Warnings) != 0 || heads != 0 {
			t.Fatalf("reading saved links performed provider checks or lost copies: %#v, heads=%d", links, heads)
		}
		for _, link := range links.Items {
			if link.Backend != "local" && !(link.Kind == "thumb" && link.Backend == "pimg") {
				t.Fatalf("link moved to selected provider: %#v", link)
			}
			if link.Role == "temporary" {
				if link.ID != "temporary:recorded-ready" {
					t.Fatal("unconfirmed, deleted or expired copy exposed", link)
				}
				assertLinkExpiry(t, link)
			}
		}
	}
	// Explicit refresh keeps the existing live verification semantics.
	if _, err := s.FileLinks(ctx, f.ID); err != nil || heads == 0 {
		t.Fatal("explicit refresh stopped checking remote availability", heads, err)
	}
	if err := s.DB.SetPublished(f.ID, false, "", ""); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	links := s.RecordedFileLinks(ctx, f, objects[f.ID]...)
	if len(links.Items) != 1 || links.Items[0].Role != "temporary" {
		t.Fatal("revoked publication still returned links", links)
	}
	if err := s.DB.SetPurgeState(f.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	if links := s.RecordedFileLinks(ctx, f, objects[f.ID]...); len(links.Items) != 0 {
		t.Fatal("deletion handed out saved links", links)
	}
}

func (linkSigningFailure) PresignGet(context.Context, string, string, time.Duration, string) (string, error) {
	return "", errors.New("private-provider-error-must-not-leak")
}

func TestFileLinksEnumerateExistingCopiesAcrossRecordedBackends(t *testing.T) {
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	f := unpublishedPlainArchive(t, s, plain.Bytes())
	ctx := context.Background()
	if err := s.RunPublish(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, obj := range []struct {
		id, backend, kind string
		expires           int64
		confirmed, gone   bool
	}{
		{"local-original", "local", "original", now + 7200, true, false},
		{"short-local-original", "local", "original", now + 120, true, false},
		{"r2-original", "r2", "original", now + 7200, true, false},
		{"r2-thumb", "r2", "thumb", now + 7200, true, false},
		{"unconfirmed", "r2", "original", now + 7200, false, false},
		{"expired", "local", "original", now, true, false},
		{"deleted", "local", "thumb", now + 7200, true, true},
	} {
		insertLinkTemp(t, s, f.ID, obj.id, obj.backend, obj.kind, obj.expires, obj.confirmed, obj.gone)
	}
	// Current choices cannot redirect any recorded publication or temporary copy.
	if err := s.SetStorage("local", "r2"); err != nil {
		t.Fatal(err)
	}
	counts := func() [2]int {
		t.Helper()
		var out [2]int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects").Scan(&out[0]); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&out[1]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := counts()
	links, err := s.FileLinks(ctx, f.ID)
	if err != nil || len(links.Items) != 6 || len(links.Warnings) != 0 {
		t.Fatalf("unexpected links: %#v, %v", links, err)
	}
	if after := counts(); after != before {
		t.Fatal("link enumeration created temporary copies or tasks", before, after)
	}
	want := map[string]string{"published:original": "local", "published:thumb": "pimg", "temporary:local-original": "local", "temporary:short-local-original": "local", "temporary:r2-original": "r2", "temporary:r2-thumb": "r2"}
	for _, link := range links.Items {
		if want[link.ID] != link.Backend {
			t.Fatalf("wrong copy or backend: %#v", link)
		}
		delete(want, link.ID)
		if link.Role == "temporary" {
			assertLinkExpiry(t, link)
		} else if link.ExpiresAt != 0 || link.ObjectExpiresAt != 0 || !strings.Contains(link.URL, "/media/permanent/") && !(link.Backend == "pimg" && strings.Contains(link.URL, "/previews/")) {
			t.Fatalf("permanent link was rerouted or incorrectly expires: %#v", link)
		}
	}
	if len(want) != 0 {
		t.Fatal("missing copy links", want)
	}

	// Missing covers must not be advertised just because the archive has covers.
	if err := s.localHot.Delete(ctx, storage.Permanent, thumbKey(f)); err != nil {
		t.Fatal(err)
	}
	s.hotBackends["local"] = linkSigningFailure{Hot: s.localHot}
	links, err = s.FileLinks(ctx, f.ID)
	if err != nil || len(links.Items) != 4 || len(links.Warnings) != 1 {
		t.Fatalf("a failed copy hid unrelated links: %#v, %v", links, err)
	}
	for _, link := range links.Items {
		if link.Role == "temporary" && link.Backend == "local" {
			t.Fatalf("unavailable copy was returned: %#v", link)
		}
	}
	encoded, _ := json.Marshal(links)
	if strings.Contains(string(encoded), "private-provider-error") {
		t.Fatal("raw provider errors leaked through warnings")
	}
	if err := s.DB.SetPurgeState(f.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileLinks(ctx, f.ID); !errors.Is(err, dbx.ErrDeleting) {
		t.Fatal("deleting file handed out new links", err)
	}
}

func TestFileLinksPrivateArchiveDoesNotExposeAuthenticatedOrTelegramURLs(t *testing.T) {
	s, f := readinessFixture(t)
	if _, err := s.DB.Exec("UPDATE files SET cold_backend='telegram' WHERE id=?", f.ID); err != nil {
		t.Fatal(err)
	}
	links, err := s.FileLinks(context.Background(), f.ID)
	if err != nil || links.Items == nil || len(links.Items) != 0 {
		t.Fatalf("private archive gained shareable links: %#v, %v", links, err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM temp_objects").Scan(&count); err != nil || count != 0 {
		t.Fatal("enumerating private links materialized plaintext", count, err)
	}
}

func TestPublicTempFileLinksUseObjectExpiry(t *testing.T) {
	s, f := readinessFixture(t)
	s.Cfg.R2.TempPublicBaseURL = "https://temp.example.com"
	s.Cfg.R2.TempCleanupBucket = "control"
	s.Cfg.R2.CDNZoneID, s.Cfg.R2.CDNAPIToken = "zone", "token"
	cfg := s.Cfg.R2
	s.R2 = r2x.New(cfg.Endpoint, "k", "s", cfg.TempBucket, cfg.PermBucket, cfg.PublicBaseURL, "zone", "token", r2x.WithPublicTemp(cfg.TempPublicBaseURL, "control", "", 600))
	if err := s.SetStorage("local", "r2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunTempMaterialize(context.Background(), f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	links, err := s.FileLinks(context.Background(), f.ID)
	if err != nil || len(links.Items) != 1 {
		t.Fatalf("public temp link missing: %#v, %v", links, err)
	}
	link := links.Items[0]
	if link.ExpiresAt != link.ObjectExpiresAt || link.ExpiresAt <= time.Now().Unix() || !strings.HasPrefix(link.URL, "https://temp.example.com/tmp/v1/") || strings.Contains(link.URL, "?") {
		t.Fatalf("public temp expiry differs from stored retention: %#v", link)
	}
}
