package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func transferAlbum(t *testing.T, s *Service) int64 {
	t.Helper()
	if err := s.DB.CreateAccount("user:recipient", "Recipient", "hash"); err != nil {
		t.Fatal(err)
	}
	a, err := s.DB.InsertOwnerAlbum("user:recipient", "Destination")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func assertTransferMetadata(t *testing.T, s *Service, source *dbx.File, targetID string, album int64) {
	t.Helper()
	f, err := s.DB.GetVisibleFile(targetID)
	if err != nil {
		t.Fatal(err)
	}
	if f.OwnerID != "user:recipient" || f.AlbumID.Int64 != album || f.ID == source.ID || f.Published || f.ShowHome || f.HotBackend != "" || f.SHA256 != source.SHA256 || f.Size != source.Size || f.Name != source.Name || f.OrigName != source.OrigName || f.CreatedAt != source.CreatedAt || f.MediaAt != source.MediaAt || f.MediaVersion != source.MediaVersion {
		t.Fatal("transfer changed original metadata or published replacement", f)
	}
	old, err := s.DB.GetFile(source.ID)
	if err != nil || old.OwnerID != source.OwnerID || old.AlbumID != source.AlbumID || !old.Hidden {
		t.Fatal("immutable source identity changed", old, err)
	}
	if _, err = s.DB.GetVisibleFile(source.ID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("retired source remained visible", err)
	}
	if _, err = s.DB.Exec(`UPDATE files SET owner_id='user:recipient' WHERE id=?`, source.ID); err == nil {
		t.Fatal("owner immutability trigger was removed")
	}
}

func TestOwnerTransferLegacyPreservesBytesDatesAndSourceTombstones(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "plain", false: "encrypted"}[direct], func(t *testing.T) {
			s, source, plain := conversionFixture(t, direct)
			album := transferAlbum(t, s)
			ctx := context.Background()
			stamp := int64(1709251199)
			op, err := s.DB.BeginTaskOperation(dbx.AdminOwner, "set_media_time", "manual-date", dbx.MediaDateScope(stamp), 1)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DB.SetMediaTimeInOperation(source.ID, stamp, op.ID); err != nil {
				t.Fatal(err)
			}
			_ = s.DB.SealTaskOperation(op.ID, dbx.AdminOwner)
			if _, err = s.DB.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES('source-family',0,?)`, source.ID); err != nil {
				t.Fatal(err)
			}
			source, _ = s.DB.GetFile(source.ID)
			c, err := s.QueueOwnerTransfer(ctx, source.ID, album, 0)
			if err != nil {
				t.Fatal(err)
			}
			if c.Kind != dbx.TaskTransferOwner || c.TargetOwnerID != "user:recipient" || c.TargetAlbumID != album {
				t.Fatal("incorrect transfer receipt", c)
			}
			if _, err = s.DB.GetVisibleFile(c.TargetID); !errors.Is(err, dbx.ErrNotFound) {
				t.Fatal("staging target exposed")
			}
			assertConversionOriginal(t, s, source.ID, plain)
			id, err := s.RunOwnerTransfer(ctx, source.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertTransferMetadata(t, s, source, id, album)
			assertConversionOriginal(t, s, id, plain)
			var tombstone string
			if err = s.DB.QueryRow(`SELECT file_id FROM telegram_source_parts WHERE source_key='source-family'`).Scan(&tombstone); err != nil || tombstone != source.ID {
				t.Fatal("source-family identity crossed accounts", tombstone, err)
			}
			if err = s.RunConvertedCleanup(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			assertConversionOriginal(t, s, id, plain)
		})
	}
}

func TestOwnerTransferMediaResumesVerifiedReplicasAndCleanup(t *testing.T) {
	s, peer, plain := peerFixture(t)
	ctx := context.Background()
	album := transferAlbum(t, s)
	i, _ := s.instance(peer)
	config := i.config
	config.Encrypt = true
	if _, err := s.SaveStorageInstance(peer, i.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "source.png", "png", sql.NullInt64{}, nil, 0, false)
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
	sourceReplicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, r := range sourceReplicas {
		if r.State == "ready" {
			ready++
		}
	}
	c, err := s.QueueOwnerTransfer(ctx, f.ID, album, 0)
	if err != nil {
		t.Fatal(err)
	}
	i, _ = s.instance(peer)
	fault := &conversionColdFault{Cold: i.cold, failPut: true}
	i.cold = fault
	if _, err = s.RunOwnerTransfer(ctx, f.ID); err == nil {
		t.Fatal("injected write failure accepted")
	}
	if original, err := s.DB.GetVisibleFile(f.ID); err != nil || original.OwnerID != f.OwnerID {
		t.Fatal("source lost before verified cutover", err)
	}
	if _, err = s.DB.GetVisibleFile(c.TargetID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("partial destination exposed")
	}
	parts, err := s.DB.MediaReplicas(c.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	var keptRef string
	for _, r := range parts {
		if r.InstanceID == "local" && len(r.Parts) == 1 && r.Parts[0].Verified {
			keptRef = r.Parts[0].Reference
		}
	}
	if keptRef == "" {
		t.Fatal("verified first replica progress was lost")
	}
	assertConversionOriginal(t, s, f.ID, plain)
	if _, err = s.DB.Exec(`UPDATE tasks SET state='running' WHERE id=?`, c.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ResetRunningTasks(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.QueueOwnerTransfer(ctx, f.ID, album, 0)
	if err != nil || resumed.TargetID != c.TargetID || resumed.TaskID != c.TaskID {
		t.Fatal("restart duplicated transfer", resumed, err)
	}
	id, err := restarted.RunOwnerTransfer(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertTransferMetadata(t, restarted, f, id, album)
	target, _ := s.DB.GetFile(id)
	replicas, err := s.DB.MediaReplicas(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(replicas) != ready {
		t.Fatal("transfer lost a source representation", len(replicas))
	}
	for _, r := range replicas {
		if r.State != "ready" || len(r.Parts) != 1 || r.Encrypted != (r.InstanceID == peer) {
			t.Fatal("replica policy changed", r)
		}
		if r.InstanceID == "local" && r.Parts[0].Reference != keptRef {
			t.Fatal("resume rewrote verified replica")
		}
		if !strings.Contains(r.Parts[0].Reference, target.ID) || strings.Contains(r.Parts[0].Reference, f.ID) {
			t.Fatal("destination reused source locator", r.Parts[0].Reference)
		}
		stream, err := restarted.openMediaPart(ctx, target, r, r.Parts[0])
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil || !bytes.Equal(data, plain) {
			t.Fatal("copied media differs", err)
		}
	}
	i, _ = restarted.instance(peer)
	cleanupFault := &conversionColdFault{Cold: i.cold, failDelete: true}
	i.cold = cleanupFault
	if err = restarted.RunConvertedCleanup(ctx, f.ID); err == nil {
		t.Fatal("cleanup failure did not retain recovery")
	}
	state, _ := s.DB.GetConversion(f.ID)
	if state.Phase != "done" || !state.CleanupPending {
		t.Fatal("cleanup failure rolled back ownership", state)
	}
	if refs, _ := s.DB.MediaReplicas(f.ID); len(refs) == 0 {
		t.Fatal("cleanup discarded failing source locator")
	}
	assertConversionOriginal(t, restarted, id, plain)
	cleanupFault.failDelete = false
	if err = restarted.RunConvertedCleanup(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if refs, _ := s.DB.MediaReplicas(f.ID); len(refs) != 0 {
		t.Fatal("cleanup left original media replicas")
	}
	assertConversionOriginal(t, restarted, id, plain)
}

func TestOwnerTransferPreservesSplitVideoPartsWithoutRemux(t *testing.T) {
	s, _, _ := peerFixture(t)
	ctx := context.Background()
	album := transferAlbum(t, s)
	f := &dbx.File{ID: "split-source", OwnerID: dbx.AdminOwner, Name: "Video", OrigName: "input.mp4", Ext: "mp4", Mime: "video/mp4", Size: 99999, SHA256: "original-container-hash-different-from-encoded-parts", Kind: "video", Duration: 7, MediaVersion: 2, Storage: "direct", ColdBackend: "local", ArchiveState: "done", KeyWrapped: []byte{1}, KeyNonce: []byte{2}}
	if err := s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	// Production V2 intake commits this column in its batch transaction. This
	// fixture supplies pre-encoded segments directly to avoid invoking ffmpeg.
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
	f, _ = s.DB.GetFile(f.ID)
	if _, err := s.QueueOwnerTransfer(ctx, f.ID, album, 0); err != nil {
		t.Fatal(err)
	}
	id, err := s.RunOwnerTransfer(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertTransferMetadata(t, s, f, id, album)
	target, _ := s.DB.GetFile(id)
	replicas, _ := s.DB.MediaReplicas(id)
	if len(replicas) != 1 || len(replicas[0].Parts) != 2 {
		t.Fatal("split layout changed", replicas)
	}
	for index, p := range replicas[0].Parts {
		if p.Index != index || p.Count != 2 || p.Start != float64(index)*3.5 || p.Duration != 3.5 || p.Original {
			t.Fatal("part timing/layout changed", p)
		}
		stream, err := s.openMediaPart(ctx, target, replicas[0], p)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil || !bytes.Equal(data, want[index]) {
			t.Fatal("segment was re-encoded or remuxed", index, err)
		}
	}
}
