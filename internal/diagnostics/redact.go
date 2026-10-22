// Package diagnostics provides bounded, read-only runtime diagnostics.
package diagnostics

import (
	"net/url"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;"'<>]+`)
	pimgPattern   = regexp.MustCompile(`pimg_[A-Za-z0-9_\-]+`)
	botPattern    = regexp.MustCompile(`\b[0-9]{5,}:[A-Za-z0-9_\-]{20,}`)
	urlPattern    = regexp.MustCompile(`https?://[^\s<>"']+`)
	secretPattern = regexp.MustCompile(`(?i)(["']?(?:password|passwd|token|access_token|refresh_token|api[_-]?key|api[_-]?hash|secret(?:[_-]?access[_-]?key)?|master[_-]?key|authorization|cookie|session)["']?\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)
)

func SensitiveKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	for _, word := range []string{"password", "passwd", "token", "secret", "authorization", "cookie", "session", "credential", "masterkey", "apikey", "apihash", "accesskey", "privatekey"} {
		if strings.Contains(key, word) {
			return true
		}
	}
	return key == "key"
}

// Redact removes credential-shaped text, URL credentials and all query values.
// It is defense in depth, not an invitation to log secrets in the first place.
func Redact(text string) string {
	text = urlPattern.ReplaceAllStringFunc(text, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		u.User = nil
		if u.RawQuery != "" {
			u.RawQuery = "redacted"
		}
		u.Fragment = ""
		return u.String()
	})
	text = bearerPattern.ReplaceAllString(text, "Bearer "+redacted)
	text = pimgPattern.ReplaceAllString(text, redacted)
	text = botPattern.ReplaceAllString(text, redacted)
	return secretPattern.ReplaceAllString(text, "${1}"+redacted)
}

func Clip(text string, max int) string {
	if len(text) <= max {
		return text
	}
	// Range boundaries are UTF-8 character boundaries.
	end := 0
	for i := range text {
		if i > max-3 {
			break
		}
		end = i
	}
	return text[:end] + "…"
}
