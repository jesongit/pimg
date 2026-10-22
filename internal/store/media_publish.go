package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/mediax"
	"pimg/internal/storage"
)

type presentation struct {
	Path, Ext, MIME, SHA256 string
	Size                    int64
}
type presentationObject struct {
	Backend, Key, MIME, SHA256, PayloadSHA256 string
	Size, PayloadSize                         int64
	Encrypted                                 bool
}

func (s *Service) buildPresentation(ctx context.Context, f *dbx.File, work string) (presentation, error) {
	inputs, err := s.mediaInputs(ctx, f, work)
	if err != nil {
		return presentation{}, err
	}
	if len(inputs) == 0 {
		return presentation{}, errors.New("媒体内容为空")
	}
	for index, input := range inputs {
		materialized, _, e := s.materializeMediaInput(ctx, f, input, work)
		if e != nil {
			return presentation{}, e
		}
		inputs[index] = materialized
	}
	path := inputs[0].path
	ext := inputs[0].part.Ext
	mime := inputs[0].part.MIME
	if f.Kind == "video" {
		if len(inputs) > 1 {
			var paths []string
			for _, input := range inputs {
				paths = append(paths, input.path)
			}
			path = filepath.Join(work, "joined.mkv")
			if err = mediax.JoinForView(ctx, paths, path); err != nil {
				return presentation{}, err
			}
		}
		output := filepath.Join(work, "view.mp4")
		info, e := mediax.InspectVideo(ctx, path)
		if e != nil {
			return presentation{}, e
		}
		mode := "transcode"
		if info.AllVideoCompatible && info.AllAudioCompatible && !info.HDR {
			mode = "remux"
		}
		if err = mediax.MakeCompatible(ctx, path, output, "video", mode); err != nil && mode == "remux" {
			err = mediax.MakeCompatible(ctx, path, output, "video", "transcode")
		}
		if err != nil {
			return presentation{}, err
		}
		path, ext, mime = output, "mp4", "video/mp4"
	} else if f.Ext == "heic" || f.Ext == "heif" {
		output := filepath.Join(work, "view.png")
		if err = mediax.MakeCompatible(ctx, path, output, "image", "image"); err != nil {
			return presentation{}, err
		}
		path, ext, mime = output, "png", "image/png"
	}
	size, sum, err := hashMediaPath(ctx, path)
	return presentation{Path: path, Ext: ext, MIME: mime, Size: size, SHA256: sum}, err
}

func (s *Service) presentationPayload(ctx context.Context, f *dbx.File, target string, p presentation, work string) (presentationObject, string, error) {
	policy, err := s.mediaPolicy(f, target)
	if err != nil {
		return presentationObject{}, "", err
	}
	object := presentationObject{Backend: target, MIME: p.MIME, Size: p.Size, SHA256: p.SHA256, Encrypted: policy.Encrypted, PayloadSize: p.Size, PayloadSHA256: p.SHA256}
	if !object.Encrypted {
		return object, p.Path, nil
	}
	input, err := os.Open(p.Path)
	if err != nil {
		return object, "", err
	}
	defer input.Close()
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return object, "", err
	}
	reader, err := cryptox.EncryptReader(input, key)
	if err != nil {
		return object, "", err
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	out, err := os.CreateTemp(work, "presentation-*.bin")
	if err != nil {
		return object, "", err
	}
	h := sha256New()
	n, err := io.Copy(io.MultiWriter(out, h), contextReaderForCache{ctx, reader})
	err = errors.Join(err, out.Close())
	object.PayloadSize = n
	object.PayloadSHA256 = hexOf(h)
	return object, out.Name(), err
}

func (s *Service) presentationCachePath(f *dbx.File, sum string) string {
	return filepath.Join(s.originalCacheDir(), sha256Hex([]byte("presentation\x00"+f.ID+"\x00"+sum))+".bin")
}
func (s *Service) cachePresentation(ctx context.Context, f *dbx.File, p presentation) error {
	if err := os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		return err
	}
	input, err := os.Open(p.Path)
	if err != nil {
		return err
	}
	defer input.Close()
	out, err := os.CreateTemp(s.originalCacheDir(), "presentation-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_ = out.Chmod(0600)
	_, err = io.Copy(out, contextReaderForCache{ctx, input})
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), s.presentationCachePath(f, p.SHA256))
}

