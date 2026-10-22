package mediax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// VideoInfo is obtained from the actual streams, rather than a filename.
type VideoInfo struct {
	HDR                                    bool
	Width, Height                          int
	Duration, Start                        float64
	VideoStream                            int
	VideoCodec, AudioCodec                 string
	PixelFormat                            string
	VideoStreams, AudioStreams             int
	AllVideoCompatible, AllAudioCompatible bool
	ExtraTracks                            bool
	UnknownData                            bool // MP4 cannot mux e.g. copied tmcd data; MOV preserves its original tag.
	Format                                 string
}

func InspectVideo(ctx context.Context, path string) (VideoInfo, error) {
	var info VideoInfo
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=index,codec_type,codec_name,pix_fmt,width,height,color_transfer:format=duration,start_time,format_name", "-of", "json", path).Output()
	if err != nil {
		return info, errors.New("无法读取视频，请确认 ffmpeg 和 ffprobe 已安装且文件有效")
	}
	var result struct {
		Streams []struct {
			Index         int
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			PixFmt        string `json:"pix_fmt"`
			ColorTransfer string `json:"color_transfer"`
			Width, Height int
		}
		Format struct {
			Duration  string
			StartTime string `json:"start_time"`
			Name      string `json:"format_name"`
		}
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return info, err
	}
	info.Duration, _ = strconv.ParseFloat(result.Format.Duration, 64)
	info.Start, _ = strconv.ParseFloat(result.Format.StartTime, 64)
	info.Format = result.Format.Name
	info.AllVideoCompatible, info.AllAudioCompatible = true, true
	for _, s := range result.Streams {
		switch s.CodecType {
		case "video":
			info.HDR = info.HDR || s.ColorTransfer == "smpte2084" || s.ColorTransfer == "arib-std-b67"
			info.VideoStreams++
			if s.CodecName != "h264" || (s.PixFmt != "yuv420p" && s.PixFmt != "yuvj420p") {
				info.AllVideoCompatible = false
			}
		case "audio":
			info.AudioStreams++
			if s.CodecName != "aac" {
				info.AllAudioCompatible = false
			}
		default:
			info.ExtraTracks = true
			if s.CodecType == "data" && (s.CodecName == "" || s.CodecName == "unknown") {
				info.UnknownData = true
			}
		}
		if s.CodecType == "video" && info.VideoCodec == "" {
			info.VideoStream, info.VideoCodec, info.PixelFormat = s.Index, s.CodecName, s.PixFmt
			info.Width, info.Height = s.Width, s.Height
		} else if s.CodecType == "audio" && info.AudioCodec == "" {
			info.AudioCodec = s.CodecName
		}
	}
	if info.VideoCodec == "" || info.Width <= 0 || info.Height <= 0 || info.Duration <= 0 || math.IsNaN(info.Duration) || math.IsInf(info.Duration, 0) {
		return info, errors.New("视频没有有效画面或时长")
	}
	return info, nil
}

// PlayableMP4 deliberately chooses a common profile for Telegram and browsers.
// Other valid files are normalized once; encryption alone never transcodes.
func (v VideoInfo) PlayableMP4(ext string) bool {
	return (ext == "mp4" || ext == "m4v") && v.AllVideoCompatible && v.AllAudioCompatible
}

func HasFastStart(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for range 256 {
		var atom [8]byte
		if _, err := io.ReadFull(f, atom[:]); err != nil {
			return false
		}
		size, header := int64(binary.BigEndian.Uint32(atom[:4])), int64(8)
		if size == 1 {
			var wide [8]byte
			if _, err := io.ReadFull(f, wide[:]); err != nil {
				return false
			}
			value := binary.BigEndian.Uint64(wide[:])
			if value > math.MaxInt64 {
				return false
			}
			size = int64(value)
			header = 16
		}
		if size < header {
			return false
		}
		switch string(atom[4:]) {
		case "moov":
			return true
		case "mdat":
			return false
		}
		if _, err := f.Seek(size-header, io.SeekCurrent); err != nil {
			return false
		}
	}
	return false
}

