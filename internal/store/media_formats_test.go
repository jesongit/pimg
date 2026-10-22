package store

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMediaAddedVideoContainersKeepExactSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, ext := range []string{"mov", "ts", "m2ts", "avi", "flv"} {
		t.Run(ext, func(t *testing.T) {
			s, _, _ := peerFixture(t)
			s.SetDynamic(-1, -1, -1, []string{ext})
			policy := s.StoragePolicy()
			policy.Archives = []string{"local"}
			if err := s.SaveStoragePolicy(policy); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "source."+ext)
			args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=12", "-t", "2", "-c:v", "libx264", "-g", "12", "-pix_fmt", "yuv420p"}
			if ext == "avi" {
				args = append(args, "-c:v", "mpeg4")
			}
			if ext == "flv" {
				args = append(args, "-c:v", "flv1")
			}
			if ext == "ts" || ext == "m2ts" {
				args = append(args, "-f", "mpegts")
			}
			args = append(args, source)
			if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, output)
			}
			plain, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithPrivateUpload(context.Background())
			f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "source."+ext, ext, sql.NullInt64{}, nil, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RunDirectArchive(ctx, f.ID); err != nil {
				t.Fatal(err)
			}
			if f.Ext != ext || f.Kind != "video" || f.SHA256 != sha256Hex(plain) {
				t.Fatal("source format or identity changed")
			}
			input, err := s.SourceStream(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(input)
			input.Close()
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatal("archive changed bytes", err)
			}
			f, _ = s.DB.GetFile(f.ID)
			view, _, err := s.OpenMediaView(ctx, f, 0, "transcode", true)
			if err != nil {
				t.Fatal("compatible viewing failed", err)
			}
			view.Close()
			input, err = s.SourceStream(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(input)
			input.Close()
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatal("compatible viewing replaced the original", err)
			}
		})
	}
}

func TestMediaJPEGKeepsExactSource(t *testing.T) {
	s, _, _ := peerFixture(t)
	s.SetDynamic(-1, -1, -1, []string{"jpeg"})
	var source bytes.Buffer
	if err := jpeg.Encode(&source, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(source.Bytes()), "source.jpeg", "jpeg", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	input, err := s.SourceStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(got, source.Bytes()) {
		t.Fatal("JPEG archive changed", err)
	}
}
