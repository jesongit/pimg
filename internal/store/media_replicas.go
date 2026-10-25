package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/storage"
)

type mediaUploadContext struct{}

type mediaArchiveUpload struct {
	File      *dbx.File
	Thumbnail []byte
	Version   string
}

func (s *Service) mediaPolicy(f *dbx.File, instance string) (dbx.MediaReplica, error) {
	i, err := s.instance(instance)
	if err != nil {
		return dbx.MediaReplica{}, err
	}
	mb := i.config.VideoPartMB
	if mb == 0 {
		mb = 256
	}
	return dbx.MediaReplica{FileID: f.ID, InstanceID: instance, Encrypted: f.Storage != "direct" && i.config.Encrypt, Split: i.config.SplitVideo, PartMB: mb}, nil
}

func (s *Service) mediaReplica(f *dbx.File, instance string) (dbx.MediaReplica, error) {
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return dbx.MediaReplica{}, err
	}
	for _, r := range all {
		if r.InstanceID == instance {
			return r, nil
		}
	}
	r, err := s.mediaPolicy(f, instance)
	if err != nil {
		return r, err
	}
	err = s.DB.EnsureMediaReplica(r)
	return r, err
}

func (s *Service) mediaLimit(f *dbx.File, r dbx.MediaReplica) int64 {
	limit := int64(math.MaxInt64 / 2)
	if s.telegramInstance(r.InstanceID) {
		limit = s.telegramUploadLimit()
	}
	if r.Split && f.Kind == "video" && int64(r.PartMB)<<20 < limit {
		limit = int64(r.PartMB) << 20
	}
	if r.Encrypted && limit < math.MaxInt64/4 {
		return videoPlainBudget(f, limit)
	}
	return limit
}

func mediaPartFile(f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) *dbx.File {
	out := *f
	out.Size = p.Size
	out.SHA256 = p.SHA256
	out.Ext = p.Ext
	out.Mime = p.MIME
	out.Duration = p.Duration
	out.Naming.PartIndex = p.Index + 1
	out.Naming.PartCount = p.Count
	if r.Encrypted {
		out.Storage = "tg"
	} else {
		out.Storage = "direct"
	}
	return &out
}

func mediaObjectKey(f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) string {
	ext := p.Ext
	if r.Encrypted {
		ext = "png"
	}
	return fmt.Sprintf("accounts/%s/albums/%d/%s/media-%05d.%s", dbx.OwnerDirectory(f.OwnerID), f.AlbumID.Int64, f.ID, p.Index, ext)
}

func hashMediaPath(ctx context.Context, path string) (int64, string, error) {
	input, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer input.Close()
	h := sha256New()
	n, err := io.Copy(h, contextReaderForCache{ctx, input})
	return n, hexOf(h), err
}

func (s *Service) openMediaPart(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) (io.ReadCloser, error) {
	if !p.Verified || p.Reference == "" {
		return nil, errors.New("片段尚未保存完成")
	}
	b, err := s.archiveBackend(r.InstanceID)
	if err != nil {
		return nil, err
	}
	var ref storage.Reference
	if err = json.Unmarshal([]byte(p.Reference), &ref); err != nil {
		return nil, err
	}
	var input io.ReadCloser
	if s.telegramInstance(r.InstanceID) && s.UserTG != nil {
		// Read the complete wire once. Replica policy, rather than a historical
		// locator's Carrier hint, determines whether a PNG must be decoded.
		obj, e := s.telegramObject(ref.Key)
		if e != nil {
			return nil, e
		}
		input, err = s.openTelegramWire(ctx, obj)
	} else {
		input, err = b.Open(ctx, ref)
	}
	if err != nil {
		return nil, err
	}
	if !r.Encrypted {
		return input, nil
	}
	{
		h, e := carrier.OpenPart(input)
		if e != nil {
			input.Close()
			return nil, e
		}
		if h.FileID != f.ID || h.Index != p.Index || h.Size != p.PayloadSize || h.SHA256 != p.PayloadSHA256 {
			input.Close()
			return nil, errors.New("本地或远端加密载体与片段索引不一致")
		}
	}
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		input.Close()
		return nil, err
	}
	plain, err := cryptox.DecryptReader(input, key)
	if err != nil {
		input.Close()
		return nil, err
	}
	return &readCloser{r: plain, closer: input}, nil
}

