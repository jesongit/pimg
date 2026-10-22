package config

import "strings"

const CurrentStartupVersion = 1

// Only the exact former default expands. Custom allowlists and explicit YAML /
// environment overrides keep their precedence and are never widened silently.
func isLegacyDefaultExtensions(exts []string) bool {
	old := strings.Fields("jpg jpeg png gif webp avif bmp mp4 m4v webm mov mkv")
	if len(exts) != len(old) {
		return false
	}
	set := map[string]bool{}
	for _, e := range exts {
		set[strings.TrimPrefix(strings.ToLower(e), ".")] = true
	}
	for _, e := range old {
		if !set[e] {
			return false
		}
	}
	return true
}
