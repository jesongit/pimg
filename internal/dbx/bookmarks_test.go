package dbx

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFileBookmarkTreeFoldersAndRecursiveDelete(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bookmarks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := AdminOwner

	videoID, err := db.CreateFileBookmark(owner, 0, "/data/Video", "Video")
	if err != nil {
		t.Fatal(err)
	}
	folderID, err := db.CreateFileBookmarkFolder(owner, 0, "影片")
	if err != nil {
		t.Fatal(err)
	}
	movieID, err := db.CreateFileBookmark(owner, folderID, "/data/Movies", "Movies")
	if err != nil {
		t.Fatal(err)
	}
	subID, err := db.CreateFileBookmarkFolder(owner, folderID, "子文件夹")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateFileBookmark(owner, subID, "/data/Nested", "Nested"); err != nil {
		t.Fatal(err)
	}

	// The same path in the same folder is not duplicated.
	dup, err := db.CreateFileBookmark(owner, 0, "/data/Video", "另一个名字")
	if err != nil {
		t.Fatal(err)
	}
	if dup != videoID {
		t.Fatalf("duplicate bookmark created a new row: %d != %d", dup, videoID)
	}

	tree, err := db.ListFileBookmarks(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[0].Type != "bookmark" || tree[0].Name != "Video" || tree[1].Type != "folder" {
		t.Fatalf("unexpected root order: %+v", tree)
	}
	if len(tree[1].Children) != 2 || tree[1].Children[0].ID != movieID || tree[1].Children[1].Type != "folder" {
		t.Fatalf("unexpected folder children: %+v", tree[1].Children)
	}
	if nested := tree[1].Children[1].Children; len(nested) != 1 || nested[0].Path != "/data/Nested" {
		t.Fatalf("unexpected nested children: %+v", nested)
	}
	if other, err := db.ListFileBookmarks("user:other"); err != nil || len(other) != 0 {
		t.Fatalf("bookmarks leaked across owners: %v %v", other, err)
	}

	// Deleting the top folder removes its nested folder and both bookmarks but
	// leaves the direct bar bookmark.
	if err = db.DeleteFileBookmarkFolder(owner, folderID); err != nil {
		t.Fatal(err)
	}
	tree, err = db.ListFileBookmarks(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || tree[0].ID != videoID {
		t.Fatalf("folder delete left extra rows: %+v", tree)
	}
	if err = db.DeleteFileBookmark(owner, videoID); err != nil {
		t.Fatal(err)
	}
	if tree, _ = db.ListFileBookmarks(owner); len(tree) != 0 {
		t.Fatalf("item delete left rows: %+v", tree)
	}
	if err = db.DeleteFileBookmark(owner, videoID); err != ErrBookmarkNotFound {
		t.Fatalf("second item delete error = %v", err)
	}
}

func TestFileBookmarkMoveReorderAndCycleGuard(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bookmarks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := AdminOwner
	a, _ := db.CreateFileBookmark(owner, 0, "/a", "a")
	b, _ := db.CreateFileBookmark(owner, 0, "/b", "b")
	c, _ := db.CreateFileBookmark(owner, 0, "/c", "c")
	names := func(nodes []*FileBookmarkNode) []string {
		out := []string{}
		for _, node := range nodes {
			out = append(out, node.Name)
		}
		return out
	}
	if err = db.MoveFileBookmark(owner, c, 0, 0); err != nil {
		t.Fatal(err)
	}
	if tree, _ := db.ListFileBookmarks(owner); strings.Join(names(tree), ",") != "c,a,b" {
		t.Fatalf("reorder failed: %v", names(tree))
	}
	folder, _ := db.CreateFileBookmarkFolder(owner, 0, "f")
	if err = db.MoveFileBookmark(owner, b, folder, 0); err != nil {
		t.Fatal(err)
	}
	tree, _ := db.ListFileBookmarks(owner)
	if strings.Join(names(tree), ",") != "c,a,f" || len(tree[2].Children) != 1 || tree[2].Children[0].Name != "b" {
		t.Fatalf("move into folder failed: %+v", tree)
	}
	if err = db.MoveFileBookmark(owner, folder, folder, 0); err != ErrBookmarkCycle {
		t.Fatalf("self move error = %v", err)
	}
	sub, _ := db.CreateFileBookmarkFolder(owner, folder, "sub")
	if err = db.MoveFileBookmark(owner, folder, sub, 0); err != ErrBookmarkCycle {
		t.Fatalf("descendant move error = %v", err)
	}
	_ = a
}

func TestFileBookmarkFolderValidationAndRename(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bookmarks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.CreateFileBookmarkFolder(AdminOwner, 0, "   "); err != ErrBookmarkName {
		t.Fatalf("blank folder name error = %v", err)
	}
	if _, err = db.CreateFileBookmarkFolder(AdminOwner, 999, "x"); err != ErrBookmarkFolder {
		t.Fatalf("missing parent error = %v", err)
	}
	id, err := db.CreateFileBookmarkFolder(AdminOwner, 0, "旧名字")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.RenameFileBookmarkFolder(AdminOwner, id, "新名字"); err != nil {
		t.Fatal(err)
	}
	tree, err := db.ListFileBookmarks(AdminOwner)
	if err != nil || len(tree) != 1 || tree[0].Name != "新名字" {
		t.Fatalf("rename did not apply: %+v %v", tree, err)
	}
	if _, err = db.CreateFileBookmark(AdminOwner, 0, "", "空路径"); err != ErrBookmarkPathEmpty {
		t.Fatalf("blank path error = %v", err)
	}
}
