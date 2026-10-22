package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

func v2CoverFixture(t *testing.T, mode string) (*Service, *coverRecorderTG, *dbx.File, []byte, image.Image) {
	t.Helper()
	s, remote := workflowEnv(t)
	recorder := &coverRecorderTG{workflowTG: remote}
	s.UserTG = recorder
	if mode == "telegram-stream" {
		s.UserTG = &coverStreamTG{recorder}
	}
	var album sql.NullInt64
	target := "telegram"
	if mode == "local" {
		target = "local"
		if err := s.SetStorage("local", "local"); err != nil {
			t.Fatal(err)
		}
		instance, err := s.instance("local")
		if err != nil {
			t.Fatal(err)
		}
		config := instance.config
		config.Encrypt = true
		if _, err = s.SaveStorageInstance("local", instance.row.Name, "local", config, true); err != nil {
			t.Fatal(err)
		}
	} else {
		id, _ := workflowBound(t, s, remote)
		album = sql.NullInt64{Int64: id, Valid: true}
	}
	policy := s.StoragePolicy()
	policy.Archives = []string{target}
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	plain := coverPattern(t)
	f, err := s.ReceiveUpload(t.Context(), bytes.NewReader(plain), "v2-cover.png", "png", album, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.MediaVersion != 2 {
		t.Fatal("fixture did not use the current media intake")
	}
	clear, err := mediax.MakePreview(s.plainPath(f.ID), "image", 256)
	if err != nil {
		t.Fatal(err)
	}
	clearImage, _, err := image.Decode(bytes.NewReader(clear.Data))
	if err != nil {
		t.Fatal(err)
	}
	return s, recorder, f, plain, clearImage
}

func v2CoverWire(t *testing.T, s *Service, recorder *coverRecorderTG, f *dbx.File) ([]byte, dbx.MediaReplica, dbx.MediaPart) {
	t.Helper()
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil || len(replicas) != 1 || !replicas[0].Encrypted || len(replicas[0].Parts) != 1 || !replicas[0].Parts[0].Verified {
		t.Fatal("expected one verified encrypted media replica", replicas, err)
	}
	r, p := replicas[0], replicas[0].Parts[0]
	var ref storage.Reference
	if err = json.Unmarshal([]byte(p.Reference), &ref); err != nil {
		t.Fatal(err)
	}
	if s.telegramInstance(r.InstanceID) {
		object, err := decodeTelegramObject(ref.Key)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Clone(recorder.data[object.MessageID]), r, p
	}
	backend, err := s.archiveBackend(r.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := backend.Open(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	return wire, r, p
}

func TestV2EncryptedMediaCarriesBlurredPreview(t *testing.T) {
	for _, mode := range []string{"local", "telegram-buffer", "telegram-stream"} {
		t.Run(mode, func(t *testing.T) {
			s, recorder, f, plain, clear := v2CoverFixture(t, mode)
			if err := s.RunArchive(t.Context(), f.ID); err != nil {
				t.Fatal(err)
			}
			wire, _, _ := v2CoverWire(t, s, recorder, f)
			preview, _, _, err := carrier.DecodePart(wire)
			if err != nil {
				t.Fatal(err)
			}
			assertBlurredCover(t, preview, clear)
			if mode != "local" {
				if _, err = jpeg.Decode(bytes.NewReader(recorder.thumbnail)); err != nil {
					t.Fatal("v2 TG upload omitted its JPEG document thumbnail", err)
				}
				assertBlurredCover(t, recorder.thumbnail, clear)
			}
			var recovered bytes.Buffer
			if _, err = carrier.Recover(bytes.NewReader(wire), &recovered, s.Master); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
				t.Fatal("blurred carrier failed exact offline recovery", err)
			}
		})
	}
}

func TestV2MediaCoverUsesLocalPreviewAfterSourceWorkRemoval(t *testing.T) {
	s, recorder, f, _, clear := v2CoverFixture(t, "telegram-stream")
	if err := s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	_, replica, part := v2CoverWire(t, s, recorder, f)
	if _, err := os.Stat(s.plainPath(f.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture still has its upload source", err)
	}
	f, err := s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := recorder.downloadCalls
	preview, legacy, err := s.prepareMediaCover(t.Context(), f, replica, part, filepath.Join(t.TempDir(), "missing-original"))
	if err != nil || legacy {
		t.Fatal("new replica cover failed local-preview fallback", legacy, err)
	}
	assertBlurredCover(t, preview, clear)
	thumbnail, err := mediaCoverThumbnail(preview)
	if err != nil {
		t.Fatal(err)
	}
	assertBlurredCover(t, thumbnail, clear)
	if recorder.downloadCalls != before {
		t.Fatal("cover fallback fetched a remote original")
	}
}

func TestV2MediaCoverUsesPlaceholderOnlyWithoutReadableMaterial(t *testing.T) {
	s, recorder, _, _, _ := v2CoverFixture(t, "telegram-buffer")
	f := &dbx.File{ID: "missing-cover-material", Kind: "image"}
	r := dbx.MediaReplica{InstanceID: "telegram", Encrypted: true}
	p := dbx.MediaPart{SHA256: strings.Repeat("0", 64)}
	before := recorder.downloadCalls
	preview, legacy, err := s.prepareMediaCover(t.Context(), f, r, p, filepath.Join(t.TempDir(), "absent"))
	if err != nil || legacy || !bytes.Equal(preview, privacyPlaceholder()) {
		t.Fatal("unreadable material did not use the privacy placeholder", legacy, err)
	}
	thumbnail, err := mediaCoverThumbnail(preview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jpeg.Decode(bytes.NewReader(thumbnail)); err != nil {
		t.Fatal("placeholder document thumbnail is invalid", err)
	}
	if recorder.downloadCalls != before {
		t.Fatal("missing cover triggered remote media access")
	}
}

func TestV2MediaCoverReadsMaterializedPartOutsideUploadWorkspace(t *testing.T) {
	s, recorder, _, plain, clear := v2CoverFixture(t, "telegram-stream")
	f := &dbx.File{ID: "materialized-part-cover", Kind: "image"}
	r := dbx.MediaReplica{InstanceID: "telegram", Encrypted: true}
	p := dbx.MediaPart{SHA256: sha256Hex(plain)}
	partSource := filepath.Join(t.TempDir(), "replica-source.png")
	if err := os.WriteFile(partSource, plain, 0600); err != nil {
		t.Fatal(err)
	}
	before := recorder.downloadCalls
	preview, legacy, err := s.prepareMediaCover(t.Context(), f, r, p, partSource)
	if err != nil || legacy {
		t.Fatal("materialized source could not produce a cover", legacy, err)
	}
	assertBlurredCover(t, preview, clear)
	if recorder.downloadCalls != before {
		t.Fatal("local part cover fetched a remote original")
	}
}

type v2CoverUploadAttempt struct {
	wire, thumbnail []byte
	random          int64
}

// Model Telegram's stable-random-ID deduplication and an acknowledgment lost
// after the document was committed. The fake performs no network operations.
type v2CoverAckTG struct {
	*workflowTG
	attempts []v2CoverUploadAttempt
	refs     map[int64]tguser.Reference
	failAck  bool
}

func (r *v2CoverAckTG) Upload(ctx context.Context, chat int64, topic int, name, mime string, wire, thumbnail []byte, caption string, random int64) (tguser.Reference, error) {
	for _, prior := range r.attempts {
		if prior.random == random && (!bytes.Equal(prior.wire, wire) || !bytes.Equal(prior.thumbnail, thumbnail)) {
			return tguser.Reference{}, errors.New("same Telegram upload identity reused for different carrier or thumbnail")
		}
	}
	r.attempts = append(r.attempts, v2CoverUploadAttempt{bytes.Clone(wire), bytes.Clone(thumbnail), random})
	if prior, ok := r.refs[random]; ok {
		return prior, nil
	}
	ref, err := r.workflowTG.Upload(ctx, chat, topic, name, mime, wire, thumbnail, caption, random)
	if err != nil {
		return ref, err
	}
	r.refs[random] = ref
	if r.failAck {
		r.failAck = false
		return tguser.Reference{}, errors.New("fixture lost acknowledgement after remote commit")
	}
	return ref, nil
}

type v2CoverAckStreamTG struct{ *v2CoverAckTG }

func (r *v2CoverAckStreamTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, input io.Reader, size int64, thumbnail []byte, caption string, random int64, _ *tguser.VideoMetadata) (tguser.Reference, error) {
	wire, err := io.ReadAll(input)
	if err != nil {
		return tguser.Reference{}, err
	}
	if int64(len(wire)) != size {
		return tguser.Reference{}, io.ErrUnexpectedEOF
	}
	return r.Upload(ctx, chat, topic, name, mime, wire, thumbnail, caption, random)
}

func (r *v2CoverAckStreamTG) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	wire, err := r.Download(ctx, ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(wire)), nil
}

func TestV2MediaCompleteWireAndThumbnailStayStableAfterLostAcknowledgement(t *testing.T) {
	for _, mode := range []string{"telegram-buffer", "telegram-stream"} {
		t.Run(mode, func(t *testing.T) {
			s, recorder, f, plain, clear := v2CoverFixture(t, mode)
			transport := &v2CoverAckTG{workflowTG: recorder.workflowTG, refs: map[int64]tguser.Reference{}, failAck: true}
			s.UserTG = transport
			if mode == "telegram-stream" {
				s.UserTG = &v2CoverAckStreamTG{transport}
			}
			if err := s.RunArchive(t.Context(), f.ID); err == nil {
				t.Fatal("fixture acknowledgement loss was not reported")
			}
			if len(transport.attempts) != 1 || len(transport.data) != 1 {
				t.Fatal("first attempt did not durably commit exactly one remote document", len(transport.attempts), len(transport.data))
			}
			first := transport.attempts[0]
			preview, _, _, err := carrier.DecodePart(first.wire)
			if err != nil {
				t.Fatal(err)
			}
			assertBlurredCover(t, preview, clear)
			assertBlurredCover(t, first.thumbnail, clear)
			replicas, err := s.DB.MediaReplicas(f.ID)
			if err != nil || len(replicas) != 1 {
				t.Fatal(replicas, err)
			}
			part := dbx.MediaPart{Size: f.Size, SHA256: f.SHA256, Ext: f.Ext, MIME: f.Mime, Count: 1, Original: true}
			legacyIdentity := stableTelegramRandomID(replicas[0].InstanceID, mediaObjectKey(f, replicas[0], part))
			if first.random == legacyIdentity {
				t.Fatal("new blurred wire reused the old black-carrier upload identity")
			}
			if err = os.Remove(s.plainPath(f.ID)); err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(s.previewPath(f)), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.previewPath(f), privacyPlaceholder(), 0600); err != nil {
				t.Fatal(err)
			}
			before := transport.downloadCalls
			cached, legacy, err := s.prepareMediaCover(t.Context(), f, replicas[0], part, s.plainPath(f.ID))
			if err != nil || legacy || !bytes.Equal(cached, preview) {
				t.Fatal("missing source/replaced preview changed the pending carrier cover", legacy, err)
			}
			if transport.downloadCalls != before {
				t.Fatal("pending cover reconstruction downloaded the remote original")
			}
			if err = os.Remove(s.previewPath(f)); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.plainPath(f.ID), plain, 0600); err != nil {
				t.Fatal(err)
			}
			if err = s.RunArchive(t.Context(), f.ID); err != nil {
				t.Fatal("pending carrier did not recover", err)
			}
			if len(transport.attempts) != 2 || len(transport.data) != 1 {
				t.Fatal("retry duplicated or abandoned the committed document", len(transport.attempts), len(transport.data))
			}
			second := transport.attempts[1]
			if first.random != second.random || !bytes.Equal(first.wire, second.wire) || !bytes.Equal(first.thumbnail, second.thumbnail) {
				t.Fatal("retry changed the complete carrier/thumbnail/upload identity")
			}
			var recovered bytes.Buffer
			if _, err = carrier.Recover(bytes.NewReader(second.wire), &recovered, s.Master); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
				t.Fatal("retry changed exact source recovery", err)
			}
		})
	}
}

