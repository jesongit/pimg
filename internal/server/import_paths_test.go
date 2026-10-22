package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAbsoluteImportPathsDoNotRequireConfiguredRootAndSurviveSettingChanges(t *testing.T) {
	s := importServer(t)
	s.Cfg.Paths.ImportDir = filepath.Join(t.TempDir(), "not-mounted")
	source := t.TempDir()
	photo := filepath.Join(source, "photo.png")
	if err := os.WriteFile(photo, interactionPNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := s.Svc.BrowseImports(source, 0)
	if err != nil || len(entries) != 1 || entries[0].Path != photo {
		t.Fatal(entries, err)
	}
	entry, _, err := s.Svc.BrowseImports(photo, 0)
	if err != nil || len(entry) != 1 || entry[0].Directory {
		t.Fatal(entry, err)
	}
	id, err := s.Svc.CreateLocalImport([]string{source}, s.guestAlbumID, false, true, "now")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s.Cfg.Paths.ImportDir = filepath.Join(t.TempDir(), "different-root")
	if err = s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	batch, err := s.Svc.LocalImportBatch(id)
	if err != nil || batch.State != "done" || batch.Counts["done"] != 1 {
		t.Fatal(batch, err)
	}
	if got, err := os.ReadFile(photo); err != nil || !bytes.Equal(got, interactionPNG(t)) {
		t.Fatal("source changed", err)
	}
}

func TestAbsoluteImportSelectionAcrossDirectoriesIsDurable(t *testing.T) {
	s := importServer(t)
	root := t.TempDir()
	paths := []string{}
	for _, name := range []string{"one", "two"} {
		directory := filepath.Join(root, name)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(directory, "photo.png")
		if err := os.WriteFile(file, interactionPNG(t), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, file)
	}
	id, err := s.Svc.CreateLocalImport(paths, s.guestAlbumID, true, true, "now")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.Svc.LocalImportBatch(id)
	if err != nil || batch.Root != root || len(batch.Paths) != 2 {
		t.Fatal(batch, err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	batch, _ = s.Svc.LocalImportBatch(id)
	if batch.State != "done" || batch.Counts["done"] != 1 || batch.Counts["duplicate"] != 1 {
		t.Fatal(batch)
	}
}