func (s *Service) verifyMediaPart(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) (retErr error) {
	span := s.DB.Performance().Start(ctx, "stage", "archive_verify")
	defer func() {
		var size int64
		if retErr == nil {
			size = p.Size
		}
		span.FinishResult(retErr, 0, size)
	}()
	p.Verified = true
	input, err := s.openMediaPart(ctx, f, r, p)
	if err != nil {
		return err
	}
	h := sha256New()
	var reader io.Reader = contextReaderForCache{ctx, input}
	if sync, _ := ctx.Value(mediaSyncContext{}).(bool); sync {
		reader = s.syncVerifyReader(ctx, reader, r.InstanceID)
	}
	n, err := io.Copy(h, io.LimitReader(reader, p.Size+1))
	err = errors.Join(err, input.Close())
	if err == nil && (n != p.Size || hexOf(h) != p.SHA256) {
		err = errors.New("存储片段恢复后与源内容不一致")
	}
	return err
}

// Local objects can change outside PIMG. Only reuse a receipt while the stored
// file still has the size and modification time observed during verification.
func (s *Service) localMediaPartStamp(ctx context.Context, r dbx.MediaReplica, p dbx.MediaPart) (int64, error) {
	if !s.localSyncInstance(r.InstanceID) {
		return 0, nil
	}
	b, err := s.archiveBackend(r.InstanceID)
	if err != nil {
		return 0, err
	}
	var ref storage.Reference
	if err = json.Unmarshal([]byte(p.Reference), &ref); err != nil {
		return 0, err
	}
	input, err := b.Open(ctx, ref)
	if err != nil {
		return 0, err
	}
	defer input.Close()
	file, ok := input.(*os.File)
	if !ok {
		return 0, nil // Unknown wrappers must still perform full verification.
	}
	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if stat.Size() != p.StoredSize {
		return 0, errors.New("本地存储片段大小已改变")
	}
	return stat.ModTime().UnixNano(), nil
}

func (s *Service) verifyAndStampMediaPart(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) (dbx.MediaPart, error) {
	before, err := s.localMediaPartStamp(ctx, r, p)
	if err != nil {
		return p, err
	}
	if err = s.verifyMediaPart(ctx, f, r, p); err != nil {
		return p, err
	}
	after, err := s.localMediaPartStamp(ctx, r, p)
	if err != nil {
		return p, err
	}
	if before != after {
		return p, errors.New("本地存储片段在校验期间发生变化")
	}
	p.LocalModNS = after
	return p, nil
}

func (s *Service) reuseVerifiedMediaPart(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var ref storage.Reference
	if !p.Verified || json.Unmarshal([]byte(p.Reference), &ref) != nil || ref.Key == "" {
		return errors.New("已校验片段的存储位置无效")
	}
	if !s.localSyncInstance(r.InstanceID) {
		// The durable receipt belongs to this replica's immutable policy and
		// matching source hash/layout. A retry must not download it again.
		return nil
	}
	stamp, err := s.localMediaPartStamp(ctx, r, p)
	if err != nil {
		return err
	}
	if stamp != 0 && stamp == p.LocalModNS {
		return nil
	}
	// Older releases did not stamp encrypted local parts. Verify once before
	// persisting a stamp; changed files also pass through the full hash check.
	p, err = s.verifyAndStampMediaPart(ctx, f, r, p)
	if err != nil {
		return err
	}
	return s.DB.SaveMediaPart(f.ID, r.InstanceID, p)
}

