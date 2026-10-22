package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/filenames"
	"pimg/internal/ids"
	"pimg/internal/tguser"
)

type toolTestTG struct {
	*workflowTG
	readMu sync.Mutex
	reads  map[int]int64
}
type toolTestReader struct {
	io.Reader
	onRead func(int)
}

func (r *toolTestReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.onRead(n)
	return n, e
}
func (r *toolTestReader) Close() error { return nil }
func (tg *toolTestTG) OpenStream(_ context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	tg.mu.Lock()
	b := bytes.Clone(tg.data[ref.MessageID])
	tg.mu.Unlock()
	return &toolTestReader{Reader: bytes.NewReader(b), onRead: func(n int) { tg.readMu.Lock(); tg.reads[ref.MessageID] += int64(n); tg.readMu.Unlock() }}, nil
}
func (tg *toolTestTG) UploadStream(context.Context, int64, int, string, string, io.Reader, int64, []byte, string, int64, *tguser.VideoMetadata) (tguser.Reference, error) {
	panic("tool adoption attempted reupload")
}

func toolEnv(t *testing.T) (*Service, *toolTestTG, int64, int64) {
	t.Helper()
	s, base := workflowEnv(t)
	tg := &toolTestTG{workflowTG: base, reads: map[int]int64{}}
	s.UserTG = tg
	a, err := s.CreateLinkedAlbum(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateLinkedAlbum(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	return s, tg, a, b
}
func toolMessages(t *testing.T, s *Service, tg *toolTestTG, album int64, plain []byte, kind string, encrypted bool, upload string, partIndex, partCount, payloadParts int) (string, []tguser.Message) {
	t.Helper()
	b, err := s.DB.GetTopicBinding(album)
	if err != nil {
		t.Fatal(err)
	}
	instance, keyset, err := s.UploaderIdentity()
	if err != nil {
		t.Fatal(err)
	}
	key := cryptox.GenerateFileKey()
	wrapped, nonce, err := s.Master.WrapFileKey(key)
	if err != nil {
		t.Fatal(err)
	}
	meta := carrier.CarrierFile{OrigName: "unhelpful-name.png", Ext: "png", Mime: "image/png", Size: int64(len(plain)), SHA256: sha256Hex(plain), Kind: kind, Width: 16, Height: 16, HasThumb: true, HasBThumb: true, Naming: filenames.Capture(time.Date(2000, 1, 1, 8, 0, 0, 0, time.FixedZone("source", 8*3600)), "exif")}
	meta.Naming.Sequence = partIndex + 4
	meta.Naming.PartIndex = partIndex
	meta.Naming.PartCount = partCount
	if kind == "video" {
		meta.Ext = "mp4"
		meta.Mime = "video/mp4"
		meta.OrigName = "movie.mp4"
		meta.Duration = 8
		meta.Width = 640
		meta.Height = 360
		meta.Naming.TimeSource = "video_metadata"
	}
	if encrypted {
		meta.KeyWrapped = wrapped
		meta.KeyNonce = nonce
		meta.CryptoVer = 1
	}
	fileID := ids.New()
	meta.Name = "T" + filenames.Stem(kind, encrypted, meta.Naming)
	var cover bytes.Buffer
	if err = jpeg.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 16, 16)), &jpeg.Options{Quality: 82}); err != nil {
		t.Fatal(err)
	}
	out := []tguser.Message{}
	for _, k := range []string{"data", "thumb", "bthumb"} {
		payload := plain
		if k != "data" {
			payload = cover.Bytes()
		}
		if encrypted {
			payload, err = cryptox.EncryptBytes(payload, key)
			if err != nil {
				t.Fatal(err)
			}
		}
		n := 1
		if k == "data" {
			n = payloadParts
		}
		for i := 0; i < n; i++ {
			block := payload[len(payload)*i/n : len(payload)*(i+1)/n]
			wire := block
			if encrypted {
				wire, err = carrier.EncodePart(workflowPNG(t), carrier.PartHeader{FileID: fileID, File: meta, Kind: k, Index: i}, block)
				if err != nil {
					t.Fatal(err)
				}
			}
			ext := meta.Ext
			if k != "data" {
				ext = "jpg"
			}
			if encrypted {
				ext = "png"
			}
			name := meta.Name
			if k == "thumb" {
				name += "_T"
			}
			if k == "bthumb" {
				name += "_B"
			}
			if i > 0 {
				name += fmt.Sprintf("_D%04d", i+1)
			}
			name += "." + ext
			caption := map[string]any{"app": "pimg", "v": 1, "tool": true, "instance_id": instance, "keyset_id": keyset, "upload_id": upload, "file": fileID, "kind": k, "idx": i, "parts": n, "sha256": sha256Hex(block), "wire_sha256": sha256Hex(wire), "wire_size": len(wire), "op": fmt.Sprintf("%x", len(out)+1)}
			if !encrypted && k == "data" && i == 0 {
				m, _ := json.Marshal(meta)
				var mobj map[string]any
				_ = json.Unmarshal(m, &mobj)
				for _, field := range []string{"name", "key_wrapped", "key_nonce", "crypto_ver"} {
					delete(mobj, field)
				}
				caption["meta"] = mobj
			}
			raw, _ := json.Marshal(caption)
			ref, err := tg.workflowTG.Upload(context.Background(), b.ChatID, b.TopicID, name, "application/octet-stream", wire, nil, string(raw), int64(len(out)+1))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, tg.messages[ref.MessageID])
		}
	}
	clear(key)
	return fileID, out
}
func consumeTool(t *testing.T, s *Service, album int64, messages []tguser.Message) *dbx.File {
	t.Helper()
	var result *dbx.File
	for _, m := range messages {
		f, err := s.ReceiveTelegramMessage(context.Background(), m, album)
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if f != nil {
			result = f
		}
	}
	return result
}

