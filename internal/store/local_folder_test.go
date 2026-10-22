package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"reflect"
	"strings"
	"testing"
)

func folderWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func folderRow(t *testing.T, s *Service, id string, c InstanceConfig) dbx.StorageInstance {
	t.Helper()
	b, err := s.sealConfig(id, c)
	if err != nil {
		t.Fatal(err)
	}
	row := dbx.StorageInstance{ID: id, Name: "旧本地文件夹", Driver: "local", Config: b, Enabled: true}
	if err = s.DB.SaveStorageInstance(row); err != nil {
		t.Fatal(err)
	}
	return row
}

func folderConfigBytes(t *testing.T, s *Service, id string) []byte {
	t.Helper()
	var b []byte
	if err := s.DB.QueryRow(`SELECT config FROM storage_instances WHERE id=?`, id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Capture indexed media data verbatim; migration must not rewrite any locator.
func folderIndex(t *testing.T, s *Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"files", "file_chunks", "archive_objects", "archive_manifests", "archive_garbage", "media_replicas", "temp_objects", "albums"} {
		rows, err := s.DB.Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var items [][]any
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			items = append(items, values)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		b, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = string(b)
	}
	return out
}

func folderTreeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		h, err := datalayout.Hash(path)
		out[filepath.ToSlash(rel)] = h
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLocalFolderLegacyUpgradePreservesIndexesBytesAndSignedLinks(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	plain := tinyMedia(t)
	legacy := archiveMedia(t, s, plain)
	if err := s.RunTempMaterialize(ctx, legacy.ID, "original"); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetTempView(ctx, legacy, "original")
	if err != nil {
		t.Fatal(err)
	}
	pub := unpublishedPlainArchive(t, s, plain)
	if err = s.RunPublish(ctx, pub.ID, false); err != nil {
		t.Fatal(err)
	}
	pub, _ = s.DB.GetFile(pub.ID)
	publicURL := s.PublicURL(pub)
	mediaPlain := append(append([]byte{}, plain...), 0x7f) // distinct valid PNG bytes, avoid deduplication with the legacy fixture
	media, err := s.ReceiveUpload(WithPrivateUpload(ctx), bytes.NewReader(mediaPlain), "new.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, media.ID); err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance("local")
	cold, hot := s.localDirectories("local", i.config)
	source := t.TempDir()
	oldCold, oldHot := filepath.Join(source, "cold"), filepath.Join(source, "hot")
	if err = datalayout.CopyTree(cold, oldCold); err != nil {
		t.Fatal(err)
	}
	if err = datalayout.CopyTree(hot, oldHot); err != nil {
		t.Fatal(err)
	}
	c := i.config
	c.LocalLayout = 0
	c.Directory = oldHot
	c.ColdDirectory = oldCold
	c.Managed = true
	folderRow(t, s, "local", c)
	beforeIndex := folderIndex(t, s)
	beforeCold, beforeHot := folderTreeHashes(t, oldCold), folderTreeHashes(t, oldHot)
	resumed, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := resumed.instance("local")
	newCold, newHot := resumed.localDirectories("local", after.config)
	if after.config.LocalLayout != localFolderLayout || after.config.ColdDirectory != "" || after.config.LocalPaths == nil {
		t.Fatal("not one folder", after.config)
	}
	if !sameLocalPath(oldCold, newCold) || !sameLocalPath(oldHot, newHot) {
		t.Fatal("configuration update changed a physical storage path")
	}
	if !reflect.DeepEqual(beforeIndex, folderIndex(t, resumed)) {
		t.Fatal("media indexes changed")
	}
	for _, pair := range [][2]string{{oldCold, newCold}, {oldHot, newHot}} {
		if !reflect.DeepEqual(folderTreeHashes(t, pair[0]), folderTreeHashes(t, pair[1])) {
			t.Fatal("bytes or signing key changed")
		}
	}
	if !reflect.DeepEqual(beforeCold, folderTreeHashes(t, oldCold)) || !reflect.DeepEqual(beforeHot, folderTreeHashes(t, oldHot)) {
		t.Fatal("source changed")
	}
	if _, err = resumed.Verify(ctx, legacy.ID); err != nil {
		t.Fatal(err)
	}
	replicas, err := resumed.DB.MediaReplicas(media.ID)
	if err != nil || len(replicas) == 0 {
		t.Fatal(err)
	}
	r, err := resumed.openMediaPart(ctx, media, replicas[0], replicas[0].Parts[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, mediaPlain) {
		t.Fatal("new media source changed", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /media/{scope}/{key...}", resumed.ServeLocalMedia)
	for _, address := range []string{view.URL, publicURL} {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", u.RequestURI(), nil))
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), plain) {
			t.Fatal("old signed/public link failed", rec.Code, rec.Body.String())
		}
	}
	sealed := folderConfigBytes(t, resumed, "local")
	// Old file/env directories can disappear after a successful migration. A
	// restart must use the saved folder without recreating their signing key.
	s.Cfg.Storage.LocalColdDir, s.Cfg.Storage.LocalHotDir = filepath.Join(source, "retired-cold"), filepath.Join(source, "retired-hot")
	again, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealed, folderConfigBytes(t, again, "local")) {
		t.Fatal("restart rewrote configuration")
	}
	if _, err = os.Stat(filepath.Join(source, "retired-hot")); !os.IsNotExist(err) {
		t.Fatal("retired path was recreated", err)
	}
	if !reflect.DeepEqual(beforeIndex, folderIndex(t, again)) {
		t.Fatal("restart changed indexes")
	}
	if err = again.localHot.PutObject(ctx, storage.Permanent, "after-upgrade.txt", strings.NewReader("new"), 3, "text/plain", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(newHot, "permanent", "after-upgrade.txt")); err != nil {
		t.Fatal("new writes outside managed folder", err)
	}
}

func TestLocalFolderConfigUpdateDoesNotTouchMedia(t *testing.T) {
	s, _, _ := newTestEnv(t)
	source := t.TempDir()
	cold, hot := filepath.Join(source, "cold"), filepath.Join(source, "hot")
	folderWrite(t, filepath.Join(cold, "a.bin"), "original")
	folderWrite(t, filepath.Join(hot, ".signing-key"), strings.Repeat("s", 32))
	folderWrite(t, filepath.Join(source, "pimg-data", "storage", "legacy-folder", "archives", "a.bin"), "unrelated existing file")
	c := InstanceConfig{Bootstrap: true, Managed: true, Directory: hot, ColdDirectory: cold, Priority: 7, VideoPartMB: 128, Encrypt: true, SplitVideo: true}
	row := folderRow(t, s, "legacy-folder", c)
	before := folderTreeHashes(t, source)
	index := folderIndex(t, s)
	updated, next, err := s.migrateLocalLayout(row, c)
	if err != nil {
		t.Fatal(err)
	}
	if next.LocalPaths == nil || next.ColdDirectory != "" || next.Priority != 7 || next.VideoPartMB != 128 || !next.Encrypt || !next.SplitVideo {
		t.Fatal("configuration or options changed")
	}
	a, b := s.localDirectories(row.ID, next)
	if !sameLocalPath(a, cold) || !sameLocalPath(b, hot) {
		t.Fatal("physical paths changed")
	}
	if !reflect.DeepEqual(before, folderTreeHashes(t, source)) || !reflect.DeepEqual(index, folderIndex(t, s)) {
		t.Fatal("configuration update changed files or indexes")
	}
	again, _, err := s.migrateLocalLayout(updated, next)
	if err != nil || !bytes.Equal(updated.Config, again.Config) {
		t.Fatal("restart rewrote configuration", err)
	}
	if _, err := os.Stat(filepath.Join(source, "archives")); !os.IsNotExist(err) {
		t.Fatal("new storage folder was created")
	}
}

func TestLocalFolderCustomLayoutsAdoptExistingDirectories(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			s, _, _ := newTestEnv(t)
			parent := t.TempDir()
			id := "old-custom"
			root := parent
			if version == 1 {
				root = filepath.Join(parent, "pimg-data", "storage", id)
			}
			folderWrite(t, filepath.Join(root, "archives", "source.bin"), "original")
			folderWrite(t, filepath.Join(root, "display", ".signing-key"), strings.Repeat("s", 32))
			c := InstanceConfig{Directory: parent, LocalLayout: version, Managed: true, VideoPartMB: 256}
			row := folderRow(t, s, id, c)
			before := folderTreeHashes(t, parent)
			_, next, err := s.migrateLocalLayout(row, c)
			if err != nil {
				t.Fatal(err)
			}
			if !sameLocalPath(s.localPath(next.Directory), root) || next.ColdDirectory != "" {
				t.Fatal("not reusing existing instance folder")
			}
			if !reflect.DeepEqual(before, folderTreeHashes(t, parent)) {
				t.Fatal("unchanged layout copied or modified bytes")
			}
		})
	}
}

