package tguser

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gotd/td/session"
)

const sessionHeader = "PIMG-TGSESSION\x01"

var (
	errSessionPath    = errors.New("telegram session: invalid storage path")
	errSessionKey     = errors.New("telegram session: invalid encryption key")
	errSessionRead    = errors.New("telegram session: could not read storage")
	errSessionWrite   = errors.New("telegram session: could not save storage")
	errSessionClear   = errors.New("telegram session: could not clear storage")
	errSessionInvalid = errors.New("telegram session: invalid encrypted storage")
)

// encryptedSessionStorage persists only authenticated ciphertext. The file
// format is a versioned header, a random GCM nonce, and the sealed session.
type encryptedSessionStorage struct {
	path     string
	aead     cipher.AEAD
	mu       sync.RWMutex
	disabled bool
}

var _ session.Storage = (*encryptedSessionStorage)(nil)

func newSessionStorage(path string, key []byte) (*encryptedSessionStorage, error) {
	if len(key) != 32 {
		return nil, errSessionKey
	}
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return nil, errSessionPath
	}
	path = filepath.Clean(path)
	if base := filepath.Base(path); base == "." || base == ".." || filepath.Dir(path) == path {
		return nil, errSessionPath
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errSessionPath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errSessionPath
	}

	// A separate domain prevents reuse of the application's image-encryption key.
	hash := sha256.New()
	_, _ = hash.Write([]byte("pimg/tguser/session/aes-gcm/v1\x00"))
	_, _ = hash.Write(key)
	derived := hash.Sum(nil)
	block, err := aes.NewCipher(derived)
	clear(derived)
	if err != nil {
		return nil, errSessionKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errSessionKey
	}
	return &encryptedSessionStorage{path: path, aead: aead}, nil
}

func (s *encryptedSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.disabled {
		return nil, session.ErrNotFound
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errSessionRead
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, session.ErrNotFound
		}
		return nil, errSessionRead
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headerLen, nonceLen := len(sessionHeader), s.aead.NonceSize()
	if len(data) < headerLen+nonceLen+s.aead.Overhead() || !bytes.Equal(data[:headerLen], []byte(sessionHeader)) {
		return nil, errSessionInvalid
	}
	plain, err := s.aead.Open(nil, data[headerLen:headerLen+nonceLen], data[headerLen+nonceLen:], data[:headerLen])
	if err != nil {
		return nil, errSessionInvalid
	}
	if err := ctx.Err(); err != nil {
		clear(plain)
		return nil, err
	}
	return plain, nil
}

func (s *encryptedSessionStorage) StoreSession(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.disabled {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errSessionWrite
	}
	sealed := make([]byte, 0, len(sessionHeader)+len(nonce)+len(data)+s.aead.Overhead())
	sealed = append(sealed, sessionHeader...)
	sealed = append(sealed, nonce...)
	sealed = s.aead.Seal(sealed, nonce, data, []byte(sessionHeader))
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errSessionWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".tg-session-*")
	if err != nil {
		return errSessionWrite
	}
	defer func() {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := temp.Chmod(0600); err != nil {
		return errSessionWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := temp.Write(sealed); err != nil {
		return errSessionWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return errSessionWrite
	}
	if err := temp.Close(); err != nil {
		return errSessionWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The temporary file is on the same filesystem. In particular, never remove
	// the old target first: a failed replacement must preserve the saved session.
	if err := os.Rename(temp.Name(), s.path); err != nil {
		return errSessionWrite
	}
	return nil
}

// Clear retires this storage instance after a successful logout. A late save
// from the old client cannot recreate its session; a new client must construct
// a new storage instance before signing in again.
func (s *encryptedSessionStorage) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.disabled {
		return nil
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.disabled = true
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errSessionClear
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errSessionClear
	}
	s.disabled = true
	return nil
}
