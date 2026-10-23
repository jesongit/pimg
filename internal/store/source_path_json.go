package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"unicode/utf8"
)

// Unix names are byte strings. JSON replaces invalid UTF-8, so such paths
// need a lossless reference for navigation and source reads. Ordinary absolute
// paths retain their existing representation and cannot collide with this URI.
const sourcePathPrefix = "pimg-path://"

func EncodeSourcePath(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return sourcePathPrefix + base64.RawURLEncoding.EncodeToString([]byte(p))
}

func EncodeSourcePaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = EncodeSourcePath(p)
	}
	return out
}

func decodeSourcePath(p string) (string, error) {
	if !strings.HasPrefix(p, sourcePathPrefix) {
		if runtime.GOOS == "windows" && !utf8.ValidString(p) {
			return "", errors.New("文件路径编码无效")
		}
		return p, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(p, sourcePathPrefix))
	if err != nil || utf8.Valid(raw) || runtime.GOOS == "windows" || strings.ContainsRune(string(raw), '\x00') {
		return "", errors.New("文件路径引用无效，请重新打开目录")
	}
	return string(raw), nil
}

func decodeSourcePaths(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		var err error
		out[i], err = decodeSourcePath(p)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (entry BrowserEntry) MarshalJSON() ([]byte, error) {
	type plain BrowserEntry
	return json.Marshal(struct {
		*plain
		Path string `json:"path"`
	}{(*plain)(&entry), EncodeSourcePath(entry.Path)})
}

// Index payloads use the same lossless encoding as HTTP responses. Restore
// native bytes before relative-path calculations or source access.
func (entry *BrowserEntry) UnmarshalJSON(data []byte) error {
	type plain BrowserEntry
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	p, err := decodeSourcePath(value.Path)
	if err != nil {
		return err
	}
	*entry = BrowserEntry(value)
	entry.Path = p
	return nil
}

func (listing BrowserListing) MarshalJSON() ([]byte, error) {
	type plain BrowserListing
	locations := append([]BrowserLocation{}, listing.Breadcrumbs...)
	for i := range locations {
		locations[i].Path = EncodeSourcePath(locations[i].Path)
	}
	return json.Marshal(struct {
		*plain
		Path        string            `json:"path"`
		Parent      string            `json:"parent"`
		Breadcrumbs []BrowserLocation `json:"breadcrumbs"`
		IndexRoot   string            `json:"index_root,omitempty"`
	}{(*plain)(&listing), EncodeSourcePath(listing.Path), EncodeSourcePath(listing.Parent), locations, EncodeSourcePath(listing.IndexRoot)})
}

func (root BrowserIndexRoot) MarshalJSON() ([]byte, error) {
	type plain BrowserIndexRoot
	return json.Marshal(struct {
		*plain
		Path string `json:"path"`
	}{(*plain)(&root), EncodeSourcePath(root.Path)})
}