func TestLocalFolderCanonicalDefaultReusesDirectories(t *testing.T) {
	s, _, _ := newTestEnv(t)
	root := filepath.Join(s.Cfg.Paths.DataDir, "storage", "adopt")
	folderWrite(t, filepath.Join(root, "archives", "source.bin"), "original")
	folderWrite(t, filepath.Join(root, "display", ".signing-key"), strings.Repeat("s", 32))
	c := InstanceConfig{Bootstrap: true, Managed: true, Directory: filepath.Join(root, "display"), ColdDirectory: filepath.Join(root, "archives")}
	row := folderRow(t, s, "adopt", c)
	before := folderTreeHashes(t, root)
	_, next, err := s.migrateLocalLayout(row, c)
	if err != nil {
		t.Fatal(err)
	}
	if next.Directory != "@data/storage/adopt" || next.ColdDirectory != "" {
		t.Fatal("default folder not portable", next)
	}
	if !reflect.DeepEqual(before, folderTreeHashes(t, root)) {
		t.Fatal("canonical data changed")
	}
}

func TestLocalFolderOldUnusualPathsRemainCompatible(t *testing.T) {
	for _, scenario := range []string{"overlapping-paths", "missing-folder", "future-version"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := newTestEnv(t)
			source := t.TempDir()
			cold, hot := filepath.Join(source, "cold"), filepath.Join(source, "hot")
			folderWrite(t, filepath.Join(cold, "original.bin"), "original")
			folderWrite(t, filepath.Join(hot, ".signing-key"), strings.Repeat("s", 32))
			c := InstanceConfig{Bootstrap: true, Managed: true, Directory: hot, ColdDirectory: cold}
			switch scenario {
			case "overlapping-paths":
				c.ColdDirectory = source
			case "missing-folder":
				c.Bootstrap = false
				c.Directory = ""
			case "future-version":
				c.LocalLayout = 99
			}
			row := folderRow(t, s, "old-layout", c)
			files := folderTreeHashes(t, source)
			index := folderIndex(t, s)
			_, next, err := s.migrateLocalLayout(row, c)
			if scenario == "overlapping-paths" {
				if err != nil {
					t.Fatal(err)
				}
				a, b := s.localDirectories(row.ID, next)
				if !sameLocalPath(a, c.ColdDirectory) || !sameLocalPath(b, c.Directory) {
					t.Fatal("old paths changed")
				}
			} else {
				if err == nil || !bytes.Equal(row.Config, folderConfigBytes(t, s, row.ID)) {
					t.Fatal("invalid configuration changed")
				}
			}
			if !reflect.DeepEqual(files, folderTreeHashes(t, source)) || !reflect.DeepEqual(index, folderIndex(t, s)) {
				t.Fatal("files or indexes changed")
			}
		})
	}
}

