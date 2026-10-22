package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/r2x"
	"pimg/internal/storagehttp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type phase1RoundTrip func(*http.Request) (*http.Response, error)

func (f phase1RoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Opt-in provider acceptance. Only a fresh, run-owned prefix is written/deleted.
func TestRealPhase1R2(t *testing.T) {
	if os.Getenv("PIMG_PHASE1_REAL") != "1" {
		t.Skip("opt-in real R2 acceptance")
	}
	cfg, e := config.Load(filepath.Join("..", "..", "config.yaml"))
	if e != nil {
		t.Fatal("cannot load protected provider configuration")
	}
	// Acceptance owns an isolated database and encryption key. Never resolve a
	// relative production key path from the package test working directory.
	master, e := cryptox.NewMaster(cryptox.GenerateMasterKey())
	if e != nil {
		t.Fatal(e)
	}
	transport, e := storagehttp.NewTransport(cfg.Storage.ProxyURL)
	if e != nil {
		t.Fatal(e)
	}
	client := r2x.New(cfg.R2.Endpoint, cfg.R2.AccessKeyID, cfg.R2.SecretAccessKey, cfg.R2.TempBucket, cfg.R2.PermBucket, cfg.R2.PublicBaseURL, "", "", r2x.WithHTTPTransport(transport))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prefix := "pimg-phase1-acceptance/" + ids.New()
	bucket := cfg.R2.PermBucket
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		cursor := ""
		for {
			page, err := client.List(cleanup, bucket, prefix+"/", cursor, 100)
			if err != nil {
				t.Error("owned prefix cleanup list failed")
				return
			}
			for _, o := range page.Objects {
				if err = client.Delete(cleanup, bucket, o.Key); err != nil {
					t.Error("owned prefix cleanup failed")
				}
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	})
	// Exercise conditional deletion before permitting any source organization.
	probe := prefix + "/conditional-probe"
	if e = client.PutObject(ctx, bucket, probe, bytes.NewReader([]byte("before")), 6, "text/plain", ""); e != nil {
		t.Fatal("R2 probe write failed")
	}
	version, e := client.ObjectVersion(ctx, bucket, probe)
	if e != nil {
		t.Fatal("R2 probe HEAD failed")
	}
	if e = client.PutObject(ctx, bucket, probe, bytes.NewReader([]byte("after")), 5, "text/plain", ""); e != nil {
		t.Fatal("R2 probe overwrite failed")
	}
	if current, err := client.ObjectVersion(ctx, bucket, probe); err != nil || current == version {
		t.Fatal("R2 source version failed to detect overwrite")
	}
	// Force an overwrite between HEAD and DELETE. A provider that ignores the
	// conditional header must fail acceptance before source cleanup is used.
	version, e = client.ObjectVersion(ctx, bucket, probe)
	if e != nil {
		t.Fatal(e)
	}
	var raceOnce sync.Once
	raceTransport := phase1RoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/conditional-probe") {
			raceOnce.Do(func() {
				if err := client.PutObject(ctx, bucket, probe, bytes.NewReader([]byte("raced")), 5, "text/plain", ""); err != nil {
					t.Error("conditional overwrite probe failed")
				}
			})
		}
		return transport.RoundTrip(r)
	})
	// Probe only: keep unsafe conditional deletion out of the production API.
	// Source organization always retains R2 originals.
	raceClient := s3.NewFromConfig(aws.Config{Region: "auto", Credentials: credentials.NewStaticCredentialsProvider(cfg.R2.AccessKeyID, cfg.R2.SecretAccessKey, ""), HTTPClient: &http.Client{Transport: raceTransport}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.R2.Endpoint)
		o.UsePathStyle = true
	})
	conditionalDeleteUnsupported := false
	etag := version[:strings.LastIndexByte(version, ':')]
	if _, e = raceClient.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(probe), IfMatch: aws.String(etag)}); e == nil {
		conditionalDeleteUnsupported = true
	}
	if !conditionalDeleteUnsupported {
		if _, e = client.HeadSize(ctx, bucket, probe); e != nil {
			t.Fatal("conditional overwrite protection failed")
		}
	}
	dir := t.TempDir()
	cfg.Paths = config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}
	cfg.Storage.Cold = "local"
	cfg.Storage.Hot = "local"
	cfg.Storage.LocalColdDir = ""
	cfg.Storage.LocalHotDir = ""
	cfg.Telegram = config.Telegram{}
	cfg.DevMock = false
	db, e := dbx.Open(filepath.Join(dir, "pimg.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	svc, e := New(cfg, db, master, nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	id, e := svc.SaveStorageInstance("", "R2 隔离验收", "r2", InstanceConfig{Endpoint: cfg.R2.Endpoint, AccessKey: cfg.R2.AccessKeyID, SecretKey: cfg.R2.SecretAccessKey, Bucket: bucket, Prefix: prefix, PublicBase: cfg.R2.PublicBaseURL}, true)
	if e != nil {
		t.Fatal(e)
	}
	p := svc.StoragePolicy()
	p.Archives = []string{"local", id}
	p.Permanent = id
	p.Temporary = id
	p.NormalMB = 128
	p.AcceleratedMB = 128
	if e = svc.SaveStoragePolicy(p); e != nil {
		t.Fatal(e)
	}
	plain := tinyMedia(t)
	f := archiveMedia(t, svc, plain)
	if e = svc.RunArchiveSync(ctx, f.ID); e != nil {
		t.Fatal("real R2 replication failed", e)
	}
	rc, e := svc.SourceStream(context.WithValue(ctx, archiveSourceContext{}, id), f)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(rc)
	rc.Close()
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("real R2 restore mismatch")
	}
	if e = svc.RunTempMaterialize(ctx, f.ID, "original"); e != nil {
		t.Fatal("real R2 temporary materialization failed", e)
	}
	obj, e := db.LiveTempObject(f.ID, "original", time.Now().Unix())
	if e != nil {
		t.Fatal(e)
	}
	realTempKey := prefix + "/temp/" + obj.ObjectKey
	if n, err := client.HeadSize(ctx, bucket, realTempKey); err != nil || n != f.Size {
		t.Fatal("real temporary object was not present before expiry", err)
	}
	_, _ = db.Exec(`UPDATE temp_objects SET expires_at=0 WHERE id=?`, obj.ID)
	if n, e := svc.CleanupExpiredTemp(ctx); e != nil || n != 1 {
		t.Fatal("real R2 expiry did not delete object", n, e)
	}
	_, e = client.HeadSize(ctx, bucket, realTempKey)
	var missing smithy.APIError
	if !errors.As(e, &missing) || (missing.ErrorCode() != "NotFound" && missing.ErrorCode() != "NoSuchKey") {
		t.Fatal("expired R2 temporary object must return explicit not-found", e)
	}
	src := prefix + "/inbox/source.png"
	if e = client.PutObject(ctx, bucket, src, bytes.NewReader(plain), int64(len(plain)), "image/png", ""); e != nil {
		t.Fatal(e)
	}
	album, e := db.EnsureDefaultAlbum()
	if e != nil {
		t.Fatal(e)
	}
	rule, e := svc.SaveImportRule(dbx.ImportRule{Name: "R2 来源整理", InstanceID: id, Source: "inbox", AlbumID: album, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = svc.RunSourceScan(ctx, rule); e != nil {
		t.Fatal("real R2 source scan failed", e)
	}
	var imported string
	if e = db.QueryRow(`SELECT file_id FROM import_receipts WHERE rule_id=? AND state='archiving'`, rule).Scan(&imported); e != nil {
		t.Fatal(e)
	}
	if e = svc.RunArchive(ctx, imported); e != nil {
		t.Fatal(e)
	}
	if e = svc.RunSourceCleanup(ctx, imported); e != nil {
		t.Fatal("real R2 source organization failed", e)
	}
	if _, e = client.HeadSize(ctx, bucket, src); e != nil {
		t.Fatal("R2 source must be retained for manual cleanup")
	}
	var cleanupState string
	if e = db.QueryRow(`SELECT state FROM import_receipts WHERE file_id=?`, imported).Scan(&cleanupState); e != nil || cleanupState != "cleanup_pending" {
		t.Fatal("R2 manual cleanup state is missing", e)
	}
	if e = svc.RunArchiveSync(ctx, imported); e != nil {
		t.Fatal(e)
	}
	// Permanent publication uses the same instance but a separate namespace.
	direct := unpublishedPlainArchive(t, svc, plain)
	if e = svc.RunPublish(ctx, direct.ID, false); e != nil {
		t.Fatal("real R2 publication failed", e)
	}
	if e = svc.RunUnpublish(ctx, direct.ID); e != nil {
		t.Fatal("real R2 revocation failed", e)
	}
	result := map[string]any{"real_r2": true, "replica_sha256": true, "temporary_expiry": true, "source_import_and_manual_cleanup_receipt": true, "changed_source_preserved": true, "permanent_publication_and_revocation": true, "isolated_prefix": prefix, "credentials_logged": false, "conditional_delete_unsupported": conditionalDeleteUnsupported}
	raw, _ := json.MarshalIndent(result, "", "  ")
	report := filepath.Join("..", "..", "audit", "2026-10-17", "single-machine-phase1", "real-r2-results.json")
	if dir := os.Getenv("PIMG_ACCEPTANCE_REPORT_DIR"); dir != "" {
		report = filepath.Join(dir, "real-r2-results.json")
	}
	if e = os.WriteFile(report, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