func (s *Service) runMediaPublish(ctx context.Context, f *dbx.File, force bool) error {
	if f.ArchiveState != "done" || f.PurgeState.String != "" {
		return errors.New("原件尚未就绪或正在删除")
	}
	if f.Published && !force {
		return nil
	}
	work, err := os.MkdirTemp(s.upDir, "publication-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	p, err := s.buildPresentation(ctx, f, work)
	if err != nil {
		return err
	}
	var failures []error
	for _, target := range s.hotCandidates("permanent", s.StoragePolicy().Permanent) {
		b, e := s.hot(target)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		object, payload, e := s.presentationPayload(ctx, f, target, p, work)
		if e != nil {
			return e
		}
		ext := p.Ext
		if object.Encrypted {
			ext = "bin"
		}
		object.Key = fmt.Sprintf("pimg/%s/view.%s", f.ID, ext)
		if _, e = s.DB.Exec(`INSERT INTO media_publications(file_id,backend,object_key,size,sha256,mime,encrypted,payload_size,payload_sha256) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(file_id,backend,object_key) DO UPDATE SET size=excluded.size,sha256=excluded.sha256,mime=excluded.mime,encrypted=excluded.encrypted,payload_size=excluded.payload_size,payload_sha256=excluded.payload_sha256,ready=0`, f.ID, target, object.Key, p.Size, p.SHA256, p.MIME, object.Encrypted, object.PayloadSize, object.PayloadSHA256); e != nil {
			return e
		}
		input, e := os.Open(payload)
		if e != nil {
			return e
		}
		started := time.Now()
		contentType := p.MIME
		if object.Encrypted {
			contentType = "application/octet-stream"
		}
		e = b.PutObject(ctx, storage.Permanent, object.Key, input, object.PayloadSize, contentType, "private, no-store")
		input.Close()
		if e == nil {
			var size int64
			size, e = b.HeadSize(ctx, storage.Permanent, object.Key)
			if e == nil && size != object.PayloadSize {
				e = errors.New("发布副本长度校验失败")
			}
		}
		if e == nil {
			e = s.verifyPresentationObject(ctx, f, object, storage.Permanent)
		}
		s.recordStorageResult(target, object.PayloadSize, started, e)
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", target, e))
			continue
		}
		if _, e = s.DB.Exec(`UPDATE media_publications SET ready=1 WHERE file_id=? AND backend=? AND object_key=?`, f.ID, target, object.Key); e != nil {
			return e
		}
		if _, e = s.DB.Exec(`UPDATE files SET hot_backend=? WHERE id=?`, target, f.ID); e != nil {
			return e
		}
		if e = s.DB.SetPublished(f.ID, true, object.Key, ""); e != nil {
			return e
		}
		if e = s.cachePresentation(ctx, f, p); e != nil {
			s.Log.Warn("展示缓存写入失败，存储副本已就绪", "file", f.ID, "err", e)
		}
		return nil
	}
	err = errors.Join(failures...)
	if err == nil {
		err = errors.New("没有可用于发布的存储")
	}
	_ = s.DB.SetPublishError(f.ID, err.Error())
	return err
}

