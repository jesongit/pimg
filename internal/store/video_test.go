package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

type videoWorkflowTG struct {
	*workflowTG
	limit                      int64
	streamUploads, streamOpens int
	videoUploads               int
}

func (v *videoWorkflowTG) Status() tguser.Status {
	s := v.workflowTG.Status()
	s.UploadLimitBytes = v.limit
	return s
}
func (v *videoWorkflowTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, r io.Reader, size int64, thumb []byte, caption string, random int64, video *tguser.VideoMetadata) (tguser.Reference, error) {
	v.streamUploads++
	data, err := io.ReadAll(r)
	if err != nil {
		return tguser.Reference{}, err
	}
	if int64(len(data)) != size || size > v.limit {
		return tguser.Reference{}, errors.New("wire size invalid")
	}
	if video != nil {
		v.videoUploads++
		if !video.SupportsStreaming || video.Duration <= 0 || video.Width <= 0 || mime != "video/mp4" {
			return tguser.Reference{}, errors.New("invalid playback metadata")
		}
	}
	return v.workflowTG.Upload(ctx, chat, topic, name, mime, data, thumb, caption, random)
}
func (v *videoWorkflowTG) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	v.streamOpens++
	data, err := v.workflowTG.Download(ctx, ref)
	return io.NopCloser(bytes.NewReader(data)), err
}

