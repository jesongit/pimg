package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"pimg/internal/storage"
)

type unavailablePresentation struct{ storage.Hot }

func (b unavailablePresentation) PutObject(context.Context, string, string, io.Reader, int64, string, string) error {
	return errors.New("synthetic write outage")
}
func (b unavailablePresentation) OpenObject(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("synthetic read outage")
}

func TestMediaPublicationFallsBackAndKeepsStableURL(t *testing.T) {
	ctx := context.Background()
	s, peer, plain := peerFixture(t)
	p := s.StoragePolicy()
	p.Permanent = "local"
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(WithPrivateUpload(ctx), bytes.NewReader(plain), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunPublish(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	stable := s.PublicURL(f)
	if stable == "" {
		t.Fatal("missing stable publication URL")
	}
	object, err := s.publicationObject(f)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-72 * time.Hour)
	if err = os.Chtimes(s.presentationCachePath(f, object.SHA256), past, past); err != nil {
		t.Fatal(err)
	}
	s.storages.mu.Lock()
	local := s.storages.instances["local"]
	local.hot = unavailablePresentation{local.hot}
	s.hotBackends["local"] = unavailablePresentation{s.hotBackends["local"]}
	other := s.storages.instances[peer]
	other.config.Priority = 100
	s.storages.mu.Unlock()
	r := httptest.NewRequest("GET", stable, nil)
	r.SetPathValue("id", f.ID)
	r.Header.Set("Range", "bytes=0-15")
	w := httptest.NewRecorder()
	s.ServeMediaPublication(w, r)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), plain[:16]) {
		t.Fatalf("gateway fallback: %d %s", w.Code, w.Body.String())
	}
	f, _ = s.DB.GetFile(f.ID)
	if f.HotBackend != peer || s.PublicURL(f) != stable {
		t.Fatal("fallback changed URL or missed healthy peer", f.HotBackend)
	}
	if err = s.RunUnpublish(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.ServeMediaPublication(w, r)
	if w.Code != 404 {
		t.Fatal("revoked publication remained accessible", w.Code)
	}
}

func TestMediaTemporaryFallbackDoesNotExtendOldLinkAndSupportsEncryption(t *testing.T) {
	ctx := context.Background()
	s, peer, plain := peerFixture(t)
	i, _ := s.instance("local")
	c := i.config
	c.Encrypt = true
	if _, err := s.SaveStorageInstance("local", i.row.Name, "local", c, true); err != nil {
		t.Fatal(err)
	}
	p := s.StoragePolicy()
	p.Temporary = "local"
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "private.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bool
	if err = s.DB.QueryRow(`SELECT encrypted FROM media_temporary WHERE object_id=?`, obj.ID).Scan(&encrypted); err != nil || !encrypted {
		t.Fatal("missing actual encryption", err)
	}
	address, expires, err := s.presignTemp(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	_, sum, _ := hashMediaPath(ctx, s.plainPath(f.ID))
	past := time.Now().Add(-72 * time.Hour)
	if err = os.Chtimes(s.presentationCachePath(f, sum), past, past); err != nil {
		t.Fatal(err)
	}
	s.storages.mu.Lock()
	local := s.storages.instances["local"]
	local.hot = unavailablePresentation{local.hot}
	s.hotBackends["local"] = unavailablePresentation{s.hotBackends["local"]}
	s.storages.instances[peer].config.Priority = 100
	s.storages.mu.Unlock()
	r := httptest.NewRequest("GET", address, nil)
	r.SetPathValue("id", obj.ID)
	w := httptest.NewRecorder()
	s.ServeMediaTemporary(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("temporary fallback %d %s", w.Code, w.Body.String())
	}
	parsed, _ := url.Parse(address)
	if parsed.Query().Get("expires") == "" || expires > obj.ExpiresAt {
		t.Fatal("link lifetime extended")
	}
	if err = s.DB.MarkTempDeleted(obj.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.ServeMediaTemporary(w, r)
	if w.Code != 404 {
		t.Fatal("revoked temporary alias remained readable", w.Code)
	}
}