func (s *Service) runMediaTemporary(ctx context.Context, f *dbx.File) error {
	work, err := os.MkdirTemp(s.upDir, "temporary-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	p, err := s.buildPresentation(ctx, f, work)
	if err != nil {
		return err
	}
	expires := time.Now().Unix() + int64(s.RetentionHours())*3600
	var failures []error
	for _, target := range s.hotCandidates("temporary", s.StoragePolicy().Temporary) {
		b, e := s.hot(target)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		object, payload, e := s.presentationPayload(ctx, f, target, p, work)
		if e != nil {
			return e
		}
		ext := p.Ext
		if object.Encrypted {
			ext = "bin"
		}
		object.Key = fmt.Sprintf("tmp/%s/%s.%s", f.ID, ids.New(), ext)
		if s.publicR2Temporary(target) {
			object.Key = fmt.Sprintf("tmp/v1/%013d/%s/%s.%s", expires, f.ID, ids.New(), ext)
		}
		obj := &dbx.TempObject{ID: ids.New(), FileID: f.ID, Kind: "original", Backend: target, ObjectKey: object.Key, Size: sql.NullInt64{Int64: object.PayloadSize, Valid: true}, ExpiresAt: expires}
		if e = s.DB.InsertTempObject(obj); e != nil {
			return e
		}
		if _, e = s.DB.Exec(`INSERT INTO media_temporary(object_id,mime,size,sha256,encrypted,payload_size,payload_sha256) VALUES(?,?,?,?,?,?,?)`, obj.ID, p.MIME, p.Size, p.SHA256, object.Encrypted, object.PayloadSize, object.PayloadSHA256); e != nil {
			return e
		}
		input, e := os.Open(payload)
		if e != nil {
			return e
		}
		started := time.Now()
		contentType := p.MIME
		if object.Encrypted {
			contentType = "application/octet-stream"
		}
		e = b.PutObject(ctx, storage.Temporary, object.Key, input, object.PayloadSize, contentType, "private, no-store")
		input.Close()
		if e == nil {
			var size int64
			size, e = b.HeadSize(ctx, storage.Temporary, object.Key)
			if e == nil && size != object.PayloadSize {
				e = errors.New("临时副本长度校验失败")
			}
		}
		if e == nil {
			e = s.verifyPresentationObject(ctx, f, object, storage.Temporary)
		}
		s.recordStorageResult(target, object.PayloadSize, started, e)
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", target, e))
			continue
		}
		if e = s.DB.MarkTempReady(obj.ID); e != nil {
			return e
		}
		if e = s.cachePresentation(ctx, f, p); e != nil {
			s.Log.Warn("展示缓存写入失败，临时副本已就绪", "file", f.ID, "err", e)
		}
		return nil
	}
	err = errors.Join(failures...)
	if err == nil {
		err = errors.New("没有可用于临时访问的存储")
	}
	return err
}

func (s *Service) mediaTemporarySignature(id string, expires int64) string {
	mac := hmac.New(sha256.New, s.Master.Raw())
	_, _ = fmt.Fprintf(mac, "pimg/temporary-gateway/v1\x00%s\x00%d", id, expires)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Service) mediaTemporaryURL(obj *dbx.TempObject) (string, int64, error) {
	if obj.DeletedAt.Valid || obj.ExpiresAt <= time.Now().Unix() {
		return "", 0, errors.New("临时副本已过期或已撤销")
	}
	expires := min(obj.ExpiresAt, time.Now().Add(time.Duration(s.PresignMinutes())*time.Minute).Unix())
	return fmt.Sprintf("%s/temporary/%s?expires=%d&sig=%s", s.PublicBaseURL(), url.PathEscape(obj.ID), expires, s.mediaTemporarySignature(obj.ID, expires)), expires, nil
}