// PreparePlayableVideo copies compressed streams when possible. Incompatible
// codecs are converted to H.264/AAC, with a short closed GOP for safe cuts.
func PreparePlayableVideo(ctx context.Context, source, dest string, info VideoInfo) error {
	if info.ExtraTracks && !strings.Contains(info.Format, "mp4") {
		return errors.New("视频含字幕、附件或其他轨道，自动转 MP4 无法保证完整保留；请先处理这些轨道后重试，未导入或删除来源")
	}
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-i", source, "-map", "0:v", "-map", "0:a?"}
	if info.ExtraTracks {
		args = []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-i", source, "-map", "0", "-c", "copy"}
	}
	if info.AllVideoCompatible {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p", "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", "-force_key_frames", "expr:gte(t,n_forced*2)")
	}
	if info.AllAudioCompatible {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	args = append(args, "-movflags", "+faststart", dest)
	return runVideoCommand(ctx, args)
}

type videoGOP struct {
	Start float64
	Bytes int64
}

// videoGOPs streams ffprobe's packet table. Memory grows with keyframes, not
// video bytes or individual packets; audio bytes count toward the same limit.
func videoGOPs(ctx context.Context, source string, info VideoInfo) ([]videoGOP, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,pts_time,size,flags", "-of", "compact=p=0", source)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	groups := []videoGOP{{Start: 0}}
	seenVideoKey := false
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var stream int
		var size int64
		var pts float64
		var key bool
		for _, field := range strings.Split(scan.Text(), "|") {
			name, value, _ := strings.Cut(field, "=")
			switch name {
			case "stream_index":
				stream, _ = strconv.Atoi(value)
			case "pts_time":
				pts, _ = strconv.ParseFloat(value, 64)
			case "size":
				size, _ = strconv.ParseInt(value, 10, 64)
			case "flags":
				key = strings.Contains(value, "K")
			}
		}
		pts -= info.Start
		if stream == info.VideoStream && key {
			if seenVideoKey && pts > groups[len(groups)-1].Start+0.00001 && groups[len(groups)-1].Bytes > 0 {
				groups = append(groups, videoGOP{Start: pts})
			}
			seenVideoKey = true
		}
		if size > 0 {
			groups[len(groups)-1].Bytes += size
		}
	}
	readErr := scan.Err()
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	if err := errors.Join(readErr, cmd.Wait()); err != nil {
		return nil, errors.New("无法读取视频关键帧，未生成不完整片段")
	}
	return groups, nil
}

