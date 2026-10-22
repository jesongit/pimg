package mediax

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestVideoSplitPreservesUnknownTimecodeTrack(t *testing.T) {
	ctx := context.Background()
	source := testVideo(t, "libx264")
	timed := filepath.Join(t.TempDir(), "timecode.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-i", source, "-map", "0", "-c", "copy", "-timecode", "01:02:03:00", timed).CombinedOutput(); err != nil {
		t.Fatalf("timecode fixture: %v %s", err, out)
	}
	before, err := os.ReadFile(timed)
	if err != nil {
		t.Fatal(err)
	}
	info, err := InspectVideo(ctx, timed)
	if err != nil || !info.UnknownData {
		t.Fatal("fixture has no unknown timecode track", info, err)
	}
	parts, err := SplitVideo(ctx, timed, "mp4", t.TempDir(), int64(len(before))*5/8)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatal("video was not split")
	}
	var data []byte
	video := 0
	for _, part := range parts {
		if filepath.Ext(part) != ".mov" {
			t.Fatal("fallback container not recorded", part)
		}
		video += videoPackets(t, part)
		out, e := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-i", part, "-map", "0:d?", "-c", "copy", "-f", "data", "-").Output()
		if e == nil {
			data = append(data, out...)
		} // A sparse timecode has one packet in the first segment.
	}
	want, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-i", timed, "-map", "0:d", "-c", "copy", "-f", "data", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !bytes.Equal(data, want) {
		t.Fatal("timecode data packets were dropped or changed", data, want)
	}
	if video != videoPackets(t, timed) {
		t.Fatal("video packets changed")
	}
	after, err := os.ReadFile(timed)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source was rewritten", err)
	}
}

func testVideo(t *testing.T, codec string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	path := filepath.Join(t.TempDir(), "sample.mp4")
	args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-f", "lavfi", "-i", "sine=frequency=900:sample_rate=48000", "-t", "6", "-c:v", codec, "-g", "24", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	return path
}

func videoPackets(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets", "-show_entries", "stream=nb_read_packets", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Streams []struct {
			Packets string `json:"nb_read_packets"`
		}
	}
	if err = json.Unmarshal(out, &result); err != nil || len(result.Streams) != 1 {
		t.Fatal("invalid packet count", string(out), err)
	}
	n, err := strconv.Atoi(result.Streams[0].Packets)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVideoSplitBalancedIndependentAndPacketComplete(t *testing.T) {
	source := testVideo(t, "libx264")
	stat, _ := os.Stat(source)
	budget := stat.Size()/2 + stat.Size()/8
	parts, err := SplitVideo(context.Background(), source, "mp4", filepath.Join(t.TempDir(), "parts"), budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected minimum 2 segments, got %d", len(parts))
	}
	packets := 0
	var totalDuration float64
	sizes := []int64{}
	for _, path := range parts {
		stat, err := os.Stat(path)
		if err != nil || stat.Size() > budget {
			t.Fatalf("oversized %v %v", stat, err)
		}
		sizes = append(sizes, stat.Size())
		info, err := InspectVideo(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		totalDuration += info.Duration
		packets += videoPackets(t, path)
	}
	if packets != videoPackets(t, source) {
		t.Fatal("video packets lost or duplicated at cuts")
	}
	if math.Abs(totalDuration-6) > 0.5 {
		t.Fatalf("unexpected segment timeline %f", totalDuration)
	}
	if float64(max64(sizes[0], sizes[1]))/float64(min(sizes[0], sizes[1])) > 1.5 {
		t.Fatalf("unbalanced VBR segments %v", sizes)
	}
	whole, err := SplitVideo(context.Background(), source, "mp4", t.TempDir(), stat.Size())
	if err != nil || len(whole) != 1 || whole[0] != source {
		t.Fatal("fitting video was unnecessarily split")
	}
}

func TestVideoPlaybackNormalizesUnsupportedCodec(t *testing.T) {
	source := testVideo(t, "mpeg4")
	info, err := InspectVideo(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if info.PlayableMP4("mp4") {
		t.Fatal("unsupported codec accepted")
	}
	dest := filepath.Join(t.TempDir(), "playable.mp4")
	if err := PreparePlayableVideo(context.Background(), source, dest, info); err != nil {
		t.Fatal(err)
	}
	info, err = InspectVideo(context.Background(), dest)
	if err != nil || !info.PlayableMP4("mp4") {
		t.Fatalf("not playable: %+v %v", info, err)
	}
}

func TestVideoBalancedRangesMinimizeLargestGroup(t *testing.T) {
	groups := []videoGOP{{Bytes: 5}, {Bytes: 1}, {Bytes: 7}, {Bytes: 2}, {Bytes: 8}, {Bytes: 1}}
	ranges := balancedVideoRanges(groups, 3)
	var largest int64
	for _, span := range ranges {
		var sum int64
		for _, g := range groups[span[0]:span[1]] {
			sum += g.Bytes
		}
		largest = max64(largest, sum)
	}
	if largest != 9 {
		t.Fatalf("expected optimum 9, got %d: %v", largest, ranges)
	}
}

func TestVideoLongGOPRejectsLossyFallback(t *testing.T) {
	source := testVideo(t, "libx264")
	long := filepath.Join(t.TempDir(), "long-gop.mp4")
	out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-i", source, "-c:v", "libx264", "-g", "1000", "-keyint_min", "1000", "-sc_threshold", "0", "-c:a", "copy", long).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	before, err := os.ReadFile(long)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	parts, err := SplitVideo(context.Background(), long, "mp4", dest, int64(len(before))/2)
	if err == nil || len(parts) != 0 || !strings.Contains(err.Error(), "无损") {
		t.Fatalf("accepted lossy fallback: %v %v", parts, err)
	}
	after, _ := os.ReadFile(long)
	if string(after) != string(before) {
		t.Fatal("source changed")
	}
	entries, _ := os.ReadDir(dest)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "recoded") {
			t.Fatal("archive was silently reencoded")
		}
	}
}

