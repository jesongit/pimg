package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/config"
)

func defaultImportFormats(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv("PIMG_DATA_DIR", t.TempDir())
	t.Setenv("PIMG_UPLOAD_EXTS", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.Cfg.Upload.AllowedExts = cfg.Upload.AllowedExts
	s.applyDynamicFromSettings()
}

func TestLocalImportScansAddedFormatsAndRespectsPolicy(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprint("restricted=", restricted), func(t *testing.T) {
			s := importServer(t)
			defaultImportFormats(t, s)
			if restricted {
				if err := s.DB.SetSetting("site.allowed_exts", "png"); err != nil {
					t.Fatal(err)
				}
				s.applyDynamicFromSettings()
			}
			root := t.TempDir()
			s.Cfg.Paths.ImportDir = root
			names := []string{"photo.HEIC", "photo.HeIf", "video.ts", "video.m2ts", "video.MTS", "video.avi", "video.flv", "old.MOV", "photo.jpeg", "photo.png", "note.txt"}
			for _, name := range names {
				if err := os.WriteFile(filepath.Join(root, name), []byte("scan fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.Svc.CreateLocalImport([]string{"."}, s.guestAlbumID, false, true, "now")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			items, err := s.Svc.LocalImportItems(id, 0)
			if err != nil || len(items) != len(names) {
				t.Fatal(items, err)
			}
			for _, item := range items {
				want := "pending"
				if strings.HasSuffix(item.Path, ".txt") || restricted && !strings.HasSuffix(item.Path, ".png") {
					want = "skipped"
				}
				if item.State != want {
					t.Errorf("%s: got %s (%s), want %s", item.Path, item.State, item.Error, want)
				}
			}
		})
	}
}

// Run with a real HEIC fixture; the ordinary scan regression requires no codec.
func TestLocalImportRealHEICKeepsOriginalBytes(t *testing.T) {
	fixture := os.Getenv("PIMG_TEST_HEIC_FIXTURE")
	if fixture == "" {
		t.Skip("set PIMG_TEST_HEIC_FIXTURE to a real HEIC image")
	}
	plain, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	s := importServer(t)
	defaultImportFormats(t, s)
	root := t.TempDir()
	s.Cfg.Paths.ImportDir = root
	source := filepath.Join(root, "photo.HEIC")
	if err = os.WriteFile(source, plain, 0400); err != nil {
		t.Fatal(err)
	}
	id, err := s.Svc.CreateLocalImport([]string{"photo.HEIC"}, s.guestAlbumID, false, false, "now")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.RunLocalImportScan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = s.Svc.ChangeLocalImport(id, "start"); err != nil {
		t.Fatal(err)
	}
	finishImport(t, s, id)
	items, err := s.Svc.LocalImportItems(id, 0)
	if err != nil || len(items) != 1 || items[0].State != "done" || len(items[0].FileIDs) != 1 {
		t.Fatal("HEIC import did not finish", items, err)
	}
	f, err := s.DB.GetFile(items[0].FileIDs[0])
	if err != nil || f.Ext != "heic" || f.Kind != "image" || f.Mime != "image/heic" || f.Published {
		t.Fatal(f, err)
	}
	w := carrierRequest(t, s)("GET", "/api/admin/files/"+f.ID+"/download", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatal("imported HEIC download changed", w.Code)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("source HEIC changed", err)
	}
}
