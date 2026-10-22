package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pimg/internal/ids"
)

// Disk uses os.Root so even symlinks cannot escape the configured directory.
type Disk struct{ Dir string }

func (d Disk) root() (*os.Root, error) { return os.OpenRoot(d.Dir) }
func localName(key string) (string, error) {
	if strings.Contains(key, "\\") {
		return "", errors.New("非法存储路径")
	}
	return filepath.Localize(key)
}
func NewDisk(dir string) (Disk, error) {
	err := os.MkdirAll(dir, 0o700)
	return Disk{Dir: dir}, err
}
func (d Disk) Put(ctx context.Context, key string, src io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := localName(key)
	if err != nil {
		return err
	}
	root, err := d.root()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp := name + "." + ids.New() + ".part"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	n, copyErr := io.Copy(f, src)
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr, ctx.Err()); err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("本地存储大小不匹配：%d != %d", n, size)
	}
	return root.Rename(tmp, name)
}
func (d Disk) Open(key string) (*os.File, error) {
	name, err := localName(key)
	if err != nil {
		return nil, err
	}
	root, err := d.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(name)
}
func (d Disk) Delete(key string) error {
	name, err := localName(key)
	if err != nil {
		return err
	}
	root, err := d.root()
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (d Disk) Check(ctx context.Context) error {
	key := ".check-" + ids.New()
	if err := d.Put(ctx, key, strings.NewReader("ok"), 2); err != nil {
		return err
	}
	f, err := d.Open(key)
	if err != nil {
		d.Delete(key)
		return err
	}
	b, readErr := io.ReadAll(f)
	f.Close()
	err = d.Delete(key)
	if string(b) != "ok" {
		return errors.New("本地存储读写校验失败")
	}
	return errors.Join(readErr, err)
}

type LocalCold struct{ Disk Disk }

func (l LocalCold) Put(ctx context.Context, key string, data []byte, _ string) (Reference, error) {
	err := l.Disk.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
	return Reference{Key: key}, err
}
func (l LocalCold) Open(ctx context.Context, ref Reference) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return l.Disk.Open(ref.Key)
}
func (l LocalCold) Delete(_ context.Context, ref Reference) error { return l.Disk.Delete(ref.Key) }
func (l LocalCold) Check(ctx context.Context) error               { return l.Disk.Check(ctx) }

type LocalHot struct {
	Disk    Disk
	BaseURL string
	// Optional resolver lets the service change its public address without
	// replacing storage handles or mutating fields used by active requests.
	BaseURLFunc func() string
	secret      []byte
}

func NewLocalHot(dir, base string) (*LocalHot, error) {
	disk, err := NewDisk(dir)
	if err != nil {
		return nil, err
	}
	root, err := disk.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	key, err := root.ReadFile(".signing-key")
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, createErr := root.OpenFile(".signing-key", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		_, writeErr := f.Write(key)
		syncErr := f.Sync()
		closeErr := f.Close()
		err = errors.Join(writeErr, syncErr, closeErr)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("本地热存储签名密钥损坏")
	}
	return &LocalHot{Disk: disk, BaseURL: strings.TrimRight(base, "/") + "/media", secret: key}, nil
}
func scoped(scope, key string) (string, error) {
	if scope != Temporary && scope != Permanent {
		return "", errors.New("非法存储区域")
	}
	if _, err := localName(key); err != nil {
		return "", err
	}
	return scope + "/" + key, nil
}
func (l *LocalHot) PutObject(ctx context.Context, scope, key string, src io.Reader, size int64, _, _ string) error {
	name, err := scoped(scope, key)
	if err != nil {
		return err
	}
	return l.Disk.Put(ctx, name, src, size)
}
func (l *LocalHot) HeadSize(ctx context.Context, scope, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	name, err := scoped(scope, key)
	if err != nil {
		return 0, err
	}
	f, err := l.Disk.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
func (l *LocalHot) Delete(_ context.Context, scope, key string) error {
	name, err := scoped(scope, key)
	if err != nil {
		return err
	}
	return l.Disk.Delete(name)
}
func (l *LocalHot) signature(key, expiry string) string {
	h := hmac.New(sha256.New, l.secret)
	io.WriteString(h, "temp\n"+key+"\n"+expiry)
	return hex.EncodeToString(h.Sum(nil))
}
func (l *LocalHot) PresignGet(_ context.Context, scope, key string, ttl time.Duration, _ string) (string, error) {
	if scope != Temporary || ttl <= 0 {
		return "", errors.New("临时签名参数无效")
	}
	if _, err := scoped(scope, key); err != nil {
		return "", err
	}
	expires := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return l.baseURL() + "/temp/" + key + "?expires=" + expires + "&signature=" + l.signature(key, expires), nil
}
func (l *LocalHot) baseURL() string {
	if l.BaseURLFunc != nil {
		return l.BaseURLFunc()
	}
	return l.BaseURL
}
func (l *LocalHot) PublicURL(key string) string              { return l.baseURL() + "/permanent/" + key }
func (l *LocalHot) PurgeCDN(context.Context, []string) error { return nil }
func (l *LocalHot) Check(ctx context.Context) error          { return l.Disk.Check(ctx) }

// Serve serves only a validated object, never directory listings. The caller
// additionally enforces persisted publication and temporary-object lifetime.
func (l *LocalHot) Serve(w http.ResponseWriter, r *http.Request, scope, key string) {
	w.Header().Set("Cache-Control", "private, max-age=0, no-store")
	if scope == Temporary {
		expiry := r.URL.Query().Get("expires")
		expires, err := strconv.ParseInt(expiry, 10, 64)
		if err != nil || expires <= time.Now().Unix() || !hmac.Equal([]byte(r.URL.Query().Get("signature")), []byte(l.signature(key, expiry))) {
			http.Error(w, "链接无效或已过期", http.StatusForbidden)
			return
		}
	}
	name, err := scoped(scope, key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := l.Disk.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, filepath.Base(key), st.ModTime(), f)
}
