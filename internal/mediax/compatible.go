package mediax

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// MakeCompatible only writes a disposable view. It never replaces an archive.
func MakeCompatible(ctx context.Context, source, dest, kind, mode string) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, BrowserCaps{}, nil)
}

// MakeCompatibleCaps converts for a specific browser capability set. The zero
// value keeps the universally playable H.264/AAC baseline.
func MakeCompatibleCaps(ctx context.Context, source, dest, kind, mode string, caps BrowserCaps) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, caps, nil)
}

// MakeCompatibleCapsProgress converts for a capability set while reporting
// 0..1 conversion progress.
func MakeCompatibleCapsProgress(ctx context.Context, source, dest, kind, mode string, caps BrowserCaps, progress func(float64)) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, caps, progress)
}

// MakeCompatibleProgress converts while reporting 0..1 conversion progress.
func MakeCompatibleProgress(ctx context.Context, source, dest, kind, mode string, progress func(float64)) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, BrowserCaps{}, progress)
}

// Linux subprocesses inherit the already validated read-only descriptor. They
// can seek without copying a multi-GB source or reopening a replaced pathname.
func MakeCompatibleFile(ctx context.Context, source *os.File, dest, mode string, caps BrowserCaps, progress func(float64)) error {
	if runtime.GOOS != "linux" {
		return errors.New("当前平台需要兼容处理快照")
	}
	return makeCompatible(ctx, "/proc/self/fd/3", source, dest, "video", mode, caps, progress)
}

// MakeCompatibleFileStream converts while writing fragmented MP4 to client, and
// caches the same output at dest so later opens serve it as a complete file.
// Fragmented output starts at the first keyframe, so playback no longer waits
// for the whole conversion. Linux subprocesses inherit the validated read-only
// descriptor; elsewhere the validated bytes are snapshotted once, because the
// handle's name is root-relative and unusable by a subprocess.
// MakeCompatibleFileFragments converts into a fragmented MP4 written directly
// to dest (the caller supplies a temporary path and renames on success).
// Fragmented output starts at the first keyframe, so a reader can follow the
// file while conversion runs. Encoding strategies fall back in order: VAAPI
// hardware first when a render node exists, then software x264.
func MakeCompatibleFileFragments(ctx context.Context, source *os.File, dest, mode string, caps BrowserCaps) error {
	if mode != "remux" && mode != "transcode" {
		return errors.New("此文件不能流式生成兼容查看版本")
	}
	sourcePath := source.Name()
	var snapshot string
	if runtime.GOOS == "linux" {
		sourcePath = "/proc/self/fd/3"
	} else {
		snap, err := os.CreateTemp(filepath.Dir(dest), "stream-src-*.part")
		if err != nil {
			return err
		}
		if _, err = io.Copy(snap, source); err != nil {
			snap.Close()
			return err
		}
		if err = snap.Close(); err != nil {
			return err
		}
		snapshot = snap.Name()
		defer os.Remove(snapshot)
		sourcePath = snapshot
	}
	command := func(name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		if runtime.GOOS == "linux" {
			cmd.ExtraFiles = []*os.File{source}
		}
		return cmd
	}
	info, err := inspectVideo(command("ffprobe", "-v", "error", "-show_entries", "stream=index,codec_type,codec_name,pix_fmt,width,height,color_transfer:format=duration,start_time,format_name", "-of", "json", sourcePath))
	if err != nil {
		return err
	}
	var lastErr error
	attempts := compatibleAttempts(info, mode, caps)
	for _, attempt := range attempts {
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1"}
		args = append(args, attempt.globals...)
		args = append(args, "-i", sourcePath)
		args = append(args, attempt.args...)
		args = append(args, "-f", "mp4", "-movflags", "+empty_moov+frag_keyframe+default_base_moof", dest)
		if err = runVideoCommand(ctx, args); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return lastErr
}

func makeCompatible(ctx context.Context, source string, file *os.File, dest, kind, mode string, caps BrowserCaps, progress func(float64)) error {
	command := func(name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		if file != nil {
			cmd.ExtraFiles = []*os.File{file}
		}
		return cmd
	}
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1", "-i", source}
	if kind == "image" {
		if isHEIF(source) {
			return heifPNG(ctx, source, dest, 8192)
		}
		args = append(args, "-frames:v", "1", "-f", "image2", "-c:v", "png", dest)
		return runVideoCommand(ctx, args)
	}
	if kind != "video" {
		return errors.New("此文件不能生成兼容查看版本")
	}
	info, err := inspectVideo(command("ffprobe", "-v", "error", "-show_entries", "stream=index,codec_type,codec_name,pix_fmt,width,height,color_transfer:format=duration,start_time,format_name", "-of", "json", source))
	if err != nil {
		return err
	}
	var lastErr error
	for _, attempt := range compatibleAttempts(info, mode, caps) {
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1"}
		args = append(args, attempt.globals...)
		args = append(args, "-i", source)
		args = append(args, attempt.args...)
		args = append(args, "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", "-f", "mp4", dest)
		cmd := command("ffmpeg", args...)
		var diagnostic limitedVideoDiagnostic
		cmd.Stderr = &diagnostic
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err = cmd.Start(); err != nil {
			return err
		}
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			key, value, _ := strings.Cut(scan.Text(), "=")
			if key == "out_time_us" && progress != nil {
				micros, _ := strconv.ParseFloat(value, 64)
				progress(math.Max(0, math.Min(0.99, micros/1e6/info.Duration)))
			}
		}
		if scan.Err() != nil {
			_ = cmd.Process.Kill()
		}
		if err = errors.Join(scan.Err(), cmd.Wait()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failed strategy (e.g. GPU init) falls back to the next one.
			lastErr = fmt.Errorf("ffmpeg 视频处理失败: %s", truncate(diagnostic.Bytes(), 20000))
			continue
		}
		return nil
	}
	return lastErr
}