func TestVideoNormalizationPreservesAudioAndRejectsUnrepresentableTracks(t *testing.T) {
	source := testVideo(t, "libx264")
	dual := filepath.Join(t.TempDir(), "two-audio.mkv")
	out, err := exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-i", source, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-map", "0", "-map", "1:a:0", "-c:v", "copy", "-c:a:0", "copy", "-c:a:1", "pcm_s16le", dual).CombinedOutput()
	if err != nil {
		t.Fatalf("multi audio fixture: %v %s", err, out)
	}
	info, err := InspectVideo(context.Background(), dual)
	if err != nil || info.AudioStreams != 2 || info.AllAudioCompatible {
		t.Fatalf("audio inventory wrong %+v %v", info, err)
	}
	dest := filepath.Join(t.TempDir(), "all-audio.mp4")
	if err := PreparePlayableVideo(context.Background(), dual, dest, info); err != nil {
		t.Fatal(err)
	}
	info, err = InspectVideo(context.Background(), dest)
	if err != nil || info.AudioStreams != 2 || !info.AllAudioCompatible {
		t.Fatalf("audio track lost or not normalized %+v %v", info, err)
	}
	if !HasFastStart(dest) {
		t.Fatal("playback index still at end")
	}
	sub := filepath.Join(t.TempDir(), "subtitle.srt")
	_ = os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,000\nPreserve this subtitle\n"), 0600)
	extra := filepath.Join(t.TempDir(), "subtitled.mkv")
	out, err = exec.Command("ffmpeg", "-nostdin", "-y", "-v", "error", "-i", source, "-i", sub, "-map", "0", "-map", "1", "-c", "copy", extra).CombinedOutput()
	if err != nil {
		t.Fatalf("subtitle fixture: %v %s", err, out)
	}
	info, err = InspectVideo(context.Background(), extra)
	if err != nil || !info.ExtraTracks {
		t.Fatal("subtitle omitted from inventory", err)
	}
	if err := PreparePlayableVideo(context.Background(), extra, filepath.Join(t.TempDir(), "must-not-lose.mp4"), info); err == nil {
		t.Fatal("subtitle silently discarded")
	}
}
