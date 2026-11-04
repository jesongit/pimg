package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndHardlinkBrowserItems(t *testing.T) {
	s, _, _ := newTestEnv(t)
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	os.MkdirAll(parent, 0755)
	if err := s.CreateBrowserDir(parent, "子目录"); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(parent, "子目录")
	if st, err := os.Stat(sub); err != nil || !st.IsDir() {
		t.Fatal("mkdir failed", err)
	}
	// create a file
	src := filepath.Join(parent, "a.txt")
	os.WriteFile(src, []byte("x"), 0644)
	// hardlink a file into a target dir
	target := filepath.Join(root, "target")
	os.MkdirAll(target, 0755)
	if err := s.HardlinkBrowserItems([]string{src}, target); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(target, "a.txt")
	if _, err := os.Stat(linked); err != nil {
		t.Fatal("hardlink missing", err)
	}
	// hardlink a directory (recursive)
	srcdir := filepath.Join(parent, "sub1")
	os.MkdirAll(filepath.Join(srcdir, "inner"), 0755)
	os.WriteFile(filepath.Join(srcdir, "inner", "b.txt"), []byte("y"), 0644)
	if err := s.HardlinkBrowserItems([]string{srcdir}, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "sub1", "inner", "b.txt")); err != nil {
		t.Fatal("dir hardlink recurse failed", err)
	}
}