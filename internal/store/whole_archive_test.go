package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/storage"
)

func TestOriginalOnlyArchiveAndDatabaseFreeRecovery(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "encrypted", true: "plaintext"}[direct], func(t *testing.T) {
			s, _, _ := newTestEnv(t)
			if err := s.SetStorage("local", "local"); err != nil {
				t.Fatal(err)
			}
			plain := workflowPNG(t)
			f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(plain), "original.png", "png", sql.NullInt64{}, nil, 0, direct)
			if err != nil {
				t.Fatal(err)
			}
			if direct {
				err = s.RunDirectArchive(context.Background(), f.ID)
			} else {
				err = s.RunArchive(context.Background(), f.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, _ = s.DB.GetFile(f.ID)
			var count, aux int
			if err := s.DB.QueryRow(`SELECT count(*),COALESCE(sum(kind!='data'),0) FROM file_chunks WHERE file_id=?`, f.ID).Scan(&count, &aux); err != nil {
				t.Fatal(err)
			}
			if count != 1 || aux != 0 || !f.HasThumb || f.HasBThumb {
				t.Fatal("auxiliary archive", count, aux)
			}
			root := s.coldBackends["local"].(storage.LocalCold).Disk.Dir
			files := []string{}
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					files = append(files, path)
				}
				return nil
			})
			if len(files) != 1 {
				t.Fatal("expected only original; no manifest/preview", files)
			}
			wire, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if direct {
				if !bytes.Equal(wire, plain) {
					t.Fatal("plaintext not directly readable")
				}
			} else {
				// Recovery accepts only this physical archive and the master.
				var recovered bytes.Buffer
				if _, err := carrier.Recover(bytes.NewReader(wire), &recovered, s.Master); err != nil || !bytes.Equal(recovered.Bytes(), plain) {
					t.Fatal("offline recovery", err)
				}
			}
			_ = os.Remove(s.previewPath(f))
			s.previews = newPreviewCache(1<<20, 0)
			preview, err := s.PreviewThumbnail(context.Background(), f)
			if err != nil || len(preview) == 0 {
				t.Fatal("local preview regeneration", err)
			}
			if _, err := s.GetTempView(context.Background(), f, "thumb"); err == nil {
				t.Fatal("thumbnail stored in temporary provider")
			}
			stream, err := s.SourceStream(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(stream)
			stream.Close()
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatal("normal source load", err)
			}
		})
	}
}

func TestTelegramOriginalRecoversAfterDatabaseClosed(t *testing.T) {
	s, remote := workflowEnv(t)
	album, _ := workflowBound(t, s, remote)
	f := workflowArchivedFile(t, s, album)
	parts, err := s.DB.ListChunks(f.ID, "data")
	if err != nil || len(parts) != 1 {
		t.Fatal(err)
	}
	ref, err := s.TGReference(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), remote.data[ref.MessageID]...)
	if len(remote.data) != 1 {
		t.Fatal("Telegram must contain only the original", len(remote.data))
	}
	master := s.Master
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	var recovered bytes.Buffer
	if _, err = carrier.Recover(bytes.NewReader(wire), &recovered, master); err != nil || !bytes.Equal(recovered.Bytes(), workflowPNG(t)) {
		t.Fatal("closed DB recovery", err)
	}
}

func TestStreamingSyncCanRetryAnotherSourceWithoutDiskSpool(t *testing.T) {
	s, _, plain := peerFixture(t)
	f := archiveMedia(t, s, plain)
	if err := s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	parts, _ := s.DB.ListChunks(f.ID, "data")
	disk := s.coldBackends["local"].(storage.LocalCold).Disk
	path := filepath.Join(disk.Dir, filepath.FromSlash(parts[0].ObjectKey))
	wire, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wire[len(wire)-1] ^= 1
	if err = os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE archive_objects SET verified=0 WHERE file_id=? AND instance_id='r2'`, f.ID); err != nil {
		t.Fatal(err)
	}
	s.storages.policy.SyncCacheMB = 0
	if err = s.RunArchiveSync(context.Background(), f.ID); err != nil {
		t.Fatal("streaming repair", err)
	}
	fresh, _ := s.DB.GetFile(f.ID)
	input, err := s.SourceStream(context.WithValue(context.Background(), archiveSourceContext{}, "r2"), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(input)
	input.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("streamed copy corrupted", err)
	}
	spools, _ := filepath.Glob(filepath.Join(s.ctDir, "peer-*.part"))
	if len(spools) != 0 {
		t.Fatal("stream path created a whole-file spool")
	}
}
