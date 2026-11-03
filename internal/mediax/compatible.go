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
// for the whole conversion.
func MakeCompatibleFileStream(ctx context.Context, source *os.File, dest, mode string, caps BrowserCaps, client io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("当前平台需要兼容处理快照")
	}
	if mode != "remux" && mode != "transcode" {
		return errors.New("此文件不能流式生成兼容查看版本")
	}
	command := func(name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.ExtraFiles = []*os.File{source}
		return cmd
	}
	info, err := inspectVideo(command("ffprobe", "-v", "error", "-show_entries", "stream=index,codec_type,codec_name,pix_fmt,width,height,color_transfer:format=duration,start_time,format_name", "-of", "json", "/proc/self/fd/3"))
	if err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part.*")
	if err != nil {
		return err
	}
	defer func() {
		output.Close()
		os.Remove(output.Name())
	}()
	args := append(compatibleArgs(info, mode, caps), "-f", "mp4", "-movflags", "+empty_moov+frag_keyframes+default_base_moof", "pipe:1")
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
	_, copyErr := io.Copy(io.MultiWriter(output, client), stdout)
	// A client disconnect must not leave a truncated cache behind: stop ffmpeg
	// before judging the copy result.
	if copyErr != nil {
		_ = cmd.Process.Kill()
	}
	err = errors.Join(copyErr, cmd.Wait())
	if err == nil {
		err = output.Sync()
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg 视频处理失败: %s", truncate(diagnostic.Bytes(), 400))
	}
	// Only a fully converted file may appear under the cache name.
	return os.Rename(output.Name(), dest)
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
	args = append(args, compatibleArgs(info, mode, caps)...)
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
		return fmt.Errorf("ffmpeg 视频处理失败: %s", truncate(diagnostic.Bytes(), 400))
	}
	return nil
}

// compatibleArgs maps the video into the browser target. A supported picture is
// copied; only genuinely undecodable input is re-encoded, and the source
// resolution is preserved instead of a fixed downscaled ceiling.
func compatibleArgs(info VideoInfo, mode string, caps BrowserCaps) []string {
	args := []string{"-map", "0:v:0", "-map", "0:a:0?"}
	if mode == "remux" {
		return append(args, "-c", "copy")
	}
	videoCopy := !info.HDR && info.AllVideoSupported(caps)
	audioCopy := info.AllAudioSupported(caps)
	if videoCopy && !audioCopy {
		// An unsupported audio track does not require re-encoding the picture.
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "128k")
		return args
	}
	filter := "pad=ceil(iw/2)*2:ceil(ih/2)*2"
	if info.HDR {
		filter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv," + filter
	}
	args = append(args, "-vf", filter, "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "128k")
	return args
}