func (s *Service) writeMediaPart(ctx context.Context, f *dbx.File, r dbx.MediaReplica, p dbx.MediaPart, path string) (dbx.MediaPart, error) {
	b, err := s.archiveBackend(r.InstanceID)
	if err != nil {
		return p, err
	}
	repair := false
	if s.telegramInstance(r.InstanceID) && s.UserTG != nil {
		all, e := s.DB.MediaReplicas(f.ID)
		if e != nil {
			return p, e
		}
		for _, previous := range all {
			if previous.InstanceID != r.InstanceID {
				continue
			}
			for _, old := range previous.Parts {
				if old.Index != p.Index || old.Verified || old.Reference == "" || old.Size != p.Size || old.SHA256 != p.SHA256 {
					continue
				}
				if e = s.verifyMediaPart(ctx, f, r, old); e == nil {
					old.Verified = true
					return old, s.DB.SaveMediaPart(f.ID, r.InstanceID, old)
				}
				// A confirmed format mismatch must not reuse the same Telegram
				// random ID and receive that defective document again. Interrupted
				// reads keep the existing locator and retry without another upload.
				if !errors.Is(e, carrier.ErrNotPNG) {
					return p, e
				}
				if _, e = s.DB.Exec(`INSERT OR IGNORE INTO archive_garbage(file_id,instance_id,reference) VALUES(?,?,?)`, f.ID, r.InstanceID, old.Reference); e != nil {
					return p, e
				}
				repair = true
			}
		}
	}
	input, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer input.Close()
	p.Verified = false
	var source io.Reader = input
	p.PayloadSize, p.PayloadSHA256 = p.Size, p.SHA256
	var encrypted *os.File
	var preview []byte
	var legacyCover bool
	upload := mediaArchiveUpload{}
	if r.Encrypted {
		var e error
		preview, legacyCover, e = s.prepareMediaCover(ctx, f, r, p, path)
		if e != nil {
			return p, e
		}
		encrypted, p.PayloadSize, p.PayloadSHA256, e = s.prepareMediaCipher(ctx, f, r, p, input)
		if e != nil {
			return p, e
		}
		defer encrypted.Close()
		source = encrypted
	}
	partFile := mediaPartFile(f, r, p)
	upload.File = partFile
	size := p.PayloadSize
	if r.Encrypted {
		header := telegramPartHeader(partFile, "data", p.Index)
		prefix, e := carrier.PartPrefix(preview, header, p.PayloadSize, p.PayloadSHA256)
		if e != nil {
			return p, e
		}
		source = io.MultiReader(bytes.NewReader(prefix), source)
		size += int64(len(prefix))
		upload.Thumbnail, e = mediaCoverThumbnail(preview)
		if e != nil {
			return p, e
		}
		if !legacyCover {
			upload.Version = sha256Hex(prefix)
		}
	}
	if repair {
		upload.Version = sha256Hex([]byte("verified-wire-repair/v1/" + p.PayloadSHA256 + "/" + upload.Version))
	}
	if s.telegramInstance(r.InstanceID) && size > s.telegramUploadLimit() {
		return p, errors.New("封装后的片段超过 TG 单文件上限")
	}
	p.StoredSize = size
	key := mediaObjectKey(f, r, p)
	// Persist a deterministic cleanup locator before writing. TG uses a stable
	// random ID for the same key, so retries cannot publish duplicate messages.
	p.Reference = referenceJSON(storage.Reference{Key: key})
	if !s.telegramInstance(r.InstanceID) {
		if err = s.DB.SaveMediaPart(f.ID, r.InstanceID, p); err != nil {
			return p, err
		}
	}
	started := time.Now()
	if sync, _ := ctx.Value(mediaSyncContext{}).(bool); sync {
		source = s.syncCopyReader(ctx, source, r.InstanceID)
	}
	ref, err := s.putArchiveReader(context.WithValue(ctx, mediaUploadContext{}, upload), b, key, source, size)
	s.recordStorageResult(r.InstanceID, size, started, err)
	if err != nil {
		return p, err
	}
	p.Reference = referenceJSON(ref)
	if err = s.DB.SaveMediaPart(f.ID, r.InstanceID, p); err != nil {
		return p, err
	}
	if p, err = s.verifyAndStampMediaPart(ctx, f, r, p); err != nil {
		return p, err
	}
	p.Verified = true
	if err = s.DB.SaveMediaPart(f.ID, r.InstanceID, p); err != nil {
		return p, err
	}
	if encrypted != nil {
		encrypted.Close()
		_ = os.Remove(encrypted.Name())
		_ = os.Remove(encrypted.Name() + ".cover.png")
	}
	return p, nil
}

