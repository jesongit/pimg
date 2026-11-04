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
	"sync/atomic"
	"time"
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
		hardware := len(attempt.globals) > 0
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1"}
		args = append(args, attempt.globals...)
		args = append(args, "-i", sourcePath)
		args = append(args, attempt.args...)
		args = append(args, "-f", "mp4", "-movflags", "+empty_moov+frag_keyframe+default_base_moof", dest)
		err := runConversionWithStallWatchdog(ctx, dest, func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, "ffmpeg", args...)
			if runtime.GOOS == "linux" {
				cmd.ExtraFiles = []*os.File{source}
			}
			return runVideoProcess(cmd)
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hardware {
			// A GPU strategy that errors or stops producing output (driver
			// hang) is switched off for the rest of the process; software
			// always remains available.
			hardwareDisabled.Store(true)
		}
		lastErr = err
	}
	return lastErr
}

func makeCompatible(ctx context.Context, source string, file *os.File, dest, kind, mode string, caps BrowserCaps, progress func(float64)) error {
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		if file != nil {
			cmd.ExtraFiles = []*os.File{file}
		}
		return cmd
	}
	if kind == "image" {
		if isHEIF(source) {
			return heifPNG(ctx, source, dest, 8192)
		}
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1", "-i", source, "-frames:v", "1", "-f", "image2", "-c:v", "png", dest}
		return runVideoCommand(ctx, args)
	}
	if kind != "video" {
		return errors.New("此文件不能生成兼容查看版本")
	}
	info, err := inspectVideo(command(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=index,codec_type,codec_name,pix_fmt,width,height,color_transfer:format=duration,start_time,format_name", "-of", "json", source))
	if err != nil {
		return err
	}
	var lastErr error
	for _, attempt := range compatibleAttempts(info, mode, caps) {
		hardware := len(attempt.globals) > 0
		args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-threads", "2", "-filter_threads", "1"}
		args = append(args, attempt.globals...)
		args = append(args, "-i", source)
		args = append(args, attempt.args...)
		args = append(args, "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", "-f", "mp4", dest)
		err := runConversionWithStallWatchdog(ctx, dest, func(ctx context.Context) error {
			cmd := command(ctx, "ffmpeg", args...)
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
				return fmt.Errorf("ffmpeg 视频处理失败: %s", truncate(diagnostic.Bytes(), 20000))
			}
			return nil
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hardware {
			// A GPU strategy that errors or stalls is switched off for the rest
			// of the process; software always remains available.
			hardwareDisabled.Store(true)
		}
		lastErr = err
	}
	return lastErr
}

// runConversionWithStallWatchdog runs the conversion and kills it when the
// output file stops growing: a hung GPU driver blocks inside kernel DRM
// instead of failing, and would otherwise stall the attempt forever.
func runConversionWithStallWatchdog(ctx context.Context, output string, run func(context.Context) error) error {
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx2) }()
	var size int64 = -1
	stalled := 0
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-ticker.C:
			if st, err := os.Stat(output); err == nil {
				if st.Size() == size {
					stalled += 5
				} else {
					stalled, size = 0, st.Size()
				}
			}
			if size >= 0 && stalled >= 60 {
				cancel()
				<-done
				return errors.New("转码输出超过 60 秒没有增长，已中止本次尝试")
			}
		}
	}
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

// hardwareDisabled latches after a hardware attempt failed or stopped
// producing output (a hung GPU driver blocks in kernel DRM, not in ffmpeg):
// the rest of the process lifetime stays on software instead of paying the
// stall timeout for every video.
var hardwareDisabled atomic.Bool

func hardwareEncodeAvailable() bool {
	return !hardwareDisabled.Load() && hardwareEncode()
}

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
	// HDR tone mapping is expensive on the CPU: auto threads and the faster
	// preset keep it near realtime (measured ~1.1x at 2 threads vs ~2x).
	preset, threads := "veryfast", "2"
	if info.HDR {
		preset, threads = "superfast", "0"
	}
	software := encodeAttempt{args: append(append(append([]string{}, maps...), "-vf", filter, "-c:v", "libx264", "-threads", threads, "-preset", preset, "-crf", "23", "-pix_fmt", "yuv420p"), audio...)}
	// HDR needs a CPU tone-map pass before encoding; a hardware encode there
	// was observed to hang this GPU driver, so HDR stays software only.
	if !hardwareEncodeAvailable() || info.HDR {
		return []encodeAttempt{software}
	}
	hardware := encodeAttempt{
		globals: []string{"-init_hw_device", "vaapi=va:/dev/dri/renderD128", "-filter_hw_device", "va"},
		args:    append(append(append([]string{}, maps...), "-vf", filter+",format=nv12,hwupload", "-c:v", "h264_vaapi", "-rc_mode", "CQP", "-qp", "23"), audio...),
	}
	return []encodeAttempt{hardware, software}
}

