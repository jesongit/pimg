package store

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSourcePathJSONPreservesNativeBytes(t *testing.T) {
	for _, p := range []string{"/data/普通.png", "/data/pimg-path:/valid.png", "pimg-path:ordinary-name"} {
		if EncodeSourcePath(p) != p {
			t.Fatal("ordinary path changed", p)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	p := "/data/013 – 测试目录ﯧ\xed\xb0\x8aﯭ\xed\xba\x88 [57P]/same\xff.png"
	entry := BrowserEntry{ImportPath: ImportPath{Path: p, Name: filepath.Base(p), Size: 42}, Ext: "png", Kind: "image", Version: "version", RelativePath: "same\xff.png", Importable: true, CreatedAt: 10, ModifiedAt: 20}
	data, err := json.Marshal(entry)
	if err != nil || !utf8.Valid(data) || strings.Contains(string(data), `"path":"/data/`) {
		t.Fatal(string(data), err)
	}
	var restored BrowserEntry
	if err = json.Unmarshal(data, &restored); err != nil || restored.Path != p || restored.Version != entry.Version || restored.Ext != entry.Ext || !restored.Importable || restored.CreatedAt != 10 {
		t.Fatal(restored, err)
	}
	listing := BrowserListing{Path: filepath.Dir(p), Parent: filepath.Dir(filepath.Dir(p)), IndexRoot: filepath.Dir(p), Breadcrumbs: []BrowserLocation{{Path: filepath.Dir(p), Name: "目录"}}, Items: []BrowserEntry{entry}}
	data, err = json.Marshal(listing)
	var wire struct {
		Path, Parent string
		IndexRoot    string `json:"index_root"`
		Breadcrumbs  []BrowserLocation
		Items        []struct{ Path, Version string }
	}
	if err != nil || json.Unmarshal(data, &wire) != nil || len(wire.Items) != 1 || wire.Items[0].Version != "version" {
		t.Fatal(string(data), err)
	}
	for got, want := range map[string]string{wire.Path: listing.Path, wire.Parent: listing.Parent, wire.IndexRoot: listing.IndexRoot, wire.Breadcrumbs[0].Path: listing.Path, wire.Items[0].Path: p} {
		raw, e := decodeSourcePath(got)
		if e != nil || raw != want {
			t.Fatal("JSON reference lost bytes", got, e)
		}
	}
	for _, invalid := range []string{sourcePathPrefix + "!", sourcePathPrefix + base64.RawURLEncoding.EncodeToString([]byte("/ordinary")), sourcePathPrefix + base64.RawURLEncoding.EncodeToString([]byte("/nul\x00\xff"))} {
		if _, err := decodeSourcePath(invalid); err == nil {
			t.Fatal("invalid reference accepted", invalid)
		}
	}
	s, _ := indexTestService(t)
	traversal := sourcePathPrefix + base64.RawURLEncoding.EncodeToString([]byte("../outside\xff"))
	if _, err := s.absoluteImportPath(traversal); err == nil {
		t.Fatal("encoded parent traversal accepted")
	}
}

func TestBrowserIndexInvalidUTF8NativeNames(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux byte filenames")
	}
	s, source := indexTestService(t)
	directory := filepath.Join(source, "Video", "测试用户", "013 – 测试目录ﯧ\xed\xb0\x8aﯭ\xed\xba\x88 [57P]")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(directory, "same\xff.png"), filepath.Join(directory, "same\xfe.png")}
	for i, p := range paths {
		if err := os.WriteFile(p, []byte(strings.Repeat("original", i+1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			s.ApplyBrowserWatchPaths([]string{source})
			indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Ready && !r.Updating && r.Error == "" })
		}
		listing, err := s.BrowseLocalFiles(t.Context(), EncodeSourcePath(directory), "", 0, "name")
		if err != nil || len(listing.Items) != 2 || listing.Items[0].Path == listing.Items[1].Path || listing.Indexed != indexed {
			t.Fatal("native names collapsed", listing, err)
		}
		for _, entry := range listing.Items {
			file, _, e := s.OpenBrowserSource(EncodeSourcePath(entry.Path), entry.Version)
			if e != nil {
				t.Fatal(e)
			}
			data, e := io.ReadAll(file)
			file.Close()
			want, _ := os.ReadFile(entry.Path)
			if e != nil || !reflect.DeepEqual(data, want) {
				t.Fatal("wrong native file opened", e)
			}
		}
	}
	s.StopBrowserIndex()
	s.ApplyBrowserWatchPaths([]string{source})
	indexWait(t, s, func(r BrowserIndexRoot) bool { return r.Ready && !r.Updating && r.Error == "" })
	listing, err := s.BrowseLocalFiles(t.Context(), source, "[57P]", 0, "name", BrowserOptions{Recursive: true})
	if err != nil || len(listing.Items) != 2 || listing.IndexRoot != source {
		t.Fatal("restart/search lost native paths", listing, err)
	}
	// v1.0.33 stored raw keys but lossy JSON paths. Existing cache entries
	// must resolve correctly even before the startup scan reaches them.
	r := s.browserIndex.runner
	for _, entry := range listing.Items {
		payload, _ := json.Marshal(entry)
		var old map[string]any
		_ = json.Unmarshal(payload, &old)
		old["path"] = strings.ToValidUTF8(entry.Path, "\uFFFD")
		payload, _ = json.Marshal(old)
		if _, err := r.db.Exec(`UPDATE entries SET payload=? WHERE root=? AND path=?`, string(payload), source, indexPathKey(entry.Path)); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := s.BrowseLocalFiles(t.Context(), directory, "", 0, "name")
	if err != nil || len(legacy.Items) != 2 || legacy.Items[0].Path == legacy.Items[1].Path {
		t.Fatal("legacy cache paths collapsed", legacy, err)
	}
	for _, entry := range legacy.Items {
		if _, err := os.Stat(entry.Path); err != nil {
			t.Fatal("legacy payload changed native identity", err)
		}
	}
	root, err := openImportRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var walked []string
	err = fs.WalkDir(nativeImportFS{root}, ".", func(p string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			walked = append(walked, filepath.Join(source, p))
		}
		return e
	})
	if err != nil || len(walked) != 2 {
		t.Fatal("native import traversal lost files", walked, err)
	}
	link := filepath.Join(source, "escape\xff")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportPathInfo(EncodeSourcePath(link)); err == nil {
		t.Fatal("encoded source followed a symlink")
	}
}
