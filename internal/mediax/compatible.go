package mediax

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// MakeCompatible only writes a disposable view. It never replaces an archive.
func MakeCompatible(ctx context.Context, source, dest, kind, mode string) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, nil)
}

// Linux subprocesses inherit the already validated read-only descriptor. They
// can seek without copying a multi-GB source or reopening a replaced pathname.
func MakeCompatibleFile(ctx context.Context, source *os.File, dest, mode string, progress func(float64)) error {
	if runtime.GOOS != "linux" {
		return errors.New("当前平台需要兼容处理快照")
	}
	return makeCompatible(ctx, "/proc/self/fd/3", source, dest, "video", mode, progress)
}

func MakeCompatibleProgress(ctx context.Context, source, dest, kind, mode string, progress func(float64)) error {
	return makeCompatible(ctx, source, nil, dest, kind, mode, progress)
}

func makeCompatible(ctx context.Context, source string, file *os.File, dest, kind, mode string, progress func(float64)) error {
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
	args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
	if mode == "remux" {
		args = append(args, "-c", "copy")
	} else if !info.HDR && info.AllVideoCompatible && !info.AllAudioCompatible {
		// An unsupported audio track does not require re-encoding the picture.
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "128k")
	} else {
		filter := "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,pad=ceil(iw/2)*2:ceil(ih/2)*2"
		if info.HDR {
			filter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv," + filter
		}
		args = append(args, "-vf", filter, "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "128k")
	}
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