func TestUploaderFourModesBoundedAdoptionAndRestore(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, "/", encrypted), func(t *testing.T) {
				s, tg, a, _ := toolEnv(t)
				plain := workflowPNG(t)
				if kind == "video" {
					plain = videoStoreFixture(t)
				}
				pieces := 1
				if kind == "image" {
					pieces = 2
				}
				fileID, msgs := toolMessages(t, s, tg, a, plain, kind, encrypted, ids.New(), 1, 1, pieces)
				// Covers and the second binary block may arrive first.
				for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
					msgs[i], msgs[j] = msgs[j], msgs[i]
				}
				f := consumeTool(t, s, a, msgs)
				if f == nil || f.ID != fileID || f.ArchiveState != "done" || f.Naming.Sequence != 1 {
					t.Fatalf("adoption incomplete: %+v", f)
				}
				for _, m := range msgs {
					var c toolCaption
					_ = json.Unmarshal([]byte(m.Caption), &c)
					if c.Kind != "data" {
						continue
					}
					n := tg.reads[m.Reference.MessageID]
					if !encrypted && n != 0 {
						t.Fatal("plaintext original read during adoption", n)
					}
					if encrypted && n >= m.Reference.Size {
						t.Fatal("full encrypted original read during adoption", n, m.Reference.Size)
					}
				}
				preview, err := os.ReadFile(s.previewPath(f))
				if err != nil || len(preview) > 102400 {
					t.Fatal("local clear preview missing", err)
				}
				source, err := s.SourceStream(context.Background(), f)
				if err != nil {
					t.Fatal(err)
				}
				restored, err := io.ReadAll(source)
				source.Close()
				if err != nil || !bytes.Equal(restored, plain) {
					t.Fatal("restored bytes changed", err)
				}
				if _, err = s.Verify(context.Background(), f.ID); err != nil {
					t.Fatal(err)
				}
				consumeTool(t, s, a, msgs)
				events, err := s.DB.ListIngestEvents(f.ID, "", "", 0)
				if err != nil || len(events) != 2 || events[0].Type != "verified" || events[1].Type != "adopted" {
					t.Fatal("repeated scan duplicated events", len(events), err)
				}
				var tasks, temps int
				_ = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=?`, f.ID).Scan(&tasks)
				_ = s.DB.QueryRow(`SELECT COUNT(*) FROM temp_objects WHERE file_id=?`, f.ID).Scan(&temps)
				if tasks != 0 || temps != 0 || f.Published {
					t.Fatal("adoption scheduled upload/publication", tasks, temps)
				}
			})
		}
	}
}

func TestUploaderGlobalImageDedupLogsModesAndLocalIntake(t *testing.T) {
	s, tg, a, b := toolEnv(t)
	plain := workflowPNG(t)
	_, first := toolMessages(t, s, tg, a, plain, "image", true, ids.New(), 1, 1, 1)
	kept := consumeTool(t, s, a, first)
	duplicateID, duplicate := toolMessages(t, s, tg, b, plain, "image", true, ids.New(), 1, 1, 1)
	got := consumeTool(t, s, b, duplicate)
	if got.ID != kept.ID || got.AlbumID.Int64 != a {
		t.Fatal("cross-album duplicate moved/created record")
	}
	events, err := s.DB.ListIngestEvents(kept.ID, duplicateID, "", 0)
	if err != nil || len(events) != 1 || events[0].Type != "duplicate" {
		t.Fatal(events, err)
	}
	same, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(plain), "different-name.png", "png", sql.NullInt64{Int64: b, Valid: true}, nil, 0, false)
	if err != nil || same.ID != kept.ID {
		t.Fatal("local intake didn't share dedup", err)
	}
	fromTG, err := s.receiveLegacyTelegramFixture(context.Background(), plain, "normal.png", "image/png", b, "11:77:999999")
	if err != nil || fromTG.ID != kept.ID {
		t.Fatal("ordinary Telegram intake didn't share dedup", err)
	}
	if _, err = os.Stat(s.plainPath(fromTG.ID)); err == nil {
		t.Fatal("duplicate intake left new plaintext work file")
	}
	_, readable := toolMessages(t, s, tg, b, plain, "image", false, ids.New(), 1, 1, 1)
	r := consumeTool(t, s, b, readable)
	if r == nil || r.ID == kept.ID || r.Storage != "direct" {
		t.Fatal("encryption modes merged")
	}
	var count, seq int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE hidden=0`).Scan(&count)
	_ = s.DB.QueryRow(`SELECT last_sequence FROM file_name_counters WHERE date='20000101'`).Scan(&seq)
	if count != 2 || seq != 2 {
		t.Fatal("duplicates consumed records/numbers", count, seq)
	}
}