func (s *Service) presentationFile(ctx context.Context, f *dbx.File, object presentationObject, scope string) (*os.File, error) {
	path := s.presentationCachePath(f, object.SHA256)
	if file, err := os.Open(path); err == nil {
		st, e := file.Stat()
		if e == nil && st.Size() == object.Size && time.Since(st.ModTime()) < time.Duration(s.OriginalCacheHours())*time.Hour {
			return file, nil
		}
		file.Close()
	}
	b, err := s.hot(object.Backend)
	if err != nil {
		return nil, err
	}
	reader, ok := b.(storage.HotReader)
	if !ok {
		return nil, errors.New("展示存储不支持读取降级")
	}
	startedOpen := time.Now()
	input, err := reader.OpenObject(ctx, scope, object.Key)
	if err != nil {
		s.recordStorageResult(object.Backend, 0, startedOpen, err)
		return nil, err
	}
	var source io.Reader = input
	if object.Encrypted {
		key, e := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
		if e != nil {
			input.Close()
			return nil, e
		}
		source, e = cryptox.DecryptReader(input, key)
		if e != nil {
			input.Close()
			return nil, e
		}
	}
	if err = os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		input.Close()
		return nil, err
	}
	out, err := os.CreateTemp(s.originalCacheDir(), "presentation-read-*.part")
	if err != nil {
		input.Close()
		return nil, err
	}
	defer os.Remove(out.Name())
	_ = out.Chmod(0600)
	started := time.Now()
	h := sha256New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReaderForCache{ctx, source}, object.Size+1))
	err = errors.Join(err, input.Close(), out.Close())
	if err == nil && (n != object.Size || hexOf(h) != object.SHA256) {
		err = errors.New("展示副本恢复校验失败")
	}
	s.recordStorageResult(object.Backend, n, started, err)
	if err != nil {
		return nil, err
	}
	if err = os.Rename(out.Name(), path); err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (s *Service) verifyPresentationObject(ctx context.Context, f *dbx.File, o presentationObject, scope string) error {
	b, err := s.hot(o.Backend)
	if err != nil {
		return err
	}
	reader, ok := b.(storage.HotReader)
	if !ok {
		return errors.New("展示存储不支持回读校验")
	}
	input, err := reader.OpenObject(ctx, scope, o.Key)
	if err != nil {
		return err
	}
	defer input.Close()
	var source io.Reader = input
	if o.Encrypted {
		key, e := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
		if e != nil {
			return e
		}
		source, e = cryptox.DecryptReader(input, key)
		if e != nil {
			return e
		}
	}
	h := sha256New()
	n, err := io.Copy(h, io.LimitReader(contextReaderForCache{ctx, source}, o.Size+1))
	if err != nil {
		return err
	}
	if n != o.Size || hexOf(h) != o.SHA256 {
		return errors.New("展示副本回读哈希或长度不匹配")
	}
	return nil
}

func (s *Service) publicationObject(f *dbx.File) (presentationObject, error) {
	var o presentationObject
	err := s.DB.QueryRow(`SELECT backend,object_key,size,sha256,mime,encrypted,payload_size,payload_sha256 FROM media_publications WHERE file_id=? AND ready=1 ORDER BY (backend=?) DESC`, f.ID, f.HotBackend).Scan(&o.Backend, &o.Key, &o.Size, &o.SHA256, &o.MIME, &o.Encrypted, &o.PayloadSize, &o.PayloadSHA256)
	return o, err
}

func (s *Service) ServeMediaPublication(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.GetVisibleFile(r.PathValue("id"))
	if err != nil || !f.Published || f.PurgeState.String != "" || f.MediaVersion != 2 {
		http.NotFound(w, r)
		return
	}
	object, err := s.publicationObject(f)
	var file *os.File
	if err == nil {
		file, err = s.presentationFile(r.Context(), f, object, storage.Permanent)
	}
	if err != nil {
		unlock, e := s.lockFile(r.Context(), f.ID)
		if e != nil {
			http.Error(w, "展示副本暂不可用", 503)
			return
		}
		fresh, e := s.mutableFile(f.ID)
		if e == nil && fresh.Published {
			e = s.runMediaPublish(r.Context(), fresh, true)
		} else {
			e = dbx.ErrNotFound
		}
		unlock()
		if e == nil {
			f, _ = s.DB.GetFile(f.ID)
			object, e = s.publicationObject(f)
			if e == nil {
				file, e = s.presentationFile(r.Context(), f, object, storage.Permanent)
			}
		}
		if e != nil {
			http.Error(w, "展示副本暂不可用，请稍后重试", 503)
			return
		}
	}
	defer file.Close()
	if fresh, e := s.DB.GetVisibleFile(f.ID); e != nil || !fresh.Published || fresh.PurgeState.String != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", object.MIME)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, f.Name, time.Time{}, file)
}

