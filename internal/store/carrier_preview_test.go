package store

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

type coverRecorderTG struct {
	*workflowTG
	thumbnail []byte
}

func (r *coverRecorderTG) Upload(ctx context.Context, chat int64, topic int, name, mime string, data, thumb []byte, caption string, random int64) (tguser.Reference, error) {
	r.thumbnail = append([]byte(nil), thumb...)
	return r.workflowTG.Upload(ctx, chat, topic, name, mime, data, thumb, caption, random)
}

type coverStreamTG struct{ *coverRecorderTG }

func (r *coverStreamTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, input io.Reader, size int64, thumb []byte, caption string, random int64, _ *tguser.VideoMetadata) (tguser.Reference, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return tguser.Reference{}, err
	}
	if int64(len(data)) != size {
		return tguser.Reference{}, io.ErrUnexpectedEOF
	}
	return r.Upload(ctx, chat, topic, name, mime, data, thumb, caption, random)
}

func (r *coverStreamTG) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	data, err := r.Download(ctx, ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func coverPattern(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 320; x++ {
			c := color.RGBA{R: 35, G: 35, B: 35, A: 255}
			if x < 160 {
				c.R = 180
			} else {
				c.B = 180
			}
			if y >= 80 {
				c.G = 160
			}
			if (x/4+y/4)%2 == 0 {
				c.R += 50
				c.G += 50
				c.B += 50
			}
			img.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func coverRoughness(img image.Image) float64 {
	b := img.Bounds()
	var sum float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X + 1; x < b.Max.X; x++ {
			a, ag, ab, _ := img.At(x-1, y).RGBA()
			c, cg, cb, _ := img.At(x, y).RGBA()
			sum += math.Abs(float64(a)-float64(c)) + math.Abs(float64(ag)-float64(cg)) + math.Abs(float64(ab)-float64(cb))
		}
	}
	return sum / float64(b.Dy()*(b.Dx()-1))
}

func assertBlurredCover(t *testing.T, data []byte, clear image.Image) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 256 || b.Dy() != 128 {
		t.Fatal("cover must retain source aspect and stay within 256 px", b)
	}
	r, _, blue, _ := img.At(20, 20).RGBA()
	r2, _, blue2, _ := img.At(230, 20).RGBA()
	if r <= blue || r2 >= blue2 {
		t.Fatal("cover lost source colors or became a uniform placeholder")
	}
	if got, sharp := coverRoughness(img), coverRoughness(clear); got >= sharp*0.6 {
		t.Fatal("visible cover must be blurred, not a clear thumbnail", got, sharp)
	}
	return img
}

func TestEncryptedArchiveCarriesBlurredPreview(t *testing.T) {
	for _, mode := range []string{"local", "telegram-buffer", "telegram-stream"} {
		t.Run(mode, func(t *testing.T) {
			s, remote := workflowEnv(t)
			recorder := &coverRecorderTG{workflowTG: remote}
			s.UserTG = recorder
			var album sql.NullInt64
			if mode == "local" {
				if err := s.SetStorage("local", "local"); err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "telegram-stream" {
					s.UserTG = &coverStreamTG{recorder}
				}
				id, _ := workflowBound(t, s, remote)
				album = sql.NullInt64{Int64: id, Valid: true}
			}
			plain := coverPattern(t)
			f, err := s.receiveLegacyUploadFixture(t.Context(), bytes.NewReader(plain), "cover.png", "png", album, nil, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			clear, err := mediax.MakePreview(s.plainPath(f.ID), "image", 256)
			if err != nil {
				t.Fatal(err)
			}
			clearImage, _, err := image.Decode(bytes.NewReader(clear.Data))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RunArchive(t.Context(), f.ID); err != nil {
				t.Fatal(err)
			}
			parts, err := s.DB.ListChunks(f.ID, "data")
			if err != nil || len(parts) != 1 {
				t.Fatal("only one complete archive is expected", parts, err)
			}
			var wire []byte
			if mode == "local" {
				backend, err := s.cold(f)
				if err != nil {
					t.Fatal(err)
				}
				input, err := backend.Open(t.Context(), storage.Reference{Key: parts[0].ObjectKey})
				if err != nil {
					t.Fatal(err)
				}
				wire, err = io.ReadAll(input)
				input.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				ref, err := s.TGReference(parts[0])
				if err != nil {
					t.Fatal(err)
				}
				wire = remote.data[ref.MessageID]
				if len(remote.data) != 1 {
					t.Fatal("blur must not become a separate TG document", len(remote.data))
				}
				if _, err := jpeg.Decode(bytes.NewReader(recorder.thumbnail)); err != nil {
					t.Fatal("TG document thumbnail was not attached as JPEG", err)
				}
				assertBlurredCover(t, recorder.thumbnail, clearImage)
			}
			preview, _, _, err := carrier.DecodePart(wire)
			if err != nil {
				t.Fatal(err)
			}
			assertBlurredCover(t, preview, clearImage)
			var recovered bytes.Buffer
			if _, err := carrier.Recover(bytes.NewReader(wire), &recovered, s.Master); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
				t.Fatal("blurred cover changed lossless offline recovery", err)
			}
			// Archive completion removes the upload original. Local preview must
			// still provide a blurred cover for later replicas without remote reads.
			f, _ = s.DB.GetFile(f.ID)
			before := remote.downloadCalls
			fallback, thumbnail := s.carrierPreview(t.Context(), f)
			assertBlurredCover(t, fallback, clearImage)
			assertBlurredCover(t, thumbnail, clearImage)
			if remote.downloadCalls != before {
				t.Fatal("cover fallback fetched a remote original")
			}
			if dir := os.Getenv("PIMG_COVER_TEST_OUTPUT"); dir != "" && mode == "telegram-stream" {
				for name, data := range map[string][]byte{"synthetic-source.png": plain, "carrier-cover.png": preview, "telegram-thumbnail.jpg": recorder.thumbnail} {
					if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestCarrierPreviewUsesPlaceholderOnlyWithoutReadableMedia(t *testing.T) {
	s, _, _ := newTestEnv(t)
	preview, thumb := s.carrierPreview(t.Context(), &dbx.File{ID: "missing", Kind: "image"})
	if !bytes.Equal(preview, privacyPlaceholder()) {
		t.Fatal("missing media must use the neutral cover")
	}
	if _, err := jpeg.Decode(bytes.NewReader(thumb)); err != nil {
		t.Fatal("placeholder must also have a valid TG thumbnail", err)
	}
}
