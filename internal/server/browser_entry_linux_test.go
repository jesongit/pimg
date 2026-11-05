package server

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"pimg/internal/store"
)

func TestFileBrowserEntryNativePathReferenceAndSpecialFile(t *testing.T) {
	s := importServer(t)
	root := filepath.Join(t.TempDir(), "folder\xff")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "saved\xfe.mp4")
	if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	request := fileBrowserRequest(t, s)
	w := request("GET", "/api/admin/file-browser/entry?"+url.Values{"path": {store.EncodeSourcePath(p)}}.Encode(), nil, "")
	var response struct {
		Entry  store.BrowserEntry `json:"entry"`
		Parent string             `json:"parent"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Entry.Path != p || response.Parent != store.EncodeSourcePath(root) {
		t.Fatal("source reference lost native path bytes", w.Code, w.Body.String())
	}
	fifo := filepath.Join(root, "special.mp4")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/admin/file-browser/entry?"+url.Values{"path": {store.EncodeSourcePath(fifo)}}.Encode(), nil, "")
	if w.Code != 400 {
		t.Fatal("special source accepted", w.Code, w.Body.String())
	}
}
