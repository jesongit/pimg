package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

func TestArchiveSyncIsolatesAccountAlbumLocatorsAcrossRestart(t *testing.T) {
	s, peer, plain := peerFixture(t)
	var files []*dbx.File
	for _, owner := range []string{"user:sync-alpha", "user:sync-beta"} {
		if err := s.DB.CreateAccount(owner, strings.TrimPrefix(owner, "user:"), "hash"); err != nil {
			t.Fatal(err)
		}
		album, err := s.DB.InsertOwnerAlbum(owner, "同名相册")
		if err != nil {
			t.Fatal(err)
		}
		f, err := s.receiveLegacyUploadFixture(dbx.WithOwner(context.Background(), owner), bytes.NewReader(plain), "same.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RunArchive(context.Background(), f.ID); err != nil {
			t.Fatal(err)
		}
		f, err = s.DB.GetFile(f.ID)
		if err != nil || f.OwnerID != owner || f.AlbumID.Int64 != album {
			t.Fatal("initial archive lost its account or album", f, err)
		}
		files = append(files, f)
	}
	if files[0].ID == files[1].ID {
		t.Fatal("same content was deduplicated across accounts")
	}
	if err := s.RunArchiveSync(context.Background(), files[0].ID); err != nil {
		t.Fatal(err)
	}
	restored, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	locators := map[string]bool{}
	for _, f := range files {
		for attempt := 0; attempt < 2; attempt++ {
			if err = restored.RunArchiveSync(context.Background(), f.ID); err != nil {
				t.Fatal("sync retry after runtime reconstruction failed", err)
			}
		}
		for _, target := range []string{"local", peer, "r2"} {
			objects, err := restored.DB.ArchiveObjects(f.ID, target)
			if err != nil || len(objects) == 0 {
				t.Fatal("missing replica", target, err)
			}
			prefix := fmt.Sprintf("accounts/%s/albums/%d/%s/", dbx.OwnerDirectory(f.OwnerID), f.AlbumID.Int64, f.ID)
			for _, object := range objects {
				key := objectReference(object).Key
				managedKey := key
				if target == "r2" {
					// R2 retains its configured archive namespace outside the account path.
					managedKey = strings.TrimPrefix(key, "archives/")
				}
				if !object.Verified || object.AlbumID != f.AlbumID.Int64 || !strings.HasPrefix(managedKey, prefix) {
					t.Fatal("replica was not verified in its account and album", target, object)
				}
				locator := target + "/" + key
				if locators[locator] {
					t.Fatal("replicas from separate accounts share an object locator", locator)
				}
				locators[locator] = true
			}
			r, err := restored.SourceStream(context.WithValue(context.Background(), archiveSourceContext{}, target), f)
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(r)
			r.Close()
			if readErr != nil || !bytes.Equal(got, plain) {
				t.Fatal("account replica did not restore its original bytes", target, readErr)
			}
		}
	}
}