func TestV2VerifiedLegacyBlackCarrierIsNotRewritten(t *testing.T) {
	s, recorder, f, plain, _ := v2CoverFixture(t, "telegram-stream")
	transport := &v2CoverAckTG{workflowTG: recorder.workflowTG, refs: map[int64]tguser.Reference{}}
	s.UserTG = &v2CoverAckStreamTG{transport}
	r, err := s.mediaPolicy(f, "telegram")
	if err != nil || !r.Encrypted {
		t.Fatal(r, err)
	}
	p := dbx.MediaPart{Size: f.Size, SHA256: f.SHA256, Ext: f.Ext, MIME: f.Mime, Count: 1, Original: true}
	// A cipher without its cover sidecar is precisely the old pending format.
	cipher, _, _, err := s.prepareMediaCipher(t.Context(), f, r, p, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	cipher.Close()
	if err = s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	if len(transport.attempts) != 1 || transport.attempts[0].random != stableTelegramRandomID(r.InstanceID, mediaObjectKey(f, r, p)) {
		t.Fatal("old pending carrier did not retain its original upload identity")
	}
	wire, _, originalPart := v2CoverWire(t, s, recorder, f)
	preview, _, _, err := carrier.DecodePart(wire)
	if err != nil || !bytes.Equal(preview, privacyPlaceholder()) {
		t.Fatal("old pending carrier representation was changed", err)
	}
	var recovered bytes.Buffer
	if _, err = carrier.Recover(bytes.NewReader(wire), &recovered, s.Master); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
		t.Fatal("old pending carrier no longer recovers", err)
	}
	before := len(recorder.data)
	if err = s.RunArchive(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(t.Context(), f.ID); err != nil {
		t.Fatal(err)
	}
	after, _, afterPart := v2CoverWire(t, s, recorder, f)
	if len(recorder.data) != before || afterPart.Reference != originalPart.Reference || !bytes.Equal(after, wire) {
		t.Fatal("verified existing carrier was silently rewritten")
	}
}
