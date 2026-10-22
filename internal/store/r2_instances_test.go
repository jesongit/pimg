package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func TestDefaultR2InstancesExposeSeparateBucketsAndPurposes(t *testing.T) {
	s, _, _ := newTestEnv(t)
	views := map[string]InstanceView{}
	for _, view := range s.StorageInstances() {
		views[view.ID] = view
	}
	permanent, temporary := views["r2"], views[defaultR2Temporary]
	if len(views) != 4 || permanent.Name != "R2 永久存储" || temporary.Name != "R2 临时存储" {
		t.Fatalf("default instances were not split: %#v", views)
	}
	if permanent.Config.Bucket != s.Cfg.R2.PermBucket || temporary.Config.Bucket != s.Cfg.R2.TempBucket || permanent.Config.Bucket == temporary.Config.Bucket {
		t.Fatal("default instances do not identify their own bucket")
	}
	if slices.Contains(permanent.Capabilities, "temporary") || !slices.Contains(permanent.Capabilities, "permanent") || slices.Contains(temporary.Capabilities, "permanent") || slices.Contains(temporary.Capabilities, "archive") || !slices.Contains(temporary.Capabilities, "temporary") {
		t.Fatal("default buckets expose the wrong purposes", permanent.Capabilities, temporary.Capabilities)
	}
	for _, view := range []InstanceView{permanent, temporary} {
		if !view.HasCredentials || view.Config.AccessKey != "" || view.Config.SecretKey != "" {
			t.Fatal("inherited credentials were missing or exposed")
		}
	}
	p := s.StoragePolicy()
	if p.Permanent != "r2" || p.Temporary != defaultR2Temporary {
		t.Fatal("fresh policy does not use the split defaults", p)
	}
	p.Temporary = "r2"
	if err := s.SaveStoragePolicy(p); err == nil {
		t.Fatal("permanent bucket was accepted for new temporary copies")
	}
}

func TestDefaultR2SplitPreservesOldFileLinksAndTemporaryCleanup(t *testing.T) {
	s, _, mock := newTestEnv(t)
	ctx := context.Background()
	plain := bigPNG(t)
	f := unpublishedPlainArchive(t, s, plain)
	if err := s.RunPublish(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	oldURL := s.publicURL(f, origKey(f))
	legacy := &dbx.TempObject{ID: "legacy-r2-temp", FileID: f.ID, Kind: "original", Backend: "r2", ObjectKey: "tmp/legacy/" + f.ID + ".png", Size: sql.NullInt64{Int64: int64(len(plain)), Valid: true}, ExpiresAt: time.Now().Unix() + 3600}
	if err := s.R2.PutObject(ctx, s.Cfg.R2.TempBucket, legacy.ObjectKey, bytes.NewReader(plain), int64(len(plain)), "image/png", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.InsertTempObject(legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.MarkTempReady(legacy.ID); err != nil {
		t.Fatal(err)
	}
	// Recreate a database from before the split, including the old policy and
	// an existing live temporary object whose backend must remain unchanged.
	if _, err := s.DB.Exec(`DELETE FROM storage_instances WHERE id=?`, defaultR2Temporary); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE storage_instances SET name='Cloudflare R2' WHERE id='r2'`); err != nil {
		t.Fatal(err)
	}
	p := s.StoragePolicy()
	p.Temporary = "r2"
	if err := s.DB.SetSetting("storage.policy", dbx.JSONValue(p)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.initInstances(); err != nil {
			t.Fatal(err)
		}
		got, err := s.DB.GetFile(f.ID)
		if err != nil || !reflect.DeepEqual(got, f) || s.publicURL(got, origKey(got)) != oldURL {
			t.Fatal("migration rewrote an existing file or published URL", err)
		}
		if s.StoragePolicy().Temporary != defaultR2Temporary || len(s.StorageInstances()) != 4 {
			t.Fatal("migration is incomplete or not idempotent")
		}
	}
	obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	if err != nil || obj.Backend != "r2" {
		t.Fatal("old temporary object's binding was rewritten", err)
	}
	b, err := s.hot(obj.Backend)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := b.(storage.HotReader).OpenObject(ctx, storage.Temporary, obj.ObjectKey)
	if err != nil {
		t.Fatal("old temporary bucket can no longer be read", err)
	}
	read, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(read, plain) {
		t.Fatal("legacy temporary bytes changed", err)
	}
	if _, err := s.DB.Exec(`UPDATE temp_objects SET expires_at=? WHERE id=?`, time.Now().Unix()-1, obj.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CleanupExpiredTemp(ctx); err != nil || n != 1 {
		t.Fatal("legacy cleanup failed", n, err)
	}
	if err := s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	obj, err = s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	if err != nil || obj.Backend != defaultR2Temporary {
		t.Fatal("new temporary object did not bind the separate instance", err)
	}
	if got := fetchTempObject(t, mock.Base+"/"+s.Cfg.R2.TempBucket+"/"+obj.ObjectKey); !bytes.Equal(got, plain) {
		t.Fatal("new temporary copy was not written to the temporary bucket")
	}
	if _, err := s.R2.HeadSize(ctx, s.Cfg.R2.PermBucket, obj.ObjectKey); err == nil {
		t.Fatal("temporary plaintext also appeared in the permanent bucket")
	}
	if _, err := s.DB.Exec(`UPDATE temp_objects SET expires_at=? WHERE id=?`, time.Now().Unix()-1, obj.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CleanupExpiredTemp(ctx); err != nil || n != 1 {
		t.Fatal("new temporary cleanup failed", n, err)
	}
	r, err := http.Get(oldURL)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	got, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 || !bytes.Equal(got, plain) {
		t.Fatal("temporary cleanup affected the existing permanent link", err)
	}
}

func TestDefaultR2SplitKeepsExplicitOtherTemporaryInstance(t *testing.T) {
	s, _, _ := newTestEnv(t)
	p := s.StoragePolicy()
	p.Temporary = "local"
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	if err := s.initInstances(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, s.StoragePolicy()) {
		t.Fatal("restart changed an explicitly selected temporary instance")
	}
}
