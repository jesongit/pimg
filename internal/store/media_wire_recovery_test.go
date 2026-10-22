package store

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

func TestMediaSyncRepairsKnownBadCarrierWithoutDeduplicatingBadDocument(t *testing.T) {
	s, recorder, f, plain, _ := v2CoverFixture(t, "telegram-buffer")
	if err := s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	wire, r, p := v2CoverWire(t, s, recorder, f)
	_, _, ciphertext, err := carrier.DecodePart(wire)
	if err != nil {
		t.Fatal(err)
	}
	var ref storage.Reference
	if err = json.Unmarshal([]byte(p.Reference), &ref); err != nil {
		t.Fatal(err)
	}
	obj, err := decodeTelegramObject(ref.Key)
	if err != nil {
		t.Fatal(err)
	}
	// Model an older accepted upload which stored raw ciphertext while its
	// durable locator claimed PNG. The independent verified source is retained.
	recorder.data[obj.MessageID] = bytes.Clone(ciphertext)
	obj.Size = int64(len(ciphertext))
	obj.WireSHA256 = sha256Hex(ciphertext)
	obj.Carrier = true
	ref.Key = encodeTelegramObject(obj)
	p.Reference = referenceJSON(ref)
	p.Verified = false
	if err = s.DB.SaveMediaPart(f.ID, r.InstanceID, p); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SetMediaReplicaState(f.ID, r.InstanceID, "failed", "归档载体缺少 PNG 签名"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.plainPath(f.ID), plain, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.mediaCipherPath(f, r, p), ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	legacyRandom := stableTelegramRandomID(r.InstanceID, mediaObjectKey(f, r, p))
	ack := &v2CoverAckTG{workflowTG: recorder.workflowTG, refs: map[int64]tguser.Reference{legacyRandom: obj.Reference}}
	s.UserTG = ack
	recorder.failDelete[obj.MessageID] = 1
	if err = s.RunArchiveSync(t.Context(), f.ID); err == nil {
		t.Fatal("cleanup interruption was hidden")
	}
	current, err := s.DB.MediaReplicas(f.ID)
	if err != nil || len(current) != 1 || len(current[0].Parts) != 1 || !current[0].Parts[0].Verified {
		t.Fatal("new verified replica was lost", current, err)
	}
	if len(ack.attempts) != 1 || ack.attempts[0].random == legacyRandom {
		t.Fatal("repair reused poisoned Telegram upload identity", ack.attempts)
	}
	var retained int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM archive_garbage WHERE file_id=?`, f.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("failed deletion lost cleanup locator", retained, err)
	}
	if err = s.RunArchiveSync(t.Context(), f.ID); err != nil {
		t.Fatal("repair cleanup did not resume", err)
	}
	if len(ack.attempts) != 1 {
		t.Fatal("cleanup retry duplicated the new document")
	}
	if _, exists := recorder.data[obj.MessageID]; exists {
		t.Fatal("retired bad document was not cleaned")
	}
	fresh, _ := s.DB.GetFile(f.ID)
	input, err := s.SourceStream(t.Context(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(plain, got) {
		t.Fatal("repair changed original bytes", err)
	}
	if _, err = s.Verify(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMediaPlainReplicaDoesNotTrustHistoricalCarrierFlag(t *testing.T) {
	s, recorder, f, plain, _ := v2CoverFixture(t, "telegram-buffer")
	instance, _ := s.instance("telegram")
	config := instance.config
	config.Encrypt = false
	if _, err := s.SaveStorageInstance("telegram", instance.row.Name, "telegram", config, true); err != nil {
		t.Fatal(err)
	}
	// This file's already captured policy is changed only in the synthetic
	// fixture, to model a pre-existing plaintext representation.
	if _, err := s.DB.Exec(`UPDATE media_replicas SET policy='{"encrypted":false,"split":false,"part_mb":256}' WHERE file_id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := s.DB.MediaReplicas(f.ID)
	r, p := all[0], all[0].Parts[0]
	var ref storage.Reference
	_ = json.Unmarshal([]byte(p.Reference), &ref)
	obj, _ := decodeTelegramObject(ref.Key)
	obj.Carrier = true
	ref.Key = encodeTelegramObject(obj)
	p.Reference = referenceJSON(ref)
	if err := s.DB.SaveMediaPart(f.ID, r.InstanceID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := carrier.OpenPart(bytes.NewReader(recorder.data[obj.MessageID])); err == nil {
		t.Fatal(err)
	}
	input, err := s.openMediaPart(t.Context(), f, r, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("plaintext policy was decoded as a carrier", err)
	}
}

func TestMediaEncryptedReplicaDecodesHistoricalCarrierExactlyOnce(t *testing.T) {
	s, recorder, f, plain, _ := v2CoverFixture(t, "telegram-buffer")
	if err := s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	_, r, p := v2CoverWire(t, s, recorder, f)
	var ref storage.Reference
	if err := json.Unmarshal([]byte(p.Reference), &ref); err != nil {
		t.Fatal(err)
	}
	obj, err := decodeTelegramObject(ref.Key)
	if err != nil {
		t.Fatal(err)
	}
	obj.Carrier = true
	ref.Key = encodeTelegramObject(obj)
	p.Reference = referenceJSON(ref)
	input, err := s.openMediaPart(t.Context(), f, r, p)
	if err != nil {
		t.Fatal("historical locator caused duplicate PNG decoding", err)
	}
	got, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("encrypted source bytes changed", err)
	}
}
