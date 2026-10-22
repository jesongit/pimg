package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"testing"

	"pimg/internal/dbx"
)

func TestModernEncryptionConversionPreservesSplitVideoBytes(t *testing.T) {
	s, _, _ := peerFixture(t)
	ctx := context.Background()
	i, _ := s.instance("local")
	cfg := i.config
	cfg.Encrypt = true
	if _, err := s.SaveStorageInstance("local", i.row.Name, "local", cfg, true); err != nil {
		t.Fatal(err)
	}
	f := &dbx.File{ID: "split-conversion", OwnerID: dbx.AdminOwner, Name: "Video", OrigName: "input.mp4", Ext: "mp4", Mime: "video/mp4", Size: 99999, SHA256: "original-hash-differs-from-encoded-parts", Kind: "video", Duration: 7, Storage: "direct", ColdBackend: "local", ArchiveState: "done", KeyWrapped: []byte{1}, KeyNonce: []byte{2}}
	if err := s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE files SET media_version=2 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	r := dbx.MediaReplica{FileID: f.ID, InstanceID: "local", Split: true, PartMB: 256}
	if err := s.DB.EnsureMediaReplica(r); err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("encoded segment one"), []byte("encoded segment two")}
	for index, data := range want {
		path := s.plainPath(f.ID) + string(rune('a'+index))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
		p := dbx.MediaPart{Index: index, Count: 2, Ext: "mp4", MIME: "video/mp4", Size: int64(len(data)), SHA256: sha256Hex(data), Duration: 3.5, Start: float64(index) * 3.5}
		if _, err := s.writeMediaPart(ctx, f, r, p, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB.SetMediaReplicaState(f.ID, "local", "ready", ""); err != nil {
		t.Fatal(err)
	}
	for _, encrypted := range []bool{true, false} {
		if _, err := s.QueueConversion(ctx, f.ID, encrypted); err != nil {
			t.Fatal(err)
		}
		id, err := s.RunConversion(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		target, _ := s.DB.GetFile(id)
		replicas, err := s.DB.MediaReplicas(id)
		if err != nil || len(replicas) != 1 || replicas[0].Encrypted != encrypted || !replicas[0].Split || len(replicas[0].Parts) != 2 {
			t.Fatal("split encryption policy changed", replicas, err)
		}
		for index, p := range replicas[0].Parts {
			if p.Index != index || p.Count != 2 || p.Start != float64(index)*3.5 || p.Duration != 3.5 || p.Original {
				t.Fatal("segment metadata changed", p)
			}
			stream, err := s.openMediaPart(ctx, target, replicas[0], p)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(stream)
			stream.Close()
			if err != nil || !bytes.Equal(data, want[index]) {
				t.Fatal("segment re-encoded or remuxed", index, err)
			}
		}
		if err := s.RunConvertedCleanup(ctx, f.ID); err != nil {
			t.Fatal(err)
		}
		f = target
	}
}

func TestModernEncryptionConversionPreservesBytesPoliciesAndResume(t *testing.T) {
	s, peer, plain := peerFixture(t)
	ctx := context.Background()
	p := s.StoragePolicy()
	p.Archives = []string{"local", peer}
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	local, _ := s.instance("local")
	cfg := local.config
	cfg.Encrypt = true
	if _, err := s.SaveStorageInstance("local", local.row.Name, "local", cfg, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "original.png", "png", sql.NullInt64{}, nil, 0, true)
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
	c, err := s.QueueConversion(ctx, f.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.GetVisibleFile(c.TargetID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("unverified target visible", err)
	}
	peerInstance, _ := s.instance(peer)
	fault := &conversionColdFault{Cold: peerInstance.cold, failPut: true}
	peerInstance.cold = fault
	if _, err = s.RunConversion(ctx, f.ID); err == nil {
		t.Fatal("copy interruption hidden")
	}
	assertConversionOriginal(t, s, f.ID, plain)
	var kept string
	all, _ := s.DB.MediaReplicas(c.TargetID)
	for _, r := range all {
		if r.InstanceID == "local" && len(r.Parts) == 1 && r.Parts[0].Verified {
			kept = r.Parts[0].Reference
		}
	}
	if kept == "" {
		t.Fatal("verified progress lost")
	}
	// Changing upload defaults cannot reinterpret this already accepted task.
	cfg.Encrypt = false
	if _, err = s.SaveStorageInstance("local", local.row.Name, "local", cfg, true); err != nil {
		t.Fatal(err)
	}
	retry, err := s.QueueConversion(ctx, f.ID, true)
	if err != nil || retry.TargetID != c.TargetID {
		t.Fatal("retry lost captured policy", retry, err)
	}
	fault.failPut = false
	id, err := s.RunConversion(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := s.DB.GetVisibleFile(id)
	if target.MediaVersion != 2 || target.OwnerID != f.OwnerID || target.AlbumID != f.AlbumID || target.SHA256 != f.SHA256 || target.OrigName != f.OrigName || target.MediaAt != f.MediaAt || target.Published {
		t.Fatal("replacement changed identity or original metadata", target)
	}
	all, _ = s.DB.MediaReplicas(id)
	for _, r := range all {
		if r.InstanceID == "local" && (!r.Encrypted || r.Parts[0].Reference != kept) {
			t.Fatal("captured encrypted policy or progress changed", r)
		}
		if r.InstanceID == peer && r.Encrypted {
			t.Fatal("storage opt-out ignored")
		}
	}
	assertConversionOriginal(t, s, id, plain)
	if err = s.RunConvertedCleanup(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	assertConversionOriginal(t, s, id, plain)
	if _, err = s.QueueConversion(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	finalID, err := s.RunConversion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	all, _ = s.DB.MediaReplicas(finalID)
	for _, r := range all {
		if r.Encrypted {
			t.Fatal("plaintext conversion left encrypted replica")
		}
	}
	assertConversionOriginal(t, s, finalID, plain)
}

func TestModernEncryptionConversionRetainsIntentWithoutEligibleStorage(t *testing.T) {
	s, peer, plain := peerFixture(t)
	ctx := context.Background()
	p := s.StoragePolicy()
	p.Archives = []string{"local", peer}
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "plain.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckConversion(f.ID, true); err != nil {
		t.Fatal("file setting must not require an eligible encrypted replica", err)
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM file_conversions`).Scan(&count)
	if count != 0 {
		t.Fatal("preview staged conversion state")
	}
	for _, encrypted := range []bool{true, false} {
		if _, err = s.QueueConversion(ctx, f.ID, encrypted); err != nil {
			t.Fatal(err)
		}
		id, runErr := s.RunConversion(ctx, f.ID)
		if runErr != nil {
			t.Fatal(runErr)
		}
		f, err = s.DB.GetVisibleFile(id)
		if err != nil || (f.Storage != "direct") != encrypted {
			t.Fatal("file setting was not retained", f, err)
		}
		replicas, replicaErr := s.DB.MediaReplicas(id)
		if replicaErr != nil || len(replicas) != 2 {
			t.Fatal(replicas, replicaErr)
		}
		for _, replica := range replicas {
			if replica.Encrypted || replica.State != "ready" {
				t.Fatal("plaintext-only storage received an encrypted or unready replica", replica)
			}
		}
		assertConversionOriginal(t, s, id, plain)
		if err = s.CheckConversion(id, encrypted); err != dbx.ErrConversionNotNeeded {
			t.Fatal("already matching file setting must be skipped", err)
		}
	}
}

func TestConversionCheckUsesActualMixedReplicaPoliciesAndDoesNotWrite(t *testing.T) {
	s, peer, plain := peerFixture(t)
	policy := s.StoragePolicy()
	policy.Archives = []string{"local", peer}
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	local, _ := s.instance("local")
	cfg := local.config
	cfg.Encrypt = true
	if _, err := s.SaveStorageInstance("local", local.row.Name, "local", cfg, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(plain), "mixed.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	var before int
	s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&before)
	if err = s.CheckConversion(f.ID, true); err != dbx.ErrConversionNotNeeded {
		t.Fatal("matching mixed replicas must be skipped", err)
	}
	if _, err = s.QueueConversion(context.Background(), f.ID, true); err != dbx.ErrConversionNotNeeded {
		t.Fatal("submission failed to recheck unchanged policies", err)
	}
	var conversions, after int
	s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM file_conversions),(SELECT COUNT(*) FROM files)`).Scan(&conversions, &after)
	if conversions != 0 || before != after {
		t.Fatal("no-op staged replacement data")
	}
	other, _ := s.instance(peer)
	otherConfig := other.config
	otherConfig.Encrypt = true
	if _, err = s.SaveStorageInstance(peer, other.row.Name, "local", otherConfig, true); err != nil {
		t.Fatal(err)
	}
	// The file-wide intent is unchanged, but this actual replica now needs work.
	if err = s.CheckConversion(f.ID, true); err != nil {
		t.Fatal("changed replica policy ignored", err)
	}
	if _, err = s.QueueConversion(context.Background(), f.ID, true); err != nil {
		t.Fatal(err)
	}
	id, err := s.RunConversion(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	replicas, _ := s.DB.MediaReplicas(id)
	for _, replica := range replicas {
		if !replica.Encrypted {
			t.Fatal("replica conversion ignored selected policy", replica)
		}
	}
	assertConversionOriginal(t, s, id, plain)
}