type mediaInput struct {
	path   string
	part   dbx.MediaPart
	remote bool
}

type mediaSyncContext struct{}

func (s *Service) mediaInputs(ctx context.Context, f *dbx.File, work string) ([]mediaInput, error) {
	if st, err := os.Stat(s.plainPath(f.ID)); err == nil && st.Size() == f.Size {
		n, sum, err := hashMediaPath(ctx, s.plainPath(f.ID))
		if err == nil && n == f.Size && sum == f.SHA256 {
			return []mediaInput{{path: s.plainPath(f.ID), part: dbx.MediaPart{Count: 1, Ext: f.Ext, MIME: f.Mime, Size: f.Size, SHA256: f.SHA256, Duration: f.Duration, Original: true}}}, nil
		}
	}
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return nil, err
	}
	for pass := 0; pass < 2; pass++ {
		for _, r := range replicas {
			if r.State != "ready" || len(r.Parts) == 0 {
				continue
			}
			whole := len(r.Parts) == 1 && r.Parts[0].Original
			if whole && (r.Parts[0].Size != f.Size || r.Parts[0].SHA256 != f.SHA256) {
				continue
			}
			if (pass == 0) != whole {
				continue
			}
			var inputs []mediaInput
			for _, p := range r.Parts {
				input := mediaInput{part: p, remote: true}
				if !r.Encrypted {
					if raw, e := s.openMediaPart(ctx, f, r, p); e == nil {
						if file, ok := raw.(*os.File); ok {
							if n, sum, e := hashMediaPath(ctx, file.Name()); e == nil && n == p.Size && sum == p.SHA256 {
								input.path = file.Name()
								input.remote = false
							}
						}
						raw.Close()
					}
				}
				inputs = append(inputs, input)
			}
			return inputs, nil
		}
	}
	return nil, errors.New("没有完整可读取的存储副本，原件工作文件也不可用")
}

func (s *Service) materializeMediaInput(ctx context.Context, f *dbx.File, input mediaInput, work string) (mediaInput, func(), error) {
	if !input.remote {
		return input, func() {}, nil
	}
	if sync, _ := ctx.Value(mediaSyncContext{}).(bool); sync && input.part.Size > int64(s.StoragePolicy().SyncCacheMB)<<20 {
		return input, func() {}, errors.New("单个来源文件超过同步工作空间上限，请增加工作空间或先查看原件保存到本地")
	}
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return input, func() {}, err
	}
	var failures []error
	for _, r := range all {
		for _, p := range r.Parts {
			if !p.Verified || p.Size != input.part.Size || p.SHA256 != input.part.SHA256 {
				continue
			}
			raw, e := s.openMediaPart(ctx, f, r, p)
			if e != nil {
				failures = append(failures, e)
				continue
			}
			out, e := os.CreateTemp(work, "media-source-*."+p.Ext)
			if e != nil {
				raw.Close()
				return input, func() {}, e
			}
			path := out.Name()
			h := sha256New()
			var reader io.Reader = raw
			if sync, _ := ctx.Value(mediaSyncContext{}).(bool); sync {
				reader = s.syncCopyReader(ctx, reader, r.InstanceID)
			}
			started := time.Now()
			n, e := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReaderForCache{ctx, reader}, p.Size+1))
			e = errors.Join(e, raw.Close(), out.Close())
			if e == nil && (n != p.Size || hexOf(h) != p.SHA256) {
				e = errors.New("来源副本哈希或长度不匹配")
			}
			s.recordStorageResult(r.InstanceID, n, started, e)
			if e != nil {
				_ = os.Remove(path)
				failures = append(failures, e)
				continue
			}
			input.path = path
			input.remote = false
			return input, func() { _ = os.Remove(path) }, nil
		}
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("没有可读取的相同来源副本"))
	}
	return input, func() {}, errors.Join(failures...)
}

