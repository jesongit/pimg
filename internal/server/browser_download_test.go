package server

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileBrowserDownloadOriginalFilesAndRanges(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	request := fileBrowserRequest(t, s)
	for name, original := range map[string][]byte{
		"图片 space #.png": interactionPNG(t),
		"movie.mp4":      []byte("original video bytes"),
		"notes.txt":      []byte("original text bytes\r\n中文"),
		"archive.zip":    []byte("PK\x03\x04original archive bytes\x00\xff"),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(root, name)
			if err := os.WriteFile(p, original, 0600); err != nil {
				t.Fatal(err)
			}
			entry, err := s.Svc.BrowserEntryInfo(p)
			if err != nil {
				t.Fatal(err)
			}
			address := browserContentURL(p, entry.Version, "") + "&download=1"
			w := request(http.MethodGet, address, nil, "")
			disposition, params, dispositionErr := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), original) || dispositionErr != nil || disposition != "attachment" || params["filename"] != name {
				t.Fatal("original download failed", w.Code, w.Header(), w.Body.String(), dispositionErr)
			}
			if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Type") != entry.MIME {
				t.Fatal("download headers changed", w.Header())
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				r := httptest.NewRequest(method, address, nil)
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "file-browser-test-session"})
				r.Header.Set("Range", "bytes=2-8")
				out := httptest.NewRecorder()
				s.Handler().ServeHTTP(out, r)
				if out.Code != 206 || out.Header().Get("Content-Range") != fmt.Sprintf("bytes 2-8/%d", len(original)) || method == http.MethodGet && !bytes.Equal(out.Body.Bytes(), original[2:9]) || method == http.MethodHead && out.Body.Len() != 0 {
					t.Fatal("download range failed", method, out.Code, out.Header(), out.Body.Bytes())
				}
			}
			if entry.Kind == "other" {
				if out := request(http.MethodGet, browserContentURL(p, entry.Version, ""), nil, ""); out.Code != 409 {
					t.Fatal("download enabled inline nonmedia content", out.Code)
				}
				for _, mode := range []string{"image", "remux", "transcode"} {
					if out := request(http.MethodGet, browserContentURL(p, entry.Version, mode)+"&download=1", nil, ""); out.Code != 409 {
						t.Fatal("download enabled nonmedia conversion", mode, out.Code)
					}
				}
			}
			if err := os.WriteFile(p, append(append([]byte{}, original...), '!'), 0600); err != nil {
				t.Fatal(err)
			}
			if out := request(http.MethodGet, address, nil, ""); out.Code != 409 {
				t.Fatal("download accepted stale file version", out.Code)
			}
		})
	}
}

func TestFileBrowserDownloadRequiresAdminAndRejectsUnsafeSources(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	p := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(p, []byte("original text"), 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Svc.BrowserEntryInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	address := browserContentURL(p, entry.Version, "") + "&download=1"
	anonymous := httptest.NewRecorder()
	s.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, address, nil))
	if anonymous.Code != 401 {
		t.Fatal("anonymous source download", anonymous.Code)
	}
	albumImportAccounts(t, s)
	if out := uploaderCall(s, http.MethodGet, address, "user:alpha", nil); out.Code != 401 {
		t.Fatal("ordinary account gained source download", out.Code, out.Body.String())
	}
	request := fileBrowserRequest(t, s)
	for _, test := range []struct{ path, version string }{{root, "directory"}, {p, ""}, {filepath.Join(root, "missing.txt"), entry.Version}} {
		if out := request(http.MethodGet, browserContentURL(test.path, test.version, "")+"&download=1", nil, ""); out.Code != 409 {
			t.Fatal("invalid download source accepted", test, out.Code)
		}
	}
	link := filepath.Join(root, "linked.txt")
	if err := os.Symlink(p, link); err != nil {
		t.Logf("symlink endpoint check unavailable: %v", err)
	} else if out := request(http.MethodGet, browserContentURL(link, entry.Version, "")+"&download=1", nil, ""); out.Code != 409 {
		t.Fatal("download followed a symbolic link", out.Code)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if out := request(http.MethodGet, address, nil, ""); out.Code != 409 {
		t.Fatal("download reused a disappeared source", out.Code)
	}
}