func videoStoreFixture(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	path := filepath.Join(t.TempDir(), "movie.mp4")
	out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24", "-t", "8", "-c:v", "libx264", "-b:v", "2M", "-minrate", "2M", "-maxrate", "2M", "-bufsize", "1M", "-x264-params", "nal-hrd=cbr:force-cfr=1", "-g", "24", "-pix_fmt", "yuv420p", "-movflags", "+faststart", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg fixture: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVideoReplicaKeepsOneIndexAndIndependentLayouts(t *testing.T) {
	data := videoStoreFixture(t)
	s, base := workflowEnv(t)
	transport := &videoWorkflowTG{workflowTG: base, limit: 1 << 20}
	s.UserTG = transport
	i, _ := s.instance("local")
	c := i.config
	c.Priority = 10
	if _, err := s.SaveStorageInstance("local", i.row.Name, "local", c, true); err != nil {
		t.Fatal(err)
	}
	p := s.StoragePolicy()
	p.Archives = []string{"local", "telegram"}
	p.Permanent = "local"
	p.Temporary = "local"
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	ctx := WithPrivateUpload(context.Background())
	files, err := s.ReceiveUploadBatch(ctx, bytes.NewReader(data), "replicated.mp4", "mp4", sql.NullInt64{}, nil, 0, true)
	if err != nil || len(files) != 1 {
		t.Fatal("one source must have one gallery record", err, len(files))
	}
	f := files[0]
	if err = s.RunDirectArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tgParts int
	for _, r := range all {
		if r.State != "ready" {
			t.Fatal(r)
		}
		if r.InstanceID == "local" {
			if len(r.Parts) != 1 || !r.Parts[0].Original || r.Parts[0].SHA256 != sha256Hex(data) {
				t.Fatal("local original rewritten", r)
			}
		} else if r.InstanceID == "telegram" {
			tgParts = len(r.Parts)
			for _, part := range r.Parts {
				if part.Original || part.Count != tgParts || part.StoredSize > transport.limit {
					t.Fatal("invalid TG representation", part)
				}
			}
		}
	}
	if tgParts < 2 || transport.videoUploads != tgParts {
		t.Fatal("TG did not retain independently playable segments", tgParts, transport.videoUploads)
	}
	input, err := s.SourceStream(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(data, restored) {
		t.Fatal("local source changed", err)
	}
	_, total, err := s.DB.ListFiles(dbx.FileFilter{PerPage: 100})
	if err != nil || total != 1 {
		t.Fatal("storage segments became gallery records", total, err)
	}
}

func TestTelegramVideoPartsRoundTripWithoutLossyFallback(t *testing.T) {
	data := videoStoreFixture(t)
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprint("direct=", direct), func(t *testing.T) {
			s, base := workflowEnv(t)
			transport := &videoWorkflowTG{workflowTG: base, limit: 1 << 20}
			s.UserTG = transport
			p := s.StoragePolicy()
			p.Archives = []string{"telegram"}
			if err := s.SaveStoragePolicy(p); err != nil {
				t.Fatal(err)
			}
			ctx := WithPrivateUpload(context.Background())
			ctx = context.WithValue(ctx, telegramSourceKey{}, fmt.Sprintf("11:77:%d", 10+len(data)))
			ctx = WithFileCreatedTime(ctx, time.Date(2000, 1, 1, 9, 0, 0, 0, time.FixedZone("test", 8*3600)))
			files, err := s.ReceiveUploadBatch(ctx, bytes.NewReader(data), "movie.mp4", "mp4", sql.NullInt64{}, nil, 0, direct)
			if err != nil || len(files) != 1 {
				t.Fatal("source was split into gallery files", err, len(files))
			}
			f := files[0]
			if f.OrigName != "movie.mp4" || f.SHA256 != sha256Hex(data) || f.Naming.Date != "20000101" || f.Naming.PartCount != 1 {
				t.Fatal("source identity changed", f.Naming)
			}
			if direct {
				err = s.RunDirectArchive(ctx, f.ID)
			} else {
				err = s.RunArchive(ctx, f.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, _ = s.DB.GetFile(f.ID)
			all, err := s.DB.MediaReplicas(f.ID)
			if err != nil || len(all) != 1 {
				t.Fatal(err)
			}
			r := all[0]
			if len(r.Parts) < 2 || r.Encrypted == direct {
				t.Fatal("actual TG policy missing", r)
			}
			var duration float64
			for index, part := range r.Parts {
				var ref storage.Reference
				if err = json.Unmarshal([]byte(part.Reference), &ref); err != nil {
					t.Fatal(err)
				}
				obj, err := s.telegramObject(ref.Key)
				if err != nil {
					t.Fatal(err)
				}
				wire := base.data[obj.MessageID]
				if int64(len(wire)) != part.StoredSize || part.StoredSize > transport.limit {
					t.Fatal("wire exceeded limit")
				}
				if !direct {
					if _, err = png.DecodeConfig(bytes.NewReader(wire)); err != nil {
						t.Fatal(err)
					}
					_, header, _, err := carrier.DecodePart(wire)
					if err != nil || header.File.Kind != "video" || header.File.Naming.PartIndex != index+1 || header.File.Naming.PartCount != len(r.Parts) {
						t.Fatal("carrier part identity missing", err)
					}
				}
				input, err := s.openMediaPart(ctx, f, r, part)
				if err != nil {
					t.Fatal(err)
				}
				restored, err := io.ReadAll(input)
				input.Close()
				if err != nil || sha256Hex(restored) != part.SHA256 {
					t.Fatal("part recovery failed", err)
				}
				path := filepath.Join(t.TempDir(), "part.mp4")
				if err = os.WriteFile(path, restored, 0600); err != nil {
					t.Fatal(err)
				}
				info, err := mediax.InspectVideo(ctx, path)
				if err != nil || info.VideoCodec != "h264" {
					t.Fatal("segment codec changed or cannot play", info, err)
				}
				duration += info.Duration
				local, got, err := s.OpenMediaView(ctx, f, index, "", true)
				if err != nil {
					t.Fatal(err)
				}
				local.Close()
				if got.SHA256 != part.SHA256 {
					t.Fatal("local view changed segment")
				}
			}
			if duration < 7.9 || duration > 8.8 {
				t.Fatal("timeline changed", duration)
			}
			if _, err = s.SourceStream(ctx, f); err == nil {
				t.Fatal("segments were advertised as byte-identical original")
			}
			if _, err = s.Verify(ctx, f.ID); err != nil {
				t.Fatal(err)
			}
			if transport.streamUploads == 0 || transport.streamOpens == 0 {
				t.Fatal("streaming transport bypassed")
			}
			sourceParts, err := s.DB.TelegramSourcePartIDs(f.TelegramSource)
			if err != nil || len(sourceParts) != 1 || sourceParts[0] != f.ID {
				t.Fatal("logical source mapping changed", sourceParts, err)
			}
		})
	}
}

func TestTelegramCompatibleVideoRemainsOneOriginalFile(t *testing.T) {
	data := videoStoreFixture(t)
	s, base := workflowEnv(t)
	s.UserTG = &videoWorkflowTG{workflowTG: base, limit: 4 << 20}
	files, err := s.ReceiveUploadBatch(context.Background(), bytes.NewReader(data), "original.mp4", "mp4", sql.NullInt64{}, nil, 0, true)
	if err != nil || len(files) != 1 {
		t.Fatal("fitting compatible video split", err)
	}
	if files[0].SHA256 != sha256Hex(data) || files[0].OrigName != "original.mp4" {
		t.Fatal("compatible faststart original was unnecessarily rewritten")
	}
}

func TestTelegramImageKeepsSingleMessageBeyondLegacyChunkSize(t *testing.T) {
	s, base := workflowEnv(t)
	transport := &videoWorkflowTG{workflowTG: base, limit: 64 << 20}
	s.UserTG = transport
	data := append(workflowPNG(t), bytes.Repeat([]byte{13}, 17<<20)...)
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(data), "large.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	chunks, err := s.DB.ListChunks(f.ID, "data")
	if err != nil || len(chunks) != 1 {
		t.Fatal("legacy chunk size still split a fitting document", err)
	}
	ref, _ := s.telegramObject(chunks[0].ObjectKey)
	if _, err := png.DecodeConfig(bytes.NewReader(base.data[ref.MessageID])); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPerStorageVideoPolicyDefaultsAndNoGlobalInheritance(t *testing.T) {
	s, _ := workflowEnv(t)
	for _, id := range []string{"local", "telegram"} {
		i, err := s.instance(id)
		if err != nil {
			t.Fatal(err)
		}
		if i.config.VideoPartMB != 256 {
			t.Fatal("default must be per-storage 256 MB", id, i.config.VideoPartMB)
		}
		if id == "local" && (i.config.SplitVideo || i.config.Encrypt) {
			t.Fatal("local must default to complete plaintext files")
		}
	}
	_ = s.DB.SetSetting("site.video_part_mb", "128")
	i, _ := s.instance("telegram")
	if i.config.VideoPartMB != 256 {
		t.Fatal("removed global configuration affected a storage")
	}
	f := dbx.File{ID: "file", SHA256: strings.Repeat("a", 64)}
	if budget := videoPlainBudget(&f, 256<<20); budget <= 0 || budget >= 256<<20 {
		t.Fatal("wire overhead not reserved")
	}
}