// encodeAttempt is one encoding strategy: extra global options plus the stream
// mapping, filter and codec options that follow the input.
type encodeAttempt struct {
	globals []string
	args    []string
}

// hardwareEncode probes the VAAPI render node once: an Intel iGPU takes the
// x264-equivalent encode work and keeps 4K/HDR transcoding near realtime.
// Missing device (or non-Linux) keeps the software encoder.
var hardwareEncode = sync.OnceValue(func() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/dev/dri/renderD128")
	return err == nil
})

// compatibleAttempts lists the encoding strategies in fallback order. A
// supported picture is copied; only genuinely undecodable input is re-encoded,
// and the source resolution is preserved instead of a downscaled ceiling.
func compatibleAttempts(info VideoInfo, mode string, caps BrowserCaps) []encodeAttempt {
	maps := []string{"-map", "0:v:0", "-map", "0:a:0?"}
	if mode == "remux" {
		return []encodeAttempt{{args: append(maps, "-c", "copy")}}
	}
	videoCopy := !info.HDR && info.AllVideoSupported(caps)
	audioCopy := info.AllAudioSupported(caps)
	if videoCopy && !audioCopy {
		// An unsupported audio track does not require re-encoding the picture.
		return []encodeAttempt{{args: append(maps, "-c:v", "copy", "-c:a", "aac", "-b:a", "128k")}}
	}
	filter := "pad=ceil(iw/2)*2:ceil(ih/2)*2"
	if info.HDR {
		filter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv," + filter
	}
	audio := []string{"-c:a", "aac", "-b:a", "128k"}
	software := encodeAttempt{args: append(append(append([]string{}, maps...), "-vf", filter, "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p"), audio...)}
	if !hardwareEncode() {
		return []encodeAttempt{software}
	}
	hardware := encodeAttempt{
		globals: []string{"-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-filter_hw_device", "va"},
		args:    append(append(append([]string{}, maps...), "-vf", filter+",format=nv12,hwupload", "-c:v", "h264_vaapi", "-rc_mode", "CQP", "-qp", "23"), audio...),
	}
	return []encodeAttempt{hardware, software}
}

