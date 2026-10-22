package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/store"
)

func fileBrowserRequest(t *testing.T, s *Server) func(string, string, io.Reader, string) *httptest.ResponseRecorder {
	t.Helper()
	const session = "file-browser-test-session"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
	if _, err := s.DB.GetSession(digest); err != nil {
		if err := s.DB.InsertSession(digest, time.Now().Add(time.Hour).Unix(), "test", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	return func(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
}

func browserListing(t *testing.T, s *Server, path, query string, offset int, ordering ...string) store.BrowserListing {
	t.Helper()
	params := url.Values{"path": {path}, "q": {query}, "offset": {fmt.Sprint(offset)}}
	if len(ordering) > 0 {
		params.Set("sort", ordering[0])
	}
	w := fileBrowserRequest(t, s)("GET", "/api/admin/file-browser?"+params.Encode(), nil, "")
	var listing store.BrowserListing
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return listing
}

func TestFileBrowserSortingKeepsDirectoriesFirstAndStableTies(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for _, name := range []string{"z-folder", "a-folder"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, size := range map[string]int{"alpha.JPG": 40, "Beta.png": 20, "gamma.jpg": 20, "notes": 10} {
		if err := os.WriteFile(filepath.Join(root, name), bytes.Repeat([]byte{1}, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for ordering, expected := range map[string]string{
		"name":      "a-folder,z-folder,alpha.JPG,Beta.png,gamma.jpg,notes",
		"name_desc": "z-folder,a-folder,notes,gamma.jpg,Beta.png,alpha.JPG",
		"size_desc": "a-folder,z-folder,alpha.JPG,Beta.png,gamma.jpg,notes",
		"size_asc":  "a-folder,z-folder,notes,Beta.png,gamma.jpg,alpha.JPG",
		"type":      "a-folder,z-folder,notes,alpha.JPG,gamma.jpg,Beta.png",
		"type_desc": "a-folder,z-folder,Beta.png,alpha.JPG,gamma.jpg,notes",
	} {
		t.Run(ordering, func(t *testing.T) {
			listing := browserListing(t, s, root, "", 0, ordering)
			var names []string
			for _, entry := range listing.Items {
				names = append(names, entry.Name)
			}
			if strings.Join(names, ",") != expected {
				t.Fatal(names)
			}
		})
	}
	filtered := browserListing(t, s, root, "jpg", 0, "size_asc")
	if filtered.Total != 2 || filtered.Items[0].Name != "gamma.jpg" || filtered.Items[1].Name != "alpha.JPG" {
		t.Fatal("filtered sorting", filtered)
	}
	request := fileBrowserRequest(t, s)
	if w := request("GET", "/api/admin/file-browser?"+url.Values{"path": {root}, "sort": {"invalid"}}.Encode(), nil, ""); w.Code != 400 {
		t.Fatal("invalid ordering accepted", w.Code)
	}
}

func TestFileBrowserSortsWholeDirectoryBeforePagination(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for i := 0; i < 205; i++ {
		ext := "jpg"
		if i%2 == 1 {
			ext = "png"
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("photo-%03d.%s", i, ext)), bytes.Repeat([]byte{1}, i+1), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first := browserListing(t, s, root, "", 0, "size_desc")
	second := browserListing(t, s, root, "", first.NextOffset, "size_desc")
	all := append(first.Items, second.Items...)
	if len(all) != 205 || first.NextOffset != 200 || second.NextOffset != 0 {
		t.Fatal("pagination", first.Total, first.NextOffset, second.NextOffset)
	}
	for i, entry := range all {
		if entry.Size != int64(205-i) {
			t.Fatalf("size ordering crosses page boundary incorrectly at %d: %v", i, entry)
		}
	}
	first = browserListing(t, s, root, "", 0, "type")
	second = browserListing(t, s, root, "", first.NextOffset, "type")
	all = append(first.Items, second.Items...)
	var expected []string
	for _, ext := range []string{"jpg", "png"} {
		for i := 0; i < 205; i++ {
			if i%2 == 0 && ext == "jpg" || i%2 == 1 && ext == "png" {
				expected = append(expected, fmt.Sprintf("photo-%03d.%s", i, ext))
			}
		}
	}
	for i, entry := range all {
		if entry.Name != expected[i] {
			t.Fatalf("type ordering crosses page boundary incorrectly at %d: %v", i, entry)
		}
	}
}

func TestFileBrowserReadOnlyNavigationRangeAndSourceChanges(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	photo := filepath.Join(root, "photo space #中文.PNG")
	plain := interactionPNG(t)
	if err := os.WriteFile(photo, plain, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "旅行照片"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("do not serve"), 0600)
	listing := browserListing(t, s, root, "", 0)
	if listing.Total != 3 || !listing.Items[0].Directory || listing.Breadcrumbs[len(listing.Breadcrumbs)-1].Path != root {
		t.Fatal(listing)
	}
	filtered := browserListing(t, s, root, "中文", 0)
	if filtered.Total != 1 || filtered.Items[0].Kind != "image" || !filtered.Items[0].Importable {
		t.Fatal(filtered)
	}
	entry := filtered.Items[0]
	address := browserContentURL(entry.Path, entry.Version, "")
	request := fileBrowserRequest(t, s)
	w := request("GET", address, nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	// Reuse a real authenticated session for HEAD/Range requests.
	const session = "file-browser-test-session"
	for _, method := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(method, address, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		r.Header.Set("Range", "bytes=2-8")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, r)
		if out.Code != 206 || out.Header().Get("Content-Range") != fmt.Sprintf("bytes 2-8/%d", len(plain)) || method == "GET" && !bytes.Equal(out.Body.Bytes(), plain[2:9]) || method == "HEAD" && out.Body.Len() != 0 {
			t.Fatal(method, out.Code, out.Header(), out.Body.String())
		}
	}
	for _, method := range []string{"POST", "PATCH", "DELETE", "PUT"} {
		w = request(method, address, strings.NewReader(`{}`), "application/json")
		if w.Code != 405 && w.Code != 404 {
			t.Fatal("source mutation route exists", method, w.Code)
		}
	}
	for _, mode := range []string{"transcode", "../image", "thumb"} {
		if w = request("GET", browserContentURL(entry.Path, entry.Version, mode), nil, ""); w.Code != 409 {
			t.Fatal(mode, w.Code)
		}
	}
	text := browserListing(t, s, root, "notes", 0).Items[0]
	if w = request("GET", browserContentURL(text.Path, text.Version, ""), nil, ""); w.Code != 409 {
		t.Fatal("nonmedia content served", w.Code)
	}
	for _, endpoint := range []string{"/api/admin/file-browser?path=" + url.QueryEscape(root), address, "/api/admin/file-browser/view"} {
		method := "GET"
		if strings.HasSuffix(endpoint, "/view") {
			method = "POST"
		}
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, httptest.NewRequest(method, endpoint, strings.NewReader(`{}`)))
		if out.Code != 401 {
			t.Fatal("anonymous source access", endpoint, out.Code)
		}
	}
	before, _ := os.ReadFile(photo)
	if !bytes.Equal(before, plain) {
		t.Fatal("viewing changed the source")
	}
	var count int
	s.DB.QueryRow("SELECT count(*) FROM files").Scan(&count)
	if count != 0 {
		t.Fatal("browsing ingested source", count)
	}
	os.WriteFile(photo, append(plain, 0), 0600)
	if w = request("GET", address, nil, ""); w.Code != 409 {
		t.Fatal("stale source version accepted", w.Code)
	}
	if _, err := s.Svc.BrowseLocalFiles(context.Background(), "../outside", "", 0, ""); err == nil {
		t.Fatal("relative traversal accepted")
	}
}

func TestFileBrowserPaginationSearchPolicyAndSymlinks(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for i := 0; i < 205; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("photo-%03d.jpg", i)), []byte("listing fixture"), 0600)
	}
	first := browserListing(t, s, root, "", 0)
	second := browserListing(t, s, root, "", first.NextOffset)
	if len(first.Items) != 200 || len(second.Items) != 5 || first.Items[0].Name != "photo-000.jpg" || second.Items[0].Name != "photo-200.jpg" {
		t.Fatal(first.Total, first.NextOffset, second)
	}
	if got := browserListing(t, s, root, "204", 0); got.Total != 1 || got.Items[0].Name != "photo-204.jpg" {
		t.Fatal("search only examined the first page", got)
	}
	s.Svc.SetDynamic(-1, -1, -1, []string{"png"})
	if entry := browserListing(t, s, root, "204", 0).Items[0]; entry.Importable || entry.Kind != "image" {
		t.Fatal("upload policy should restrict import, not viewing", entry)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "outside.png"), interactionPNG(t), 0600)
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err == nil {
		listing := browserListing(t, s, root, "linked", 0)
		if listing.Total != 0 {
			t.Fatal("symlink listed", listing)
		}
		if _, err = s.Svc.BrowseLocalFiles(context.Background(), link, "", 0, ""); err == nil {
			t.Fatal("symlink ancestor accepted")
		}
	}
}

func waitBrowserView(t *testing.T, s *Server, entry store.BrowserEntry, mode string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for retry := true; ; retry = false {
		view, err := s.Svc.PrepareBrowserView(t.Context(), entry.Path, entry.Version, mode, retry)
		if err != nil || view.Status == "failed" {
			t.Fatal("prepare view", err, view)
		}
		if view.Status == "ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("view timeout")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestFileBrowserCompatibleImagesAreDisposableAndOriginalsStayExact(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	s := importServer(t)
	root := t.TempDir()
	p := filepath.Join(root, "original.png")
	plain := interactionPNG(t)
	os.WriteFile(p, plain, 0600)
	entry := browserListing(t, s, root, "", 0).Items[0]
	waitBrowserView(t, s, entry, "image")
	w := fileBrowserRequest(t, s)("GET", browserContentURL(p, entry.Version, "image"), nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatal("compatible image is not decodable", err)
	}
	entries, _ := os.ReadDir(root)
	after, _ := os.ReadFile(p)
	if len(entries) != 1 || !bytes.Equal(after, plain) {
		t.Fatal("compatibility wrote into the source directory")
	}
	cache, _ := filepath.Glob(filepath.Join(s.Cfg.Paths.RuntimeRoot(), "cache", "originals", "browser-*.png"))
	if len(cache) != 1 {
		t.Fatal(cache)
	}
	old := time.Now().Add(-time.Duration(s.Svc.OriginalCacheHours()+1) * time.Hour)
	os.Chtimes(cache[0], old, old)
	if _, _, err := s.Svc.OpenBrowserContent(p, entry.Version, "image"); err == nil {
		t.Fatal("expired cache served")
	}
	if err := s.Svc.CleanupMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache[0]); !os.IsNotExist(err) {
		t.Fatal("cache not cleaned", err)
	}
}

func TestFileBrowserVideoFormatsWatchableWithoutImport(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	s := importServer(t)
	root := t.TempDir()
	for _, ext := range []string{"mp4", "m4v", "webm", "mov", "mkv", "ts", "m2ts", "mts", "avi", "flv"} {
		t.Run(ext, func(t *testing.T) {
			p := filepath.Join(root, "sample."+ext)
			codec := "libx264"
			if ext == "webm" {
				codec = "libvpx"
			} else if ext == "avi" {
				codec = "mpeg4"
			} else if ext == "flv" {
				codec = "flv1"
			}
			args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=8", "-t", "0.5", "-c:v", codec, "-pix_fmt", "yuv420p"}
			if ext == "ts" || ext == "m2ts" || ext == "mts" {
				args = append(args, "-f", "mpegts")
			}
			if ext == "m4v" {
				args = append(args, "-f", "mp4")
			}
			args = append(args, p)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture %v %s", err, out)
			}
			plain, _ := os.ReadFile(p)
			entry := browserListing(t, s, root, "sample."+ext, 0).Items[0]
			mode, err := s.Svc.BrowserPlaybackMode(t.Context(), p, entry.Version)
			if err != nil {
				t.Fatal(err)
			}
			waitBrowserView(t, s, entry, mode)
			content, _, err := s.Svc.OpenBrowserContent(p, entry.Version, mode)
			if err != nil {
				t.Fatal(err)
			}
			info, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,pix_fmt", "-of", "csv=p=0", content.Name()).Output()
			content.Close()
			if err != nil || !strings.Contains(string(info), "h264,yuv420p") {
				t.Fatal("not a compatible video", string(info), err)
			}
			w := fileBrowserRequest(t, s)("GET", browserContentURL(p, entry.Version, "")+"&download=1", nil, "")
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
				t.Fatal("source download changed", w.Code)
			}
			after, _ := os.ReadFile(p)
			if !bytes.Equal(after, plain) {
				t.Fatal("source bytes changed")
			}
		})
	}
	var count int
	s.DB.QueryRow("SELECT count(*) FROM files").Scan(&count)
	if count != 0 {
		t.Fatal("viewing imported files", count)
	}
}

func TestFileBrowserImageFormatMetadata(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for _, ext := range []string{"jpg", "jpeg", "png", "gif", "webp", "avif", "bmp", "heic", "heif"} {
		os.WriteFile(filepath.Join(root, "sample."+ext), []byte("metadata fixture"), 0600)
	}
	listing := browserListing(t, s, root, "", 0)
	for _, entry := range listing.Items {
		if entry.Kind != "image" || !entry.Importable {
			t.Fatal(entry)
		}
	}
	// This test asserts format routing only; actual decoding is exercised with
	// real media in the isolated browser/codec acceptance, not these placeholders.
}
