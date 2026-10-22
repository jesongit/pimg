package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"pimg/internal/r2x"
)

func TestPublicTempRoundTripAndApplicationOwnedExpiry(t *testing.T) {
	svc, _, mock := newTestEnv(t)
	cfg := &svc.Cfg.R2
	cfg.TempPublicBaseURL = "https://temp.example.com"
	cfg.TempCleanupBucket = "control"
	cfg.CDNZoneID = "zone"
	cfg.CDNAPIToken = "token"
	svc.R2 = r2x.New(mock.Base, "k", "s", cfg.TempBucket, cfg.PermBucket, cfg.PublicBaseURL, "zone", "token", r2x.WithPublicTemp(cfg.TempPublicBaseURL, "control", "", 600))
	ctx := context.Background()
	plain := bigPNG(t)
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "original.png", "png", nullInt(0), nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = svc.DB.GetFile(f.ID)
	for _, kind := range []string{"original"} {
		if err = svc.RunTempMaterialize(ctx, f.ID, kind); err != nil {
			t.Fatal(err)
		}
		obj, err := svc.DB.LiveTempObject(f.ID, kind, time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		if !obj.ReadyAt.Valid {
			t.Fatal("successful public temporary upload was not confirmed")
		}
		tv, err := svc.GetTempView(ctx, f, kind)
		if err != nil || tv.Status != "ready" || tv.URL != cfg.TempPublicBaseURL+"/"+obj.ObjectKey || tv.ExpiresAt != obj.ExpiresAt {
			t.Fatal("wrong public temporary URL/expiry", tv, err)
		}
		got := fetchTempObject(t, fmt.Sprintf("%s/%s/%s", mock.Base, cfg.TempBucket, obj.ObjectKey))
		if kind == "original" && sha256Hex(got) != f.SHA256 {
			t.Fatal("public original hash mismatch")
		}
		if len(got) == 0 {
			t.Fatal("empty temporary copy")
		}

		// Advance only the isolated database; pimg owns delayed cleanup.
		if _, err = svc.DB.Exec("UPDATE temp_objects SET expires_at=? WHERE id=?", time.Now().Unix()-1, obj.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := svc.CleanupExpiredTemp(ctx); err != nil || n != 1 {
			t.Fatal("local index reconciliation failed", n, err)
		}
		response, err := http.Get(mock.Base + "/" + cfg.TempBucket + "/" + obj.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("expired temporary source was not deleted by pimg")
		}

		if err = svc.RunTempMaterialize(ctx, f.ID, kind); err != nil {
			t.Fatal(err)
		}
		next, err := svc.DB.LiveTempObject(f.ID, kind, time.Now().Unix())
		if err != nil || next.ObjectKey == obj.ObjectKey {
			t.Fatal("expired public path reused", err)
		}
	}
	if _, err = svc.Verify(ctx, f.ID); err != nil {
		t.Fatal("cold original restore verification failed", err)
	}
	// Explicit deletion must still delete and purge managed copies immediately;
	// scheduled expiry uses the same durable pimg records.
	live, err := svc.DB.FileTempObjects(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	purged := map[string]bool{}
	svc.R2.SetCDNHTTPClient(&http.Client{Transport: publicTempPurgeTransport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Files []string `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		for _, url := range body.Files {
			purged[url] = true
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"success":true}`))}, nil
	})})
	cancelFixtureReferences(t, svc, f.ID)
	if err = svc.RunPurgeFile(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	for _, obj := range live {
		if !purged[cfg.TempPublicBaseURL+"/"+obj.ObjectKey] {
			t.Fatal("explicit purge skipped CDN", obj.ObjectKey)
		}
		response, err := http.Get(mock.Base + "/" + cfg.TempBucket + "/" + obj.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("explicit purge left a public source")
		}
	}
}

type publicTempPurgeTransport func(*http.Request) (*http.Response, error)

func (f publicTempPurgeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fetchTempObject(t *testing.T, url string) []byte {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("temporary object not available: %d %v", r.StatusCode, err)
	}
	return b
}