func TestLocalFolderNewInstanceAndDirectoryChangesRemainProtected(t *testing.T) {
	s, _, _ := newTestEnv(t)
	root := t.TempDir()
	id, err := s.SaveStorageInstance("", "一个文件夹", "local", InstanceConfig{Directory: root}, true)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(id)
	if i.config.LocalLayout != localFolderLayout || i.config.ColdDirectory != "" {
		t.Fatal("not unified")
	}
	if err = i.hot.PutObject(context.Background(), storage.Permanent, "sample", strings.NewReader("test"), 4, "text/plain", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "display", "permanent", "sample")); err != nil {
		t.Fatal(err)
	}
	if i.config.Encrypt || i.config.SplitVideo || i.config.VideoPartMB != 256 {
		t.Fatal("local defaults changed")
	}
	c := i.config
	c.ColdDirectory = t.TempDir()
	if _, err = s.SaveStorageInstance(id, "不允许双目录", "local", c, true); err == nil {
		t.Fatal("second folder accepted")
	}
	p := s.StoragePolicy()
	p.Archives = []string{id}
	p.Permanent = id
	p.Temporary = id
	if err = s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	c = i.config
	c.Directory = t.TempDir()
	if _, err = s.SaveStorageInstance(id, "不可直接移动", "local", c, true); err == nil {
		t.Fatal("active instance moved")
	}
	view := s.StorageInstances()
	for _, v := range view {
		if v.Driver == "local" && v.Config.ColdDirectory != "" {
			t.Fatal("old second directory exposed")
		}
	}
	for _, driver := range s.StorageDrivers() {
		if driver.ID == "local" && (len(driver.Fields) != 1 || driver.Fields[0].Key != "directory" || driver.Fields[0].Label != "存储文件夹") {
			t.Fatal("local editor still has multiple folder fields")
		}
	}
}

func TestLocalFolderCommitFailureRetainsOriginalConfiguration(t *testing.T) {
	s, _, _ := newTestEnv(t)
	source := t.TempDir()
	c := InstanceConfig{Bootstrap: true, Managed: true, Directory: filepath.Join(source, "hot"), ColdDirectory: filepath.Join(source, "cold")}
	folderWrite(t, filepath.Join(c.ColdDirectory, "source.bin"), "original")
	folderWrite(t, filepath.Join(c.Directory, ".signing-key"), strings.Repeat("s", 32))
	row := folderRow(t, s, "commit-failure", c)
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_folder_commit BEFORE UPDATE OF config ON storage_instances WHEN NEW.id='commit-failure' BEGIN SELECT RAISE(ABORT,'synthetic commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.migrateLocalLayout(row, c); err == nil {
		t.Fatal("database failure ignored")
	}
	if !bytes.Equal(row.Config, folderConfigBytes(t, s, row.ID)) {
		t.Fatal("failed commit changed configuration")
	}
	if _, err := s.DB.Exec(`DROP TRIGGER reject_folder_commit`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.migrateLocalLayout(row, c); err != nil {
		t.Fatal("retry after failed commit did not resume", err)
	}
}