func (s *Service) writeMediaReplica(ctx context.Context, f *dbx.File, target string, inputs []mediaInput, work string) (retErr error) {
	r, err := s.mediaReplica(f, target)
	if err != nil {
		return err
	}
	if r.State == "ready" {
		return nil
	}
	span := s.DB.Performance().Start(ctx, "stage", "archive_write")
	defer func() {
		size := int64(0)
		if retErr == nil {
			size = f.Size
		}
		span.FinishResult(retErr, 0, size)
	}()
	defer func() {
		if retErr != nil {
			_ = s.DB.SetMediaReplicaState(f.ID, target, "failed", retErr.Error())
		}
	}()
	limit := s.mediaLimit(f, r)
	if limit <= 0 {
		return errors.New("分片容量不足以容纳加密封装")
	}
	remote, needsSplit := false, false
	for _, input := range inputs {
		remote = remote || input.remote
		needsSplit = needsSplit || input.part.Size > limit
	}
	if remote && !needsSplit {
		if err = mediaLayoutMatches(r, inputs); err != nil {
			return err
		}
		// Preserve boundaries and process one remote segment at a time.
		for index, input := range inputs {
			p := input.part
			p.Index = index
			p.Count = len(inputs)
			kept := false
			for _, old := range r.Parts {
				if old.Index == index && old.Verified && old.SHA256 == p.SHA256 && old.Size == p.Size && old.Count == p.Count {
					if err = s.reuseVerifiedMediaPart(ctx, f, r, old); err == nil {
						kept = true
					} else if ctx.Err() != nil {
						return ctx.Err()
					}
					break
				}
			}
			if kept {
				continue
			}
			materialized, cleanup, e := s.materializeMediaInput(ctx, f, input, work)
			if e != nil {
				return e
			}
			_, e = s.writeMediaPart(ctx, f, r, p, materialized.path)
			cleanup()
			if e != nil {
				return e
			}
		}
		return s.completeMediaReplica(ctx, f, target)
	}
	if remote {
		var total int64
		for _, input := range inputs {
			if input.remote {
				total += input.part.Size
			}
		}
		if sync, _ := ctx.Value(mediaSyncContext{}).(bool); sync && total > int64(s.StoragePolicy().SyncCacheMB)<<20 {
			return errors.New("重新切分所需来源超过同步工作空间上限；请增加上限或先保存本地原件")
		}
		for index, input := range inputs {
			materialized, cleanup, e := s.materializeMediaInput(ctx, f, input, work)
			if e != nil {
				return e
			}
			defer cleanup()
			inputs[index] = materialized
		}
	}
	var prepared []mediaInput
	for i, input := range inputs {
		paths := []string{input.path}
		if input.part.Size > limit {
			if f.Kind != "video" || !r.Split {
				return errors.New("文件超过该存储的单文件上限；请开启无损分片或改用其他存储")
			}
			paths, err = mediax.SplitVideo(ctx, input.path, input.part.Ext, filepath.Join(work, fmt.Sprintf("parts-%s-%d", target, i)), limit)
			if err != nil {
				return err
			}
		}
		start := input.part.Start
		for _, path := range paths {
			p := input.part
			if path != input.path {
				p.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
				if contentType := mime.TypeByExtension("." + p.Ext); contentType != "" {
					p.MIME = contentType
				}
				p.Size, p.SHA256, err = hashMediaPath(ctx, path)
				if err != nil {
					return err
				}
				info, e := mediax.InspectVideo(ctx, path)
				if e != nil {
					return e
				}
				p.Duration = info.Duration
				p.Original = false
			}
			p.Start = start
			start += p.Duration
			prepared = append(prepared, mediaInput{path: path, part: p})
		}
	}
	if err = mediaLayoutMatches(r, prepared); err != nil {
		return err
	}
	for i, input := range prepared {
		p := input.part
		p.Index = i
		p.Count = len(prepared)
		var kept *dbx.MediaPart
		for j := range r.Parts {
			old := &r.Parts[j]
			if old.Index == i && old.Verified && old.SHA256 == p.SHA256 && old.Size == p.Size && old.Count == p.Count {
				kept = old
				break
			}
		}
		if kept != nil {
			if err = s.reuseVerifiedMediaPart(ctx, f, r, *kept); err != nil {
				return err
			}
			continue
		}
		if _, err = s.writeMediaPart(ctx, f, r, p, input.path); err != nil {
			return err
		}
	}
	return s.completeMediaReplica(ctx, f, target)
}