func (s *Service) ServeMediaTemporary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	given, e := hex.DecodeString(r.URL.Query().Get("sig"))
	expected, _ := hex.DecodeString(s.mediaTemporarySignature(id, expires))
	if err != nil || e != nil || expires <= time.Now().Unix() || !hmac.Equal(given, expected) {
		http.NotFound(w, r)
		return
	}
	var fileID string
	var objectExpires, deleted int64
	if err = s.DB.QueryRow(`SELECT file_id,expires_at,COALESCE(deleted_at,0) FROM temp_objects WHERE id=?`, id).Scan(&fileID, &objectExpires, &deleted); err != nil || deleted != 0 || expires > objectExpires {
		http.NotFound(w, r)
		return
	}
	f, err := s.DB.GetVisibleFile(fileID)
	if err != nil || f.PurgeState.String != "" || f.MediaVersion != 2 {
		http.NotFound(w, r)
		return
	}
	load := func() (presentationObject, error) {
		var o presentationObject
		err := s.DB.QueryRow(`SELECT t.backend,t.object_key,m.size,m.sha256,m.mime,m.encrypted,m.payload_size,m.payload_sha256 FROM temp_objects t JOIN media_temporary m ON m.object_id=t.id WHERE t.file_id=? AND t.ready_at IS NOT NULL AND t.deleted_at IS NULL AND t.expires_at>? ORDER BY t.ready_at DESC LIMIT 1`, f.ID, time.Now().Unix()).Scan(&o.Backend, &o.Key, &o.Size, &o.SHA256, &o.MIME, &o.Encrypted, &o.PayloadSize, &o.PayloadSHA256)
		return o, err
	}
	object, err := load()
	var file *os.File
	if err == nil {
		file, err = s.presentationFile(r.Context(), f, object, storage.Temporary)
	}
	if err != nil {
		unlock, e := s.lockFile(r.Context(), f.ID)
		if e != nil {
			http.Error(w, "临时副本暂不可用", 503)
			return
		}
		fresh, e := s.mutableFile(f.ID)
		if e == nil && fresh.PurgeState.String == "" {
			e = s.runMediaTemporary(r.Context(), fresh)
		}
		unlock()
		if e == nil {
			object, e = load()
			if e == nil {
				file, e = s.presentationFile(r.Context(), f, object, storage.Temporary)
			}
		}
		if e != nil {
			http.Error(w, "临时副本暂不可用", 503)
			return
		}
	}
	defer file.Close()
	if expires <= time.Now().Unix() {
		http.NotFound(w, r)
		return
	}
	if fresh, e := s.DB.GetVisibleFile(f.ID); e != nil || fresh.PurgeState.String != "" {
		http.NotFound(w, r)
		return
	}
	if e := s.DB.QueryRow(`SELECT COALESCE(deleted_at,0) FROM temp_objects WHERE id=?`, id).Scan(&deleted); e != nil || deleted != 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", object.MIME)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, f.Name, time.Time{}, file)
}

func (s *Service) removeMediaPublications(ctx context.Context, f *dbx.File) error {
	rows, err := s.DB.Query(`SELECT backend,object_key FROM media_publications WHERE file_id=?`, f.ID)
	if err != nil {
		return err
	}
	type locator struct{ backend, key string }
	var all []locator
	for rows.Next() {
		var l locator
		if err = rows.Scan(&l.backend, &l.key); err != nil {
			rows.Close()
			return err
		}
		all = append(all, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, l := range all {
		b, e := s.hot(l.backend)
		if e == nil {
			e = b.Delete(ctx, storage.Permanent, l.key)
		}
		if e != nil {
			failures = append(failures, e)
			continue
		}
		if _, e = s.DB.Exec(`DELETE FROM media_publications WHERE file_id=? AND backend=? AND object_key=?`, f.ID, l.backend, l.key); e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
