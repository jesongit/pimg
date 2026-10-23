package store

import (
	"io/fs"
	"os"
)

// os.Root.FS requires UTF-8 paths. Import traversal also needs Unix byte
// names, with the same confinement, read-only and symlink rules as viewing.
type nativeImportFS struct{ root *os.Root }

func (f nativeImportFS) Stat(p string) (fs.FileInfo, error) { return importInfo(f.root, p) }

func (f nativeImportFS) Open(p string) (fs.File, error) {
	info, err := importInfo(f.root, p)
	if err != nil {
		return nil, err
	}
	file, err := f.root.Open(p)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrSourceChanged
	}
	return file, nil
}