func TestUploaderVideoGroupOutOfOrderAndResume(t *testing.T) {
	s, tg, a, _ := toolEnv(t)
	plain := videoStoreFixture(t)
	batch := ids.New()
	id2, second := toolMessages(t, s, tg, a, plain, "video", true, batch, 2, 2, 1)
	if f := consumeTool(t, s, a, second); f != nil {
		t.Fatal("incomplete group was finalized")
	}
	var counters int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM file_name_counters`).Scan(&counters)
	if counters != 0 {
		t.Fatal("partial group consumed formal numbers")
	}
	// Staging is persisted, not attached to service memory.
	s.previews = newPreviewCache(1<<20, time.Minute)
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	diskDB, err := dbx.Open(filepath.Join(s.Cfg.Paths.DataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { diskDB.Close() })
	s.DB = diskDB
	id1, first := toolMessages(t, s, tg, a, plain, "video", true, batch, 1, 2, 1)
	consumeTool(t, s, a, first)
	f1, e1 := s.DB.GetFile(id1)
	f2, e2 := s.DB.GetFile(id2)
	if e1 != nil || e2 != nil || f1.Naming.Sequence != 1 || f2.Naming.Sequence != 2 {
		t.Fatal("out-of-order group renumbered", e1, e2, f1, f2)
	}
}

func TestUploaderRetirementRejectsCopiedMessagesButAllowsNewIdentity(t *testing.T) {
	s, tg, a, _ := toolEnv(t)
	plain := workflowPNG(t)
	fileID, msgs := toolMessages(t, s, tg, a, plain, "image", true, ids.New(), 1, 1, 1)
	f := consumeTool(t, s, a, msgs)
	if _, err := s.DB.Exec(`UPDATE files SET hidden=1 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	m := msgs[0]
	ref, err := tg.workflowTG.Upload(context.Background(), m.Reference.ChatID, m.Reference.TopicID, m.Name, m.MIME, tg.data[m.Reference.MessageID], nil, m.Caption, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReceiveTelegramMessage(context.Background(), tg.messages[ref.MessageID], a); err != nil || got != nil {
		t.Fatal("retired identity resurrected", got, err)
	}
	i, k, _ := s.UploaderIdentity()
	receipt, err := s.DB.UploaderReceipt(i, k, fileID)
	if err != nil || receipt.State != "retired" {
		t.Fatal(receipt, err)
	}
	_, fresh := toolMessages(t, s, tg, a, plain, "image", true, ids.New(), 1, 1, 1)
	if got := consumeTool(t, s, a, fresh); got == nil || got.ID == f.ID {
		t.Fatal("retired content blocked new explicit upload")
	}
}

func TestUploaderBadWireHashFailsActualReadAndBadKeyFailsFast(t *testing.T) {
	s, tg, a, _ := toolEnv(t)
	plain := workflowPNG(t)
	_, msgs := toolMessages(t, s, tg, a, plain, "image", true, ids.New(), 1, 1, 1)
	var c map[string]any
	_ = json.Unmarshal([]byte(msgs[0].Caption), &c)
	c["wire_sha256"] = fmt.Sprintf("%064d", 0)
	b, _ := json.Marshal(c)
	msgs[0].Caption = string(b)
	f := consumeTool(t, s, a, msgs)
	if f == nil {
		t.Fatal("declared original hash forced full read")
	}
	if _, err := s.Verify(context.Background(), f.ID); err == nil {
		t.Fatal("bad wire hash escaped restore validation")
	}
	_, bad := toolMessages(t, s, tg, a, append(bytes.Clone(plain), 1), "image", true, ids.New(), 1, 1, 1)
	msg := bad[0]
	wire := tg.data[msg.Reference.MessageID]
	r := bytes.NewReader(wire)
	h, err := carrier.OpenPart(r)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(r)
	h.File.KeyWrapped[0] ^= 1
	wire, err = carrier.EncodePart(workflowPNG(t), h, payload)
	if err != nil {
		t.Fatal(err)
	}
	tg.data[msg.Reference.MessageID] = wire
	_ = json.Unmarshal([]byte(msg.Caption), &c)
	c["wire_sha256"] = sha256Hex(wire)
	c["wire_size"] = len(wire)
	b, _ = json.Marshal(c)
	msg.Caption = string(b)
	msg.Reference.Size = int64(len(wire))
	if got, err := s.ReceiveTelegramMessage(context.Background(), msg, a); err == nil || got != nil {
		t.Fatal("bad wrapping accepted", got, err)
	}
}

func TestUploaderDeleteRetiresDuplicateAliasesAndKeepsLogs(t *testing.T) {
	s, tg, a, b := toolEnv(t)
	plain := workflowPNG(t)
	id, msgs := toolMessages(t, s, tg, a, plain, "image", true, ids.New(), 1, 1, 1)
	kept := consumeTool(t, s, a, msgs)
	aliasID, alias := toolMessages(t, s, tg, b, plain, "image", true, ids.New(), 1, 1, 1)
	consumeTool(t, s, b, alias)
	if err := s.RunPurgeFile(context.Background(), kept.ID); err != nil {
		t.Fatal(err)
	}
	instance, keyset, _ := s.UploaderIdentity()
	for _, fileID := range []string{id, aliasID} {
		receipt, err := s.DB.UploaderReceipt(instance, keyset, fileID)
		if err != nil || receipt.State != "retired" {
			t.Fatal("alias lost tombstone", receipt, err)
		}
	}
	m := alias[0]
	tg.mu.Lock()
	wire := bytes.Clone(tg.data[m.Reference.MessageID])
	tg.mu.Unlock()
	ref, err := tg.workflowTG.Upload(context.Background(), m.Reference.ChatID, m.Reference.TopicID, m.Name, m.MIME, wire, nil, m.Caption, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ReceiveTelegramMessage(context.Background(), tg.messages[ref.MessageID], b)
	if err != nil || got != nil {
		t.Fatal("deleted duplicate resurrected", got, err)
	}
	events, err := s.DB.ListIngestEvents(kept.ID, "", "", 0)
	if err != nil || len(events) < 4 {
		t.Fatal("deletion removed audit trail", len(events), err)
	}
}

func TestUploaderVideoCounterFailureRollsBackEntireGroup(t *testing.T) {
	s, tg, a, _ := toolEnv(t)
	plain := videoStoreFixture(t)
	upload := ids.New()
	id1, one := toolMessages(t, s, tg, a, plain, "video", false, upload, 1, 2, 1)
	consumeTool(t, s, a, one)
	id2, two := toolMessages(t, s, tg, a, plain, "video", false, upload, 2, 2, 1)
	if _, err := s.DB.Exec(`INSERT INTO file_name_counters(date,last_sequence) VALUES('20000101',9998)`); err != nil {
		t.Fatal(err)
	}
	for i, m := range two {
		_, err := s.ReceiveTelegramMessage(context.Background(), m, a)
		if i == len(two)-1 && err == nil {
			t.Fatal("exhausted counter accepted partial group")
		}
		if i < len(two)-1 && err != nil {
			t.Fatal(err)
		}
	}
	var count, counter int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE id IN (?,?)`, id1, id2).Scan(&count)
	_ = s.DB.QueryRow(`SELECT last_sequence FROM file_name_counters WHERE date='20000101'`).Scan(&counter)
	if count != 0 || counter != 9998 {
		t.Fatal("counter failure partially committed", count, counter)
	}
	if _, err := s.DB.Exec(`UPDATE file_name_counters SET last_sequence=9997 WHERE date='20000101'`); err != nil {
		t.Fatal(err)
	}
	consumeTool(t, s, a, two)
	f1, _ := s.DB.GetFile(id1)
	f2, _ := s.DB.GetFile(id2)
	if f1 == nil || f2 == nil || f1.Naming.Sequence != 9998 || f2.Naming.Sequence != 9999 {
		t.Fatal("retry lost group order", f1, f2)
	}
}
