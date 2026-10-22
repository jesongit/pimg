package tguser

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gotd/td/session"
)

func TestEncryptedSessionRoundTripAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "session.bin")
	key := bytes.Repeat([]byte{0x37}, 32)
	storage, err := newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	payload := []byte(`{"auth_key":"secret-session-material","account_id":123456789}`)
	if err := storage.StoreSession(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first, payload) || bytes.Contains(first, []byte("secret-session-material")) || bytes.Contains(first, key) {
		t.Fatal("saved session contains plaintext or the supplied key")
	}
	if !bytes.HasPrefix(first, []byte(sessionHeader)) {
		t.Fatal("saved session has no versioned header")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("session mode: %o", info.Mode().Perm())
		}
	}
	// Rebuilding storage proves the session does not rely on a memory cache.
	restarted, err := newSessionStorage(path, bytes.Clone(key))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.LoadSession(context.Background())
	if err != nil || !bytes.Equal(loaded, payload) {
		t.Fatalf("restart round trip failed: %v", err)
	}
	// An existing target must be replaceable on Windows as well as Unix.
	if err := restarted.StoreSession(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("repeated save reused the GCM nonce")
	}
	replacement := []byte("replacement session")
	if err := restarted.StoreSession(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	loaded, err = storage.LoadSession(context.Background())
	if err != nil || !bytes.Equal(loaded, replacement) {
		t.Fatalf("replacement round trip failed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatal("temporary session files were left behind")
	}
}

func TestEncryptedSessionRejectsWrongKeyAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.bin")
	key := bytes.Repeat([]byte{0x23}, 32)
	storage, err := newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(context.Background(), []byte("private session bytes")); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := newSessionStorage(path, bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.LoadSession(context.Background()); !errors.Is(err, errSessionInvalid) {
		t.Fatalf("wrong key accepted: %v", err)
	}
	cases := map[string][]byte{
		"empty":      {},
		"plaintext":  []byte("private session bytes"),
		"header":     bytes.Clone(original),
		"version":    bytes.Clone(original),
		"nonce":      bytes.Clone(original),
		"ciphertext": bytes.Clone(original),
		"tag":        bytes.Clone(original),
		"truncated":  original[:len(original)-1],
		"short":      original[:len(sessionHeader)+storage.aead.NonceSize()],
		"appended":   append(bytes.Clone(original), 0),
	}
	cases["header"][0] ^= 1
	cases["version"][len(sessionHeader)-1] ^= 1
	cases["nonce"][len(sessionHeader)] ^= 1
	cases["ciphertext"][len(sessionHeader)+storage.aead.NonceSize()] ^= 1
	cases["tag"][len(original)-1] ^= 1
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if data, err := storage.LoadSession(context.Background()); data != nil || !errors.Is(err, errSessionInvalid) {
				t.Fatalf("corrupt session accepted: %v", err)
			}
		})
	}
}

func TestEncryptedSessionCancellationDoesNotMutate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "not-created", "session.bin")
	storage, err := newSessionStorage(path, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(ctx, []byte("canceled session")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save: %v", err)
	}
	if _, err := storage.LoadSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled save created storage: %v", err)
	}
	if err := storage.StoreSession(context.Background(), []byte("keep this session")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(ctx, []byte("canceled replacement")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("canceled replacement changed the saved session")
	}
}

func TestEncryptedSessionCancellationWhileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.bin")
	storage, err := newSessionStorage(path, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	storage.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- storage.StoreSession(ctx, []byte("must not be saved")) }()
	cancel()
	storage.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("save after canceled lock wait: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled lock wait created a session: %v", err)
	}
}

func TestEncryptedSessionRejectsInvalidConfiguration(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	for _, path := range []string{"", " \t", ".", "..", "bad\x00path", t.TempDir()} {
		if _, err := newSessionStorage(path, key); !errors.Is(err, errSessionPath) {
			t.Fatalf("invalid path accepted: %q: %v", path, err)
		}
	}
	for _, size := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := newSessionStorage(filepath.Join(t.TempDir(), "session.bin"), make([]byte, size)); !errors.Is(err, errSessionKey) {
			t.Fatalf("invalid key length accepted: %d: %v", size, err)
		}
	}
	storage, err := newSessionStorage(filepath.Join("relative-session-dir", "session.bin"), key)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(storage.path) {
		t.Fatal("relative storage path was made absolute")
	}
}

func TestEncryptedSessionFilesystemErrorsAreSanitized(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sensitive-parent")
	path := filepath.Join(parent, "sensitive-session.bin")
	storage, err := newSessionStorage(path, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(context.Background(), []byte("secret payload")); err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("save error was not sanitized: %v", err)
	}
	if _, err := storage.LoadSession(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("load error was not sanitized: %v", err)
	}
}

func TestEncryptedSessionClearRetiresInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.bin")
	key := bytes.Repeat([]byte{1}, 32)
	storage, err := newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(context.Background(), []byte("retired session")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.Clear(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled clear: %v", err)
	}
	if _, err := storage.LoadSession(context.Background()); err != nil {
		t.Fatalf("canceled clear removed the session: %v", err)
	}
	if err := storage.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(context.Background(), []byte("late save from the old client")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("retired storage loaded a session: %v", err)
	}
	if err := storage.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late save recreated the session: %v", err)
	}
	// New login clients can use a fresh instance at the same configured path.
	fresh, err := newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.StoreSession(context.Background(), []byte("new login")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("retired instance loaded a new login session: %v", err)
	}
}

func TestEncryptedSessionClearMissingAndFailure(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	path := filepath.Join(t.TempDir(), "session.bin")
	storage, err := newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Clear(context.Background()); err != nil {
		t.Fatalf("clearing a missing session: %v", err)
	}
	if !storage.disabled {
		t.Fatal("clearing a missing session did not retire storage")
	}
	storage, err = newSessionStorage(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := storage.Clear(context.Background()); !errors.Is(err, errSessionClear) || strings.Contains(err.Error(), path) {
		t.Fatalf("clear failure was not sanitized: %v", err)
	}
	if storage.disabled {
		t.Fatal("failed clear retired storage")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("clear removed a directory: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := storage.StoreSession(context.Background(), []byte("still enabled")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadSession(context.Background()); err != nil {
		t.Fatalf("failed clear disabled storage: %v", err)
	}
}
