package mediax

import (
	"context"
	"errors"
)

// MakeCompatible only writes a disposable view. It never replaces an archive.
func MakeCompatible(ctx context.Context, source, dest, kind, mode string) error {
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-i", source}
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
	args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
	if mode == "remux" {
		args = append(args, "-c", "copy", "-movflags", "+faststart", "-f", "mp4", dest)
		return runVideoCommand(ctx, args)
	}
	info, err := InspectVideo(ctx, source)
	if err != nil {
		return err
	}
	filter := "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,pad=ceil(iw/2)*2:ceil(ih/2)*2"
	if info.HDR {
		filter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv," + filter
	}
	args = append(args, "-vf", filter, "-c:v", "libx264", "-preset", "fast", "-crf", "23", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", dest)
	return runVideoCommand(ctx, args)
}