func (s *Service) completeMediaReplica(ctx context.Context, f *dbx.File, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.DB.SetMediaReplicaState(f.ID, target, "ready", ""); err != nil {
		return err
	}
	return s.cleanupArchiveGarbage(ctx, f.ID)
}

func (s *Service) runMediaArchive(ctx context.Context, f *dbx.File, publish bool) error {
	if f.ArchiveState == "done" {
		return s.queueMediaSync(f)
	}
	if err := s.DB.SetArchiveState(f.ID, "running", ""); err != nil {
		return err
	}
	work, err := os.MkdirTemp(s.upDir, "media-archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	inputs, err := s.mediaInputs(ctx, f, work)
	if err == nil {
		var failures []error
		written := false
		for _, target := range s.rankedArchives() {
			i, e := s.instance(target)
			if e != nil || !i.row.Enabled {
				continue
			}
			if e = s.writeMediaReplica(ctx, f, target, inputs, work); e != nil {
				failures = append(failures, fmt.Errorf("%s: %w", target, e))
				continue
			}
			f.ColdBackend = target
			written = true
			_, err = s.DB.Exec(`UPDATE files SET cold_backend=? WHERE id=?`, target, f.ID)
			break
		}
		if !written {
			err = errors.Join(failures...)
			if err == nil {
				err = errors.New("没有可用的归档存储")
			}
		}
	}
	if err != nil {
		_ = s.setArchiveFailure(ctx, f.ID, err)
		return err
	}
	if err = s.archiveThumbs(ctx, f, s.plainPath(f.ID)); err != nil {
		s.Log.Warn("预览生成失败，原件已保存", "file", f.ID, "err", err)
	}
	if err = s.DB.SetArchiveState(f.ID, "done", ""); err != nil {
		return err
	}
	f.ArchiveState = "done"
	if err = s.queueMediaSync(f); err != nil {
		return err
	}
	if publish && f.Storage == "direct" {
		return s.runPublish(ctx, f.ID, true)
	}
	return nil
}

func (s *Service) queueMediaSync(f *dbx.File) error {
	var missing bool
	if err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM json_each(?) target WHERE NOT EXISTS(SELECT 1 FROM media_replicas r WHERE r.file_id=? AND r.instance_id=target.value AND r.state='ready'))`, dbx.JSONValue(s.StoragePolicy().Archives), f.ID).Scan(&missing); err != nil {
		return err
	}
	if !missing {
		// All selected replicas already hold verified bytes. There is no
		// synchronization operation to expose; retire only upload work space.
		_ = os.Remove(s.plainPath(f.ID))
		return nil
	}
	_, err := s.DB.EnqueueUniqueTask(dbx.TaskArchiveSync, f.ID, "{}")
	return err
}

func (s *Service) runMediaSync(ctx context.Context, f *dbx.File) error {
	ctx = context.WithValue(ctx, mediaSyncContext{}, true)
	work, err := os.MkdirTemp(s.upDir, "media-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var inputs []mediaInput
	var failures []error
	for _, target := range s.rankedArchives() {
		i, e := s.instance(target)
		if e != nil || !i.row.Enabled {
			continue
		}
		r, e := s.mediaReplica(f, target)
		if e != nil {
			return e
		}
		if r.State == "ready" {
			continue
		}
		if inputs == nil {
			inputs, err = s.mediaInputs(ctx, f, work)
			if err != nil {
				return err
			}
		}
		if err = s.writeMediaReplica(ctx, f, target, inputs, work); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", target, err))
			continue
		}
	}
	// A verified layout now exists at every selected archive. The upload source
	// is work space, not an implicit permanent local replica.
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	_ = os.Remove(s.plainPath(f.ID))
	return nil
}

func (s *Service) openWholeMedia(ctx context.Context, f *dbx.File) (io.ReadCloser, error) {
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return nil, err
	}
	forced, _ := ctx.Value(archiveSourceContext{}).(string)
	for _, r := range replicas {
		if forced != "" && r.InstanceID != forced {
			continue
		}
		if r.State == "ready" && len(r.Parts) == 1 && r.Parts[0].Original {
			if input, e := s.openMediaPart(ctx, f, r, r.Parts[0]); e == nil {
				return input, nil
			}
		}
	}
	return nil, errors.New("当前仅保存无损片段，请选择片段下载；不能将片段伪装成字节一致的原文件")
}

func (s *Service) verifyMedia(ctx context.Context, f *dbx.File) (string, error) {
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return "", err
	}
	var last error
	for _, r := range replicas {
		if r.State != "ready" {
			continue
		}
		good := len(r.Parts) > 0
		for _, p := range r.Parts {
			if err = s.verifyMediaPart(ctx, f, r, p); err != nil {
				last = err
				good = false
				break
			}
		}
		if good {
			return dbx.JSONValue(map[string]any{"ok": true, "source": r.InstanceID, "parts": len(r.Parts), "original": len(r.Parts) == 1 && r.Parts[0].Original}), nil
		}
	}
	if last == nil {
		last = errors.New("没有校验通过的存储副本")
	}
	return "", last
}

func (s *Service) purgeMedia(ctx context.Context, f *dbx.File, only string) error {
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return err
	}
	var failures []error
	for _, r := range replicas {
		if only != "" && r.InstanceID != only {
			continue
		}
		if err = ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err = s.DB.SetMediaReplicaState(f.ID, r.InstanceID, "removing", ""); err != nil {
			failures = append(failures, err)
			continue
		}
		b, e := s.archiveBackend(r.InstanceID)
		if e == nil {
			for _, p := range r.Parts {
				if p.Reference == "" {
					continue
				}
				var ref storage.Reference
				if e = json.Unmarshal([]byte(p.Reference), &ref); e != nil {
					break
				}
				if e = b.Delete(ctx, ref); e != nil {
					break
				}
				if _, e = s.DB.Exec(`DELETE FROM media_parts WHERE file_id=? AND instance_id=? AND idx=?`, f.ID, r.InstanceID, p.Index); e != nil {
					break
				}
			}
		}
		if e != nil {
			_ = s.DB.SetMediaReplicaState(f.ID, r.InstanceID, "failed", e.Error())
			failures = append(failures, fmt.Errorf("%s: %w", r.InstanceID, e))
			continue
		}
		if _, e = s.DB.Exec(`DELETE FROM media_replicas WHERE file_id=? AND instance_id=?`, f.ID, r.InstanceID); e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) localMediaTarget(f *dbx.File) string {
	for _, id := range s.StoragePolicy().Archives {
		if i, e := s.instance(id); e == nil && i.row.Driver == "local" {
			return id
		}
	}
	return "local"
}

// MediaPartName deliberately distinguishes a stored segment from the original.
func MediaPartName(f *dbx.File, p dbx.MediaPart) string {
	if p.Original {
		return MediaFilename(f)
	}
	stem := strings.TrimSuffix(MediaFilename(f), filepath.Ext(MediaFilename(f)))
	return fmt.Sprintf("%s.part-%03d-of-%03d.%s", stem, p.Index+1, p.Count, p.Ext)
}

func mediaLayoutMatches(r dbx.MediaReplica, inputs []mediaInput) error {
	for _, part := range r.Parts {
		if !part.Verified {
			continue
		}
		if part.Index < 0 || part.Index >= len(inputs) || part.Count != len(inputs) || inputs[part.Index].part.SHA256 != part.SHA256 {
			return errors.New("已有未完成副本的分片布局与当前来源不同；保留旧引用，请先恢复相同来源后重试")
		}
	}
	return nil
}
