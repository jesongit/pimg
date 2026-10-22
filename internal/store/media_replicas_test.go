package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"testing"
	"time"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func TestMediaReplicasKeepSourceAndIndependentEncryption(t *testing.T) {
	ctx := context.Background()
	s, peer, plain := peerFixture(t)
	i, _ := s.instance(peer)
	config := i.config
	config.Encrypt = true
	if _, err := s.SaveStorageInstance(peer, i.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "image.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.MediaVersion != 2 {
		t.Fatal("source was not retained as one logical file")
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]dbx.MediaReplica{}
	for _, r := range replicas {
		states[r.InstanceID] = r
	}
	if states["local"].Encrypted || !states[peer].Encrypted {
		t.Fatalf("wrong actual encryption: %#v", states)
	}
	for _, id := range []string{"local", peer} {
		r := states[id]
		if r.State != "ready" || len(r.Parts) != 1 || !r.Parts[0].Original {
			t.Fatalf("bad representation %s: %#v", id, r)
		}
		input, err := s.openMediaPart(ctx, f, r, r.Parts[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(input)
		input.Close()
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("source changed on %s: %v", id, err)
		}
	}
	files, total, err := s.DB.ListFiles(dbx.FileFilter{PerPage: 100})
	if err != nil || total != 1 || len(files) != 1 {
		t.Fatalf("replicas became gallery files: %d %v", total, err)
	}
	// Later configuration changes cannot reinterpret stored bytes.
	config.Encrypt = false
	if _, err = s.SaveStorageInstance(peer, i.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.DB.MediaReplicas(f.ID)
	for _, r := range fresh {
		if r.InstanceID == peer && !r.Encrypted {
			t.Fatal("configuration changed an existing replica")
		}
	}
}

func TestMediaCipherRetryKeepsIdenticalBytes(t *testing.T) {
	s, peer, plain := peerFixture(t)
	ctx := context.Background()
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "cipher.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.mediaPolicy(f, peer)
	if err != nil {
		t.Fatal(err)
	}
	r.Encrypted = true
	p := dbx.MediaPart{Size: f.Size, SHA256: f.SHA256, Ext: f.Ext, MIME: f.Mime, Count: 1, Original: true}
	first, n, sum, err := s.prepareMediaCipher(ctx, f, r, p, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	a, err := io.ReadAll(first)
	first.Close()
	if err != nil {
		t.Fatal(err)
	}
	second, n2, sum2, err := s.prepareMediaCipher(ctx, f, r, p, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(second)
	second.Close()
	if err != nil || n != n2 || sum != sum2 || !bytes.Equal(a, b) {
		t.Fatal("retry changed ciphertext", err)
	}
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := cryptox.DecryptReader(bytes.NewReader(b), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(decoded)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("retry no longer recovers source", err)
	}
	if err = s.removeMediaCipherWork(f.ID); err != nil {
		t.Fatal(err)
	}
}

type mediaDeleteFailure struct {
	storage.Cold
	fail bool
}

func (b *mediaDeleteFailure) Delete(ctx context.Context, ref storage.Reference) error {
	if b.fail {
		return os.ErrPermission
	}
	return b.Cold.Delete(ctx, ref)
}

func TestMediaPurgeContinuesOtherStoresAndRetainsFailedReferences(t *testing.T) {
	ctx := context.Background()
	s, peer, plain := peerFixture(t)
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "purge.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(peer)
	broken := &mediaDeleteFailure{Cold: i.cold, fail: true}
	i.cold = broken
	if err = s.purgeMedia(ctx, f, ""); err == nil {
		t.Fatal("outage was hidden")
	}
	remaining, err := s.DB.MediaReplicas(f.ID)
	if err != nil || len(remaining) != 1 || remaining[0].InstanceID != peer || len(remaining[0].Parts) != 1 {
		t.Fatal("healthy stores were blocked or failed references lost", remaining, err)
	}
	broken.fail = false
	if err = s.purgeMedia(ctx, f, ""); err != nil {
		t.Fatal(err)
	}
	remaining, err = s.DB.MediaReplicas(f.ID)
	if err != nil || len(remaining) != 0 {
		t.Fatal(remaining, err)
	}
}

func TestMediaPlainUploadNeverEncryptedByStorage(t *testing.T) {
	ctx := context.Background()
	s, peer, plain := peerFixture(t)
	i, _ := s.instance(peer)
	config := i.config
	config.Encrypt = true
	if _, err := s.SaveStorageInstance(peer, i.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "image.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.MediaPolicies {
		if r.Encrypted {
			t.Fatal("plaintext upload was silently encrypted")
		}
	}
}

func TestMediaViewCreatesDurableLocalReplicaOutsideArchiveSelection(t *testing.T) {
	ctx := context.Background()
	s, _, plain := peerFixture(t)
	policy := s.StoragePolicy()
	policy.Archives = []string{"r2"}
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "image.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	if s.MediaLocalReady(f) {
		t.Fatal("a remote upload was silently saved locally")
	}
	file, p, err := s.OpenMediaView(ctx, f, 0, "", true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, plain) || !p.Original {
		t.Fatalf("view changed source: %v", err)
	}
	if !s.MediaLocalReady(f) {
		t.Fatal("view did not record the persistent local replica")
	}
	// Expiring all decode caches must not remove the durable local representation.
	file, p, err = s.OpenMediaView(ctx, f, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	file.Close()
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(path, past, past)
	if err = s.CleanupMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("cache cleanup removed durable original: %v", err)
	}
}

func TestStoragePriorityBeatsSpeedAndHealthBeatsPriority(t *testing.T) {
	s, peer, _ := peerFixture(t)
	ctx := context.Background()
	i, _ := s.instance(peer)
	config := i.config
	config.Priority = 20
	if _, err := s.SaveStorageInstance(peer, i.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	s.recordStorageResult("local", 100<<20, time.Now().Add(-time.Second), nil)
	s.recordStorageResult(peer, 1<<20, time.Now().Add(-time.Second), nil)
	if got := s.rankedArchives()[0]; got != peer {
		t.Fatalf("priority ignored: %s", got)
	}
	s.recordStorageResult(peer, 0, time.Now(), os.ErrPermission)
	if got := s.rankedArchives()[0]; got == peer {
		t.Fatal("unavailable high-priority storage was selected")
	}
	before := s.StoragePolicy()
	if err := s.RunStorageProbe(ctx); err != nil {
		t.Fatal(err)
	}
	after := s.StoragePolicy()
	if dbx.JSONValue(before) != dbx.JSONValue(after) {
		t.Fatal("probe mutated storage policy")
	}
}
