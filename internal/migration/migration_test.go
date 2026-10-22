package migration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/store"
)

func fixture(t *testing.T) *store.Service {
	t.Helper()
	root := filepath.Join(t.TempDir(), "pimg-data")
	cfg := &config.Config{Paths: config.Paths{DataDir: root, RuntimeDir: filepath.Join(filepath.Dir(root), "runtime"), DatabaseFile: filepath.Join(root, "system/pimg.db"), TempDir: filepath.Join(root, "work/tmp")}, Storage: config.Storage{Cold: "local", Hot: "local", LocalColdDir: filepath.Join(root, "storage/local/archives"), LocalHotDir: filepath.Join(root, "storage/local/display")}, Telegram: config.Telegram{SessionFile: filepath.Join(root, "system/telegram/session.enc")}, MasterKeyFile: filepath.Join(root, "system/keys/master.key")}
	db, e := dbx.Open(cfg.Paths.DatabaseFile)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = db.EnsureAdmin("original", "kept-password-hash"); e != nil {
		t.Fatal(e)
	}
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	s, e := store.New(cfg, db, master, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func write(t *testing.T, p, body string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestFullTransferRebasesAndPreservesIdentity(t *testing.T) {
	source := fixture(t)
	if e := source.DB.CreateAccount("user:original", "alice", "alice-password-hash"); e != nil {
		t.Fatal(e)
	}
	_, e := source.DB.Exec(`INSERT INTO settings VALUES('example.user.setting','original')`)
	if e != nil {
		t.Fatal(e)
	}
	write(t, source.Cfg.Telegram.SessionFile, "encrypted-tg-session")
	write(t, filepath.Join(source.Cfg.Paths.DataDir, "storage/local/display/kept.bin"), "local-original")
	write(t, filepath.Join(source.Cfg.Paths.DataDir, "previews/kept.webp"), "preview-original")
	write(t, filepath.Join(source.Cfg.Paths.DataDir, "work/uploads/kept.bin"), "pending-upload")
	external := t.TempDir()
	write(t, filepath.Join(external, "kept.bin"), "external-original")
	if _, e := source.SaveStorageInstance("", "external", "local", store.InstanceConfig{Directory: external, LocalLayout: 1}, true); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	m, e := Export(context.Background(), source, "test", AllScopes, &out)
	if e != nil {
		t.Fatal(e)
	}
	target := fixture(t)
	target.DB.Close()
	v, e := Stage(context.Background(), target.Cfg, bytes.NewReader(out.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	if v.Accounts != 2 || len(v.Scopes) != 4 {
		t.Fatalf("bad preview: %+v", v)
	}
	if e = Commit(target.Cfg, v.ID); e != nil {
		t.Fatal(e)
	}
	if e = ApplyPending(target.Cfg); e != nil {
		t.Fatal(e)
	}
	if e = ApplyRuntime(target.Cfg); e != nil {
		t.Fatal(e)
	}
	key, e := target.Cfg.ResolveMasterKey()
	if e != nil || !bytes.Equal(key, source.Master.Raw()) {
		t.Fatal("master not preserved", e)
	}
	db, e := dbx.Open(filepath.Join(Root(target.Cfg), "system/pimg.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	user, e := db.AccountByUsername("alice")
	if e != nil || user.PasswordHash != "alice-password-hash" || user.ID != "user:original" {
		t.Fatal("account changed", e)
	}
	b, e := os.ReadFile(target.Cfg.Telegram.SessionFile)
	if e != nil || string(b) != "encrypted-tg-session" {
		t.Fatal("session changed", e)
	}
	for _, p := range []string{"storage/local/display/kept.bin", "previews/kept.webp", "work/uploads/kept.bin"} {
		if _, e = os.Stat(filepath.Join(Root(target.Cfg), p)); e != nil {
			t.Fatal(p, e)
		}
	}
	for _, rel := range m.External {
		b, e = os.ReadFile(filepath.Join(Root(target.Cfg), rel, "kept.bin"))
		if e != nil || string(b) != "external-original" {
			t.Fatal("external local missing", e)
		}
	}
	// Reopening all encrypted configs also checks credentials can still decrypt.
	restored, e := store.New(target.Cfg, db, source.Master, nil, nil, source.Log)
	if e != nil {
		t.Fatal(e)
	}
	for _, dirs := range restored.MigrationLocalDirectories() {
		for _, dir := range dirs {
			if !within(Root(target.Cfg), dir) {
				t.Fatalf("path still points to source: %s", dir)
			}
		}
	}
	if CanRestore(db) {
		t.Fatal("existing accounts must block overwrite")
	}
}
func TestScopesAndRejectedPackages(t *testing.T) {
	s := fixture(t)
	write(t, s.Cfg.Telegram.SessionFile, "session")
	write(t, filepath.Join(Root(s.Cfg), "previews/a"), "cache")
	write(t, filepath.Join(Root(s.Cfg), "work/uploads/a"), "pending")
	write(t, filepath.Join(Root(s.Cfg), "storage/local/display/a"), "local")
	var out bytes.Buffer
	m, e := Export(context.Background(), s, "test", []string{}, &out)
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"system/telegram/session.enc", "previews/a", "work/uploads/a", "storage/local/display/a"} {
		if m.Entries["data/"+p] != "" {
			t.Fatal("excluded scope leaked", p)
		}
	}
	if _, e = Stage(context.Background(), s.Cfg, bytes.NewReader(out.Bytes())); e != nil {
		t.Fatal(e)
	}
	truncated := out.Bytes()[:out.Len()-5]
	if _, e = Stage(context.Background(), s.Cfg, bytes.NewReader(truncated)); e == nil {
		t.Fatal("accepted truncated gzip")
	}
	for _, name := range []string{"../escape", "data/../../escape", "data/x\\escape", "/absolute"} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		tw.Close()
		gz.Close()
		if _, e = Stage(context.Background(), s.Cfg, &b); e == nil {
			t.Fatal("accepted unsafe path", name)
		}
	}
	if e = Commit(s.Cfg, "../../other"); e == nil {
		t.Fatal("accepted invalid staging id")
	}
}

func TestSplitVolumeTransferKeepsMountRootAndSelectedRuntimeCache(t *testing.T) {
	source := fixture(t)
	write(t, filepath.Join(source.Cfg.Paths.RuntimeDir, "previews/cover.jpg"), "runtime-preview")
	write(t, filepath.Join(source.Cfg.Paths.RuntimeDir, "cache/originals/a.bin"), "runtime-cache")
	write(t, filepath.Join(source.Cfg.Paths.RuntimeDir, "migration/export-old.tar.gz"), "exclude-old-package")
	write(t, filepath.Join(source.Cfg.Paths.DataDir, "work/uploads/pending.bin"), "retry-original")
	write(t, filepath.Join(source.Cfg.Paths.DataDir, "pimg-data/retired.db"), "exclude-old-layout")
	var out bytes.Buffer
	m, err := Export(context.Background(), source, "test", AllScopes, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"data/previews/cover.jpg", "data/cache/originals/a.bin", "data/work/uploads/pending.bin"} {
		if m.Entries[p] == "" {
			t.Fatal("missing selected scope", p)
		}
	}
	for p := range m.Entries {
		if strings.Contains(p, "export-old") || strings.Contains(p, "retired.db") {
			t.Fatal("export included runtime package or old layout", p)
		}
	}
	target := fixture(t)
	target.DB.Close()
	info, err := os.Stat(Root(target.Cfg))
	if err != nil {
		t.Fatal(err)
	}
	v, err := Stage(context.Background(), target.Cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err = Commit(target.Cfg, v.ID); err != nil {
		t.Fatal(err)
	}
	if err = ApplyPending(target.Cfg); err != nil {
		t.Fatal(err)
	}
	next, err := os.Stat(Root(target.Cfg))
	if err != nil || !os.SameFile(info, next) {
		t.Fatal("mount root was replaced", err)
	}
	if err = ApplyRuntime(target.Cfg); err != nil {
		t.Fatal(err)
	}
	release, err := datalayout.Prepare(target.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for p, body := range map[string]string{filepath.Join(target.Cfg.Paths.RuntimeDir, "previews/cover.jpg"): "runtime-preview", filepath.Join(target.Cfg.Paths.RuntimeDir, "cache/originals/a.bin"): "runtime-cache", filepath.Join(Root(target.Cfg), "work/uploads/pending.bin"): "retry-original"} {
		b, e := os.ReadFile(p)
		if e != nil || string(b) != body {
			t.Fatal(p, e)
		}
	}
	if _, err = os.Stat(filepath.Join(Root(target.Cfg), "previews")); !os.IsNotExist(err) {
		t.Fatal("preview cache left in portable root")
	}
}
