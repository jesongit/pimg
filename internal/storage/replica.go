package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"

	"pimg/internal/ids"
)

// Replica owns the mapping from albums and immutable file identities to native
// folders/topics/objects. References are opaque to the sync scheduler. Put must
// be idempotent for album+file+object and reject incomplete/corrupt input before
// committing. An adapter must recover uncertain remote commits on retries.
// Prepare returns a stable operation reference before writing; Open/Delete must
// also resolve it after an interrupted Put, including a lost acknowledgment.
type Replica interface {
	EnsureAlbum(context.Context, ReplicaAlbum) (string, error)
	Prepare(context.Context, string, string, ReplicaObject) (string, error)
	Put(context.Context, string, string, ReplicaObject, io.Reader) (string, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}
type ReplicaAlbum struct {
	OwnerID string `json:"owner_id,omitempty"`
	ID      int64  `json:"id"`
	Name    string `json:"name"`
}
type ReplicaObject struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
}

// LocalReplica confines all operations to a relative directory under DataDir.
// Opaque references contain a stable album ID; renaming a folder never makes
// previously persisted references point at the wrong place.
type LocalReplica struct{ DataDir, Directory string }

func (l LocalReplica) root() (*os.Root, error) {
	base, err := os.OpenRoot(l.DataDir)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	if err = base.MkdirAll(l.Directory, 0o700); err != nil {
		return nil, err
	}
	return base.OpenRoot(l.Directory)
}
func safeReplicaName(name string) string {
	var out strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == ' ' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
		if out.Len() >= 80 {
			break
		}
	}
	n := strings.Trim(out.String(), " .")
	if n == "" {
		return "album"
	}
	return n
}
func replicaAlbumFolder(root *os.Root, id int64) (string, error) {
	dir, err := root.Open("albums")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	entries, err := dir.Readdir(-1)
	if err != nil {
		return "", err
	}
	prefix := strconv.FormatInt(id, 10) + "--"
	found := ""
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			if found != "" {
				return "", errors.New("副仓相册目录编号重复，请先处理冲突")
			}
			found = path.Join("albums", e.Name())
		}
	}
	if found == "" {
		return "", os.ErrNotExist
	}
	return found, nil
}
func (l LocalReplica) EnsureAlbum(ctx context.Context, a ReplicaAlbum) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.ID <= 0 {
		return "", errors.New("相册编号无效")
	}
	root, err := l.root()
	if err != nil {
		return "", err
	}
	defer root.Close()
	prefix := ""
	if a.OwnerID != "" {
		prefix = path.Join("accounts", "account-"+hex.EncodeToString([]byte(a.OwnerID)))
		if err = root.MkdirAll(prefix, 0700); err != nil {
			return "", err
		}
		accountRoot, e := root.OpenRoot(prefix)
		if e != nil {
			return "", e
		}
		defer accountRoot.Close()
		root = accountRoot
	}
	if err = root.MkdirAll("albums", 0o700); err != nil {
		return "", err
	}
	next := path.Join("albums", strconv.FormatInt(a.ID, 10)+"--"+safeReplicaName(a.Name))
	previous, err := replicaAlbumFolder(root, a.ID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && previous != next {
		if err = root.Rename(previous, next); err != nil {
			return "", err
		}
	}
	if err = root.MkdirAll(next, 0o700); err != nil {
		return "", err
	}
	return path.Join(prefix, strconv.FormatInt(a.ID, 10)), nil
}
func replicaObjectPath(root *os.Root, ref string) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) == 5 && parts[0] == "accounts" && strings.HasPrefix(parts[1], "account-") {
		owner, e := hex.DecodeString(strings.TrimPrefix(parts[1], "account-"))
		if e != nil || len(owner) == 0 {
			return "", errors.New("副仓账号定位无效")
		}
		prefix := path.Join(parts[0], parts[1])
		accountRoot, e := root.OpenRoot(prefix)
		if e != nil {
			return "", e
		}
		defer accountRoot.Close()
		relative, e := replicaObjectPath(accountRoot, strings.Join(parts[2:], "/"))
		if e != nil {
			return "", e
		}
		return path.Join(prefix, relative), nil
	}
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" || parts[1] == "." || parts[2] == "." || strings.ContainsAny(ref, "\\:") || path.Clean(ref) != ref || strings.Contains(ref, "..") {
		return "", errors.New("副仓对象定位无效")
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return "", errors.New("副仓相册定位无效")
	}
	folder, err := replicaAlbumFolder(root, id)
	if err != nil {
		return "", err
	}
	return path.Join(folder, parts[1], parts[2]), nil
}
func (l LocalReplica) Put(ctx context.Context, album, fileID string, obj ReplicaObject, input io.Reader) (string, error) {
	root, err := l.root()
	if err != nil {
		return "", err
	}
	defer root.Close()
	ref := album + "/" + fileID + "/" + obj.Name
	key, err := replicaObjectPath(root, ref)
	if err != nil {
		return "", err
	}
	if err = root.MkdirAll(path.Dir(key), 0o700); err != nil {
		return "", err
	}
	tmp := key + "." + ids.New() + ".part"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer root.Remove(tmp)
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(replicaContextReader{ctx, input}, obj.Size+1))
	if copyErr == nil && (n != obj.Size || hex.EncodeToString(h.Sum(nil)) != obj.SHA256) {
		copyErr = errors.New("副仓写入大小或校验值不匹配")
	}
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	if err = root.Rename(tmp, key); err != nil {
		return "", err
	}
	return ref, nil
}
func (l LocalReplica) Prepare(ctx context.Context, album, fileID string, obj ReplicaObject) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := l.root()
	if err != nil {
		return "", err
	}
	defer root.Close()
	ref := album + "/" + fileID + "/" + obj.Name
	_, err = replicaObjectPath(root, ref)
	return ref, err
}
func (l LocalReplica) Open(ctx context.Context, ref string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := l.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	key, err := replicaObjectPath(root, ref)
	if err != nil {
		return nil, err
	}
	return root.Open(key)
}
func (l LocalReplica) Delete(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := l.root()
	if err != nil {
		return err
	}
	defer root.Close()
	key, err := replicaObjectPath(root, ref)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = root.Remove(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		// Remove only empty directories; never recurse over unknown contents.
		_ = root.Remove(path.Dir(key))
		_ = root.Remove(path.Dir(path.Dir(key)))
	}
	return err
}

type replicaContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r replicaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func ReplicaJSON(v any) (ReplicaObject, []byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ReplicaObject{}, nil, err
	}
	sum := sha256.Sum256(data)
	return ReplicaObject{Name: "manifest.json", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sum), MIME: "application/json"}, data, nil
}