// balancedVideoRanges minimizes the largest contiguous packet payload for n
// independently decodable parts. Boundaries are always real keyframes.
func balancedVideoRanges(groups []videoGOP, n int) [][2]int {
	if n < 1 || n > len(groups) {
		return nil
	}
	var lo, hi int64
	for _, g := range groups {
		lo = max64(lo, g.Bytes)
		hi += g.Bytes
	}
	needed := func(cap int64) int {
		count := 1
		var size int64
		for _, g := range groups {
			if size+g.Bytes > cap {
				count++
				size = 0
			}
			size += g.Bytes
		}
		return count
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if needed(mid) <= n {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	ranges := make([][2]int, n)
	end := len(groups)
	for part := n - 1; part >= 0; part-- {
		start := end
		var size int64
		for start > part && size+groups[start-1].Bytes <= lo {
			start--
			size += groups[start].Bytes
		}
		ranges[part] = [2]int{start, end}
		end = start
	}
	return ranges
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// SplitVideo returns standalone videos using stream copy and measured packet
// sizes. MP4 timed metadata may require MOV parts. Every file is checked on disk. A
// single GOP above the limit requires a new keyframe layout, never byte slicing.
func SplitVideo(ctx context.Context, source, ext, outputDir string, maxBytes int64) ([]string, error) {
	return splitVideo(ctx, source, ext, outputDir, maxBytes, false)
}

func splitVideo(ctx context.Context, source, ext, outputDir string, maxBytes int64, recoded bool) ([]string, error) {
	if maxBytes <= 0 {
		return nil, errors.New("视频分片大小无效")
	}
	st, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if st.Size() <= maxBytes {
		return []string{source}, nil
	}
	info, err := InspectVideo(ctx, source)
	if err != nil {
		return nil, err
	}
	if info.VideoStreams > 1 {
		return nil, errors.New("视频含多路画面，无法保证各轨道在相同关键帧独立切分；请先拆分画面轨道，来源未导入或删除")
	}
	groups, err := videoGOPs(ctx, source, info)
	if err != nil {
		return nil, err
	}
	// Timed metadata may be legal in the input MP4 yet unsupported by FFmpeg's
	// MP4 muxer. MOV carries those tagged packets without dropping or encoding
	// streams. The source and its original container remain untouched.
	if info.UnknownData && (ext == "mp4" || ext == "m4v") {
		ext = "mov"
	}
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return nil, err
	}
	n := int((st.Size() + maxBytes - 1) / maxBytes)
	n = max(2, n)
	reencode := func() ([]string, error) {
		return nil, errors.New("无法在容量上限内无损切分此视频；来源已保留，未重新编码，请调整该存储的分片大小")
	}

	for _, g := range groups {
		if g.Bytes > maxBytes {
			return reencode()
		}
	}
	if n > len(groups) {
		return reencode()
	}
	// A single segment muxer invocation preserves packet continuity at cuts.
	for ; n <= len(groups); n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ranges := balancedVideoRanges(groups, n)
		var times []string
		for _, span := range ranges[1:] {
			times = append(times, strconv.FormatFloat(groups[span[0]].Start, 'f', 6, 64))
		}
		pattern := filepath.Join(outputDir, "part-%05d."+ext)
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-i", source, "-map", "0", "-copy_unknown", "-c", "copy", "-f", "segment", "-reference_stream", "v:0", "-segment_times", strings.Join(times, ","), "-segment_time_delta", "0.000001", "-reset_timestamps", "1", "-avoid_negative_ts", "make_zero", "-segment_start_number", "0"}
		if ext == "mp4" || ext == "m4v" || ext == "mov" {
			args = append(args, "-segment_format_options", "movflags=+faststart")
		}
		args = append(args, pattern)
		if err := runVideoCommand(ctx, args); err != nil {
			return nil, err
		}
		actual, err := filepath.Glob(filepath.Join(outputDir, "part-*."+ext))
		if err != nil {
			return nil, err
		}
		paths := make([]string, n)
		valid := len(actual) == n
		for i := range paths {
			paths[i] = filepath.Join(outputDir, fmt.Sprintf("part-%05d.%s", i, ext))
			part, err := os.Stat(paths[i])
			if err != nil || part.Size() <= 0 || part.Size() > maxBytes {
				valid = false
				continue
			}
			if _, err := InspectVideo(ctx, paths[i]); err != nil {
				return nil, fmt.Errorf("第 %d 个视频片段无效，已停止归档", i+1)
			}
		}
		if valid {
			return paths, nil
		}
		// A different count must not leave stale files in the result manifest.
		for _, path := range actual {
			_ = os.Remove(path)
		}
	}
	return reencode()
}

func runVideoCommand(ctx context.Context, args []string) error {
	var diagnostic limitedVideoDiagnostic
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg 视频处理失败: %s", truncate(diagnostic.Bytes(), 400))
	}
	return nil
}

type limitedVideoDiagnostic struct{ bytes.Buffer }

func (b *limitedVideoDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		_, _ = b.Buffer.Write(p[:min(len(p), 4096-b.Len())])
	}
	return n, nil
}
