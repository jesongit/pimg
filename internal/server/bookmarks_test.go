package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

type bookmarkListResponse struct {
	Items []dbx.FileBookmarkNode `json:"items"`
}

func TestFileBrowserBookmarkAPI(t *testing.T) {
	s := importServer(t)
	call := fileBrowserRequest(t, s)
	dir := t.TempDir()

	w := call("POST", "/api/admin/file-browser/bookmarks/folders", strings.NewReader(`{"parent":0,"name":"影片"}`), "application/json")
	if w.Code != 201 {
		t.Fatalf("create folder: %d %s", w.Code, w.Body.String())
	}
	var folder struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &folder); err != nil || folder.ID == 0 {
		t.Fatalf("create folder body: %v %s", err, w.Body.String())
	}

	body := fmt.Sprintf(`{"folder":%d,"path":%q,"name":"视频目录"}`, folder.ID, dir)
	w = call("POST", "/api/admin/file-browser/bookmarks/items", strings.NewReader(body), "application/json")
	if w.Code != 201 {
		t.Fatalf("create item: %d %s", w.Code, w.Body.String())
	}
	var item struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil || item.ID == 0 {
		t.Fatalf("create item body: %v %s", err, w.Body.String())
	}

	w = call("GET", "/api/admin/file-browser/bookmarks", nil, "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var listing bookmarkListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) != 1 || listing.Items[0].Type != "folder" || listing.Items[0].Name != "影片" {
		t.Fatalf("unexpected tree: %+v", listing.Items)
	}
	if children := listing.Items[0].Children; len(children) != 1 || children[0].Path != dir || children[0].Name != "视频目录" {
		t.Fatalf("unexpected children: %+v", children)
	}

	// Files can be bookmarked too; the list reports them as non-directories.
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	w = call("POST", "/api/admin/file-browser/bookmarks/items", strings.NewReader(fmt.Sprintf(`{"folder":0,"path":%q}`, file)), "application/json")
	if w.Code != 201 {
		t.Fatalf("file bookmark: %d %s", w.Code, w.Body.String())
	}
	var fileItem struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fileItem); err != nil || fileItem.ID == 0 {
		t.Fatalf("file bookmark body: %v %s", err, w.Body.String())
	}
	w = call("GET", "/api/admin/file-browser/bookmarks", nil, "")
	listing = bookmarkListResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &listing)
	var sawFile bool
	for _, node := range listing.Items {
		if node.ID == fileItem.ID {
			sawFile = node.Directory == false
		}
	}
	if !sawFile {
		t.Fatalf("file bookmark not reported as a file: %+v", listing.Items)
	}
	w = call("DELETE", fmt.Sprintf("/api/admin/file-browser/bookmarks/items/%d", fileItem.ID), nil, "")
	if w.Code != 200 {
		t.Fatalf("delete file bookmark: %d %s", w.Code, w.Body.String())
	}

	w = call("DELETE", fmt.Sprintf("/api/admin/file-browser/bookmarks/items/%d", item.ID), nil, "")
	if w.Code != 200 {
		t.Fatalf("delete item: %d %s", w.Code, w.Body.String())
	}
	// The folder now contains no items but still exists.
	w = call("GET", "/api/admin/file-browser/bookmarks", nil, "")
	listing = bookmarkListResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &listing)
	if len(listing.Items) != 1 || len(listing.Items[0].Children) != 0 {
		t.Fatalf("item delete not reflected: %+v", listing.Items)
	}
	w = call("DELETE", fmt.Sprintf("/api/admin/file-browser/bookmarks/folders/%d", folder.ID), nil, "")
	if w.Code != 200 {
		t.Fatalf("delete folder: %d %s", w.Code, w.Body.String())
	}
	w = call("GET", "/api/admin/file-browser/bookmarks", nil, "")
	listing = bookmarkListResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &listing)
	if len(listing.Items) != 0 {
		t.Fatalf("folder delete not reflected: %+v", listing.Items)
	}
}
