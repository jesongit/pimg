package mediax

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCompatibleAudioKeepsOriginalVideoPackets(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	source, dest := filepath.Join(root, "audio.mov"), filepath.Join(root, "view.mp4")
	args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=8", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "pcm_s16le", source}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	before, _ := os.ReadFile(source)
	var seen []float64
	if err := MakeCompatibleProgress(t.Context(), source, dest, "video", "transcode", func(p float64) { seen = append(seen, p) }); err != nil {
		t.Fatal(err)
	}
	info, err := InspectVideo(t.Context(), dest)
	if err != nil || info.VideoCodec != "h264" || info.AudioCodec != "aac" {
		t.Fatal(info, err)
	}
	packetHashes := func(p string) []byte {
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=data_hash", "-of", "csv=p=0", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !bytes.Equal(packetHashes(source), packetHashes(dest)) {
		t.Fatal("compatible video picture was unnecessarily re-encoded")
	}
	if len(seen) == 0 || seen[len(seen)-1] <= 0 {
		t.Fatal("conversion did not report actual progress", seen)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) {
		t.Fatal("source modified")
	}
	forced := filepath.Join(root, "forced.mp4")
	if err := MakeCompatible(t.Context(), dest, forced, "video", "transcode"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(packetHashes(dest), packetHashes(forced)) {
		t.Fatal("explicit video fallback must still re-encode a nominally compatible picture")
	}
}

func TestCompatibleReadOnlyDescriptorSurvivesPathReplacement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux inherited descriptor path")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	source, dest := filepath.Join(root, "video.mkv"), filepath.Join(root, "view.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=8", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source).CombinedOutput(); err != nil {
		t.Fatalf("fixture %v %s", err, out)
	}
	original, _ := os.ReadFile(source)
	file, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = os.Rename(source, source+".moved"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte("replaced pathname must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	var seen []float64
	if err = MakeCompatibleFile(t.Context(), file, dest, "remux", func(p float64) { seen = append(seen, p) }); err != nil {
		t.Fatal(err)
	}
	if info, err := InspectVideo(t.Context(), dest); err != nil || !info.PlayableMP4("mp4") {
		t.Fatal(info, err)
	}
	after, _ := os.ReadFile(source + ".moved")
	if !bytes.Equal(original, after) || len(seen) == 0 {
		t.Fatal("source changed or progress missing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = MakeCompatibleFile(ctx, file, dest, "remux", nil); err == nil {
		t.Fatal("cancelled conversion succeeded")
	}
}
