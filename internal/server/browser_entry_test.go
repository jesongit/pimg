package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/store"
)

func TestFileBrowserEntryResolvesBeyondListingPage(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	for i := 0; i < 205; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("video-%03d.mp4", i)), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(root, "video-204.mp4")
	first := browserListing(t, s, root, "", 0, "name")
	if first.NextOffset != 200 || len(first.Items) != 200 {
		t.Fatal("test did not establish pagination", first.Total, first.NextOffset)
	}
	for _, entry := range first.Items {
		if entry.Path == p {
			t.Fatal("target unexpectedly appeared in the first page")
		}
	}
	request := fileBrowserRequest(t, s)
	for _, target := range []string{p, root, filepath.VolumeName(root) + string(filepath.Separator)} {
		w := request("GET", "/api/admin/file-browser/entry?"+url.Values{"path": {store.EncodeSourcePath(target)}, "q": {"no-match"}, "filter": {"image"}, "offset": {"200"}}.Encode(), nil, "")
		var response struct {
			Entry  store.BrowserEntry `json:"entry"`
			Parent string             `json:"parent"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if response.Entry.Path != target || response.Parent != store.EncodeSourcePath(filepath.Dir(target)) || response.Entry.Directory != (target != p) || response.Entry.Version == "" {
			t.Fatal("wrong precise entry or parent", response)
		}
		if target == p && (response.Entry.Size != 8 || response.Entry.Kind != "video" || response.Entry.MIME != "video/mp4") {
			t.Fatal("incomplete media metadata", response)
		}
		if w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("metadata response allowed caching", w.Header())
		}
	}
}

func TestFileBrowserEntryRequiresAdminAndRejectsInvalidSources(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	p := filepath.Join(root, "saved.mp4")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/admin/file-browser/entry?" + url.Values{"path": {p}}.Encode()
	anonymous := httptest.NewRecorder()
	s.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, endpoint, nil))
	if anonymous.Code != 401 {
		t.Fatal("anonymous source access", anonymous.Code)
	}
	albumImportAccounts(t, s)
	if w := uploaderCall(s, http.MethodGet, endpoint, "user:alpha", nil); w.Code != 401 {
		t.Fatal("ordinary account gained source access", w.Code, w.Body.String())
	}
	request := fileBrowserRequest(t, s)
	for _, test := range []struct {
		path string
		code int
	}{{"", 400}, {filepath.Join(root, "missing.mp4"), 404}, {"pimg-path://!", 400}} {
		w := request("GET", "/api/admin/file-browser/entry?"+url.Values{"path": {test.path}}.Encode(), nil, "")
		if w.Code != test.code {
			t.Fatal(test.path, w.Code, w.Body.String())
		}
	}
	link := filepath.Join(root, "linked.mp4")
	if err := os.Symlink(p, link); err != nil {
		t.Logf("symlink endpoint check unavailable: %v", err)
	} else if w := request("GET", "/api/admin/file-browser/entry?"+url.Values{"path": {link}}.Encode(), nil, ""); w.Code != 400 {
		t.Fatal("symlink source accepted", w.Code, w.Body.String())
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", endpoint, nil, ""); w.Code != 404 {
		t.Fatal("vanished source accepted", w.Code, w.Body.String())
	}
}
