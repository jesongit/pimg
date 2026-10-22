package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/r2x"
	"pimg/internal/tguser"
	"strconv"
	"strings"
	"time"

	"github.com/aws/smithy-go"
)

type SourceEntry struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Version string `json:"version"`
	Name    string `json:"name"`
	MIME    string `json:"mime"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

var ErrSourceChanged = errors.New("来源已变化或不存在，等待重新扫描")

type SourcePage struct {
	Entries []SourceEntry
	Cursor  string
}

// SourceDriver is intentionally optional: archive-only adapters need not list or delete source files.
type SourceDriver interface {
	List(context.Context, string, string, bool) (SourcePage, error)
	Open(context.Context, SourceEntry) (io.ReadCloser, error)
	Delete(context.Context, SourceEntry) error
}

// Some providers cannot atomically remove the scanned version. Keep these
// unmanaged sources intact after import and expose an explicit cleanup receipt.
type ManualSourceCleanup interface{ ManualCleanupReason() string }
type localSource struct {
	dir  string
	pace func(context.Context, io.Reader) io.Reader
}

func localVersion(v fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", v.Size(), v.ModTime().UnixNano())
}
func (l *localSource) List(ctx context.Context, source, cursor string, recursive bool) (SourcePage, error) {
	root, e := os.OpenRoot(l.dir)
	if e != nil {
		return SourcePage{}, e
	}
	defer root.Close()
	source = strings.Trim(source, "/")
	if source == "" {
		source = "."
	}
	if source != "." && !fs.ValidPath(source) {
		return SourcePage{}, errors.New("源目录必须为存储根目录内的相对路径")
	}
	p := SourcePage{}
	e = fs.WalkDir(root.FS(), source, func(k string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if k != source && (!recursive || d.Name() == "pimg-data" || d.Name() == "archives" || d.Name() == "display" || d.Name() == ".pimg" || d.Name() == "albums" || d.Name() == "temp" || d.Name() == "permanent" || d.Name() == "pimg" || d.Name() == "thumbs") {
				return fs.SkipDir
			}
			return nil
		}
		if k <= cursor {
			return nil
		}
		if strings.Contains(d.Name(), ".pimg-") {
			return nil
		}
		v, err := d.Info()
		if err != nil {
			return err
		}
		if !v.Mode().IsRegular() {
			return nil
		}
		p.Entries = append(p.Entries, SourceEntry{ID: k, Key: k, Version: localVersion(v), Name: d.Name(), Size: v.Size()})
		if len(p.Entries) >= 50 {
			p.Cursor = k
			return fs.SkipAll
		}
		return nil
	})
	return p, e
}
func (l *localSource) Open(ctx context.Context, v SourceEntry) (io.ReadCloser, error) {
	root, e := os.OpenRoot(l.dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.Open(v.Key)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %v", ErrSourceChanged, e)
		}
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	if localVersion(st) != v.Version {
		f.Close()
		return nil, ErrSourceChanged
	}
	return f, nil
}
func (l *localSource) Delete(ctx context.Context, v SourceEntry) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	root, e := os.OpenRoot(l.dir)
	if e != nil {
		return e
	}
	defer root.Close()
	claim := v.Key + ".pimg-organizing"
	if claimed, err := root.Stat(claim); err == nil {
		if localVersion(claimed) != v.Version {
			return errors.New("整理占位内容已变化，保留文件")
		}
		return l.removeClaim(ctx, root, v, claim)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	st, e := root.Stat(v.Key)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if localVersion(st) != v.Version {
		return errors.New("源文件已变化，保留原文件")
	}

	// Claim the exact directory entry before deleting it. Recheck after rename so
	// a concurrent replacement cannot be mistaken for the archived version.

	if _, e = root.Lstat(claim); e == nil {
		return errors.New("源文件整理占位已存在，请检查")
	}
	if e = root.Rename(v.Key, claim); e != nil {
		return e
	}
	st, e = root.Stat(claim)
	if e != nil || localVersion(st) != v.Version {
		restoreSourceClaim(root, claim, v.Key)
		return errors.New("整理时源文件发生变化，已保留")
	}
	return l.removeClaim(ctx, root, v, claim)
}

// Renaming claims a directory entry, but size/mtime alone do not identify its
// bytes: synchronization tools may preserve timestamps when replacing content.
func (l *localSource) removeClaim(ctx context.Context, root *os.Root, v SourceEntry, claim string) error {
	file, err := root.Open(claim)
	if err != nil {
		return err
	}
	st, err := file.Stat()
	if err == nil && (!st.Mode().IsRegular() || localVersion(st) != v.Version || v.SHA256 == "") {
		err = errors.New("源文件版本或内容校验信息不完整")
	}
	if err == nil {
		var input io.Reader = contextReaderForCache{ctx, file}
		if l.pace != nil {
			input = l.pace(ctx, input)
		}
		h := sha256New()
		n, readErr := io.Copy(h, io.LimitReader(input, v.Size+1))
		err = readErr
		if err == nil && (n != v.Size || hexOf(h) != v.SHA256) {
			err = errors.New("源文件内容已变化，保留源文件")
		}
		if after, e := file.Stat(); err == nil {
			if e != nil {
				err = e
			} else if localVersion(after) != v.Version {
				err = ErrSourceChanged
			}
		}
	}
	file.Close()
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		restoreSourceClaim(root, claim, v.Key)
		return err
	}
	return root.Remove(claim)
}

func restoreSourceClaim(root *os.Root, claim, original string) {
	// Link fails atomically when a new original has appeared. A failed check
	// must never restore with an overwriting rename and destroy that new file.
	if root.Link(claim, original) == nil {
		_ = root.Remove(claim)
	}
}

type r2Source struct {
	client         *r2x.Client
	bucket, prefix string
}

func (r *r2Source) List(ctx context.Context, source, cursor string, recursive bool) (SourcePage, error) {
	pfx := strings.Trim(source, "/")
	if pfx != "" {
		pfx += "/"
	}
	if r.prefix != "" {
		pfx = r.prefix + "/" + pfx
	}
	p, e := r.client.List(ctx, r.bucket, pfx, cursor, 50)
	if e != nil {
		return SourcePage{}, e
	}
	out := SourcePage{Cursor: p.Next}
	for _, v := range p.Objects {
		relative := strings.TrimPrefix(v.Key, pfx)
		if relative == "" || (!recursive && strings.Contains(relative, "/")) {
			continue
		}
		owned := strings.TrimPrefix(v.Key, strings.Trim(r.prefix+"/", "/"))
		owned = strings.TrimPrefix(owned, "/")
		if strings.HasPrefix(owned, "archives/") || strings.HasPrefix(owned, "permanent/") || strings.HasPrefix(owned, "temp/") || strings.HasPrefix(owned, "tmp/") || strings.HasPrefix(owned, "pimg/") || strings.HasPrefix(owned, "thumbs/") {
			continue
		}
		out.Entries = append(out.Entries, SourceEntry{ID: v.Key, Key: v.Key, Name: path.Base(v.Key), Size: v.Size, Version: fmt.Sprintf("%s:%d", v.ETag, v.Size)})
	}
	return out, nil
}
func (r *r2Source) Open(ctx context.Context, v SourceEntry) (io.ReadCloser, error) {
	version, e := r.client.ObjectVersion(ctx, r.bucket, v.Key)
	if e != nil {
		var api smithy.APIError
		if errors.As(e, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
			return nil, ErrSourceChanged
		}
		return nil, e
	}
	if version != v.Version {
		return nil, ErrSourceChanged
	}
	return r.client.Get(ctx, r.bucket, v.Key)
}
func (r *r2Source) Delete(ctx context.Context, v SourceEntry) error {
	return errors.New(r.ManualCleanupReason())
}
func (r *r2Source) ManualCleanupReason() string {
	return "R2 不支持按版本原子删除；归档已完成，源文件已保留，待手动清理"
}

type telegramSource struct{ s *Service }

func (t *telegramSource) List(ctx context.Context, source, cursor string, recursive bool) (SourcePage, error) {
	topic, e := strconv.Atoi(source)
	if e != nil || topic <= 0 {
		return SourcePage{}, errors.New("TG 源需要填写话题 ID")
	}
	if e = t.s.TGReady(); e != nil {
		return SourcePage{}, e
	}
	offset, _ := strconv.Atoi(cursor)
	space, e := t.s.EnsureTelegramSpace(ctx, dbx.ContextOwner(ctx))
	if e != nil {
		return SourcePage{}, e
	}
	p, e := t.s.UserTG.History(ctx, space.ChatID, topic, time.Time{}, time.Time{}, offset, 50)
	if e != nil {
		return SourcePage{}, e
	}
	out := SourcePage{}
	if !p.Done {
		out.Cursor = strconv.Itoa(p.NextOffset)
	}
	for _, m := range p.Messages {
		var caption struct {
			App string `json:"app"`
		}
		_ = json.Unmarshal([]byte(m.Caption), &caption)
		if caption.App == "pimg" || caption.App == "pimg-part2" {
			continue
		}

		if m.IsService || m.Reference.Size <= 0 || strings.HasPrefix(m.Caption, "pimg") || strings.HasPrefix(m.Caption, "pimg-part") {
			continue
		}
		ref, _ := json.Marshal(m.Reference)
		out.Entries = append(out.Entries, SourceEntry{ID: fmt.Sprintf("%d:%d:%d", m.Reference.AccountID, m.Reference.ChatID, m.Reference.MessageID), Key: string(ref), Name: m.Name, MIME: m.MIME, Size: m.Reference.Size, Version: fmt.Sprintf("%d:%d", m.Reference.DocumentID, m.Reference.Size)})
	}
	return out, nil
}
func (t *telegramSource) Open(ctx context.Context, v SourceEntry) (io.ReadCloser, error) {
	var ref tguser.Reference
	if e := json.Unmarshal([]byte(v.Key), &ref); e != nil {
		return nil, e
	}
	if st, ok := t.s.UserTG.(tguser.StreamTransport); ok {
		return st.OpenStream(ctx, ref)
	}
	b, e := t.s.UserTG.Download(ctx, ref)
	if e != nil {
		return nil, e
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (t *telegramSource) Delete(ctx context.Context, v SourceEntry) error {
	var ref tguser.Reference
	if e := json.Unmarshal([]byte(v.Key), &ref); e != nil {
		return e
	}
	p, e := t.s.UserTG.History(ctx, ref.ChatID, ref.TopicID, time.Time{}, time.Time{}, ref.MessageID+1, 1)
	if e != nil {
		return e
	}
	for _, m := range p.Messages {
		if m.Reference.MessageID == ref.MessageID {
			if fmt.Sprintf("%d:%d", m.Reference.DocumentID, m.Reference.Size) != v.Version {
				return errors.New("TG 源消息已变化，保留消息")
			}
			return t.s.UserTG.Delete(ctx, m.Reference)
		}
	}
	return nil
}
func sourceMedia(v SourceEntry) bool {
	// Keep source scans aligned with upload/media recognition instead of
	// maintaining another extension list that can silently skip new formats.
	info := mediax.Detect(filepath.Ext(v.Name), "")
	return info.Kind == "image" || info.Kind == "video"
}
