package mediax

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// BrowserCaps reports which codecs the requesting browser can decode inside an
// MP4 container. The zero value is the strict, universally playable baseline:
// H.264 8-bit 4:2:0 video and AAC audio, so every existing caller keeps the
// previous behavior unless it passes an explicit capability set.
type BrowserCaps struct {
	HEVC8  bool // HEVC Main (8-bit) in MP4
	HEVC10 bool // HEVC Main10 (10-bit) in MP4
	AV1    bool
	VP9    bool
	MP3    bool // MP3 audio in MP4
	Opus   bool // Opus audio in MP4
	AC3    bool // AC-3 and E-AC-3 audio in MP4
	FLAC   bool // FLAC audio in MP4
}

func videoStreamSupported(codec, pixFmt string, caps BrowserCaps) bool {
	switch codec {
	case "h264":
		return pixFmt == "yuv420p" || pixFmt == "yuvj420p"
	case "hevc":
		switch pixFmt {
		case "yuv420p":
			return caps.HEVC8
		case "yuv420p10le":
			return caps.HEVC10
		}
		return false
	case "av1":
		return caps.AV1 && (pixFmt == "yuv420p" || pixFmt == "yuv420p10le")
	case "vp9":
		return caps.VP9 && (pixFmt == "yuv420p" || pixFmt == "yuv420p10le")
	}
	return false
}

func audioStreamSupported(codec string, caps BrowserCaps) bool {
	switch codec {
	case "aac":
		return true
	case "mp3":
		return caps.MP3
	case "opus":
		return caps.Opus
	case "ac3", "eac3":
		return caps.AC3
	case "flac":
		return caps.FLAC
	}
	return false
}

// AllVideoSupported reports whether every video stream is decodable by the
// browser. With zero caps this matches the strict AllVideoCompatible baseline.
func (v VideoInfo) AllVideoSupported(caps BrowserCaps) bool {
	for i, codec := range v.VideoCodecs {
		if !videoStreamSupported(codec, v.VideoPixFmts[i], caps) {
			return false
		}
	}
	return true
}

// AllAudioSupported reports whether every audio track is decodable by the
// browser. With zero caps this matches the strict AllAudioCompatible baseline.
func (v VideoInfo) AllAudioSupported(caps BrowserCaps) bool {
	for _, codec := range v.AudioCodecs {
		if !audioStreamSupported(codec, caps) {
			return false
		}
	}
	return true
}

// SupportedMP4 reports whether the file plays as-is: an MP4/M4V container
// whose every video and audio stream the browser decodes directly.
func (v VideoInfo) SupportedMP4(ext string, caps BrowserCaps) bool {
	return (ext == "mp4" || ext == "m4v") && v.AllVideoSupported(caps) && v.AllAudioSupported(caps)
}

// CapsFingerprint identifies a capability set beyond the strict baseline. It
// separates compatibility caches so a remux that keeps HEVC or AC-3 streams is
// never served to a browser that cannot decode them.
func CapsFingerprint(caps BrowserCaps) string {
	names := make([]string, 0, 8)
	for _, flag := range []struct {
		name  string
		value bool
	}{{"av1", caps.AV1}, {"ac3", caps.AC3}, {"flac", caps.FLAC}, {"hevc8", caps.HEVC8}, {"hevc10", caps.HEVC10}, {"mp3", caps.MP3}, {"opus", caps.Opus}, {"vp9", caps.VP9}} {
		if flag.value {
			names = append(names, flag.name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("pimg-caps\x00" + strings.Join(names, "+")))
	return hex.EncodeToString(sum[:])[:8]
}
