package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/filenames"
	"pimg/internal/ids"
	"pimg/internal/r2x"
	"pimg/internal/storage"
	"pimg/internal/tempcdn"
)

// ---------- 临时查看 ----------

// TempViewResult 临时查看响应。
type TempViewResult struct {
	Status    string `json:"status"` // ready|pending
	URL       string `json:"url,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
}

// GetTempView 命中副本时返回临时外链，否则入队物化任务。
// 私有桶使用限时签名；公开临时域名使用副本保留期，由独立 Worker 清理。
func (s *Service) GetTempView(ctx context.Context, f *dbx.File, kind string) (*TempViewResult, error) {
	unlock, err := s.lockFile(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err = s.mutableFile(f.ID)
	if err != nil {
		return nil, err
	}
	if kind != "original" {
		return nil, errors.New("缩略图由 Pimg 本地管理，不创建临时存储副本")
	}
	now := time.Now().Unix()
	obj, err := s.DB.LiveTempObject(f.ID, kind, now)
	if err == nil {
		if s.tempAvailable(ctx, obj) {
			if err := s.DB.MarkTempReady(obj.ID); err != nil {
				return nil, err
			}
			url, expires, err := s.presignTemp(ctx, obj)
			if err != nil {
				return nil, err
			}
			return &TempViewResult{Status: "ready", URL: url, ExpiresAt: expires}, nil
		}
		if err := s.DB.ClearTempReady(obj.ID); err != nil {
			return nil, err
		}
	}
	if err != nil && !errors.Is(err, dbx.ErrNotFound) {
		return nil, err
	}
	// 复用进行中的物化任务，避免重复下载解密
	t, err := s.DB.FindActiveTask(dbx.TaskTempMaterialize, f.ID)
	if err == nil {
		return &TempViewResult{Status: "pending", TaskID: t.ID}, nil
	}
	if !errors.Is(err, dbx.ErrNotFound) {
		return nil, err
	}
	tid, err := s.DB.EnqueueTask(dbx.TaskTempMaterialize, f.ID,
		dbx.TaskPayloadJSON(TempPayload{Kind: kind}))
	if err != nil {
		return nil, err
	}
	return &TempViewResult{Status: "pending", TaskID: tid}, nil
}

// presignTemp returns the link's own expiry, which may precede object cleanup.
func (s *Service) presignTemp(ctx context.Context, obj *dbx.TempObject) (string, int64, error) {
	if f, err := s.DB.GetFile(obj.FileID); err == nil && f.MediaVersion == 2 {
		return s.mediaTemporaryURL(obj)
	}
	if _, managed := tempcdn.Expiry(obj.ObjectKey); s.publicR2Temporary(obj.Backend) && managed {
		if obj.ExpiresAt <= time.Now().Unix() {
			return "", 0, errors.New("临时副本已过期")
		}
		b, err := s.hot(obj.Backend)
		if err != nil {
			return "", 0, err
		}
		return b.(storage.PublicTemporary).TempPublicURL(obj.ObjectKey), obj.ExpiresAt, nil
	}
	now := time.Now()
	remain := obj.ExpiresAt - now.Unix() - 30 // 30s 安全余量
	if remain <= 0 {
		return "", 0, errors.New("临时副本已过期")
	}
	ttl := time.Duration(s.PresignMinutes()) * time.Minute
	if ttl > time.Duration(remain)*time.Second {
		ttl = time.Duration(remain) * time.Second
	}
	b, err := s.hot(obj.Backend)
	if err != nil {
		return "", 0, err
	}
	address, err := b.PresignGet(ctx, storage.Temporary, obj.ObjectKey, ttl, r2x.TempCacheControl)
	if err != nil {
		return "", 0, err
	}
	expires := now.Add(ttl).Unix()
	// Read the signing timestamp from the generated URL so crossing a second
	// during signing cannot make the displayed lifetime disagree with the link.
	if parsed, err := url.Parse(address); err == nil {
		q := parsed.Query()
		if signedExpiry, err := strconv.ParseInt(q.Get("expires"), 10, 64); err == nil {
			expires = signedExpiry
		} else if signedAt, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date")); err == nil {
			if seconds, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64); err == nil {
				expires = signedAt.Unix() + seconds
			}
		}
	}
	return address, expires, nil
}

// RunTempMaterialize 冷存储→解密→热存储临时区。kind=original 全文件，kind=thumb 缩略图。
func (s *Service) RunTempMaterialize(ctx context.Context, fileID string, kind string) error {
	if kind != "original" {
		return errors.New("临时副本类型无效")
	}
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if f.ArchiveState != "done" {
		return errors.New("文件尚未归档完成")
	}
	now := time.Now().Unix()
	// 双检：等任务期间可能已有可用副本
	if obj, err := s.DB.LiveTempObject(f.ID, kind, now); err == nil {
		if s.tempAvailable(ctx, obj) {
			return s.DB.MarkTempReady(obj.ID)
		}
		if err := s.DB.ClearTempReady(obj.ID); err != nil {
			return err
		}
	}
	var size int64
	if f.MediaVersion == 2 {
		return s.runMediaTemporary(ctx, f)
	}
	var contentType string
	var spill *os.File
	if kind == "thumb" {
		if !f.HasThumb {
			return errors.New("没有缩略图")
		}
		plain, err := s.readThumbPlain(ctx, f, "thumb")
		if err != nil {
			return err
		}
		spill, err = os.CreateTemp(s.ctDir, "spill-*.bin")
		if err != nil {
			return err
		}
		if _, err := spill.Write(plain); err != nil {
			spill.Close()
			os.Remove(spill.Name())
			return err
		}
		spill.Seek(0, io.SeekStart)
		size = int64(len(plain))
		contentType = "image/jpeg"
	} else {
		drc, err := s.ViewOriginal(ctx, f)
		if err != nil {
			return err
		}
		defer drc.Close()
		var err2 error
		h := sha256New()
		spill, size, err2 = s.spillToTemp(io.LimitReader(drc, f.Size+1), h)
		if err2 != nil {
			return err2
		}
		if size != f.Size || hexOf(h) != f.SHA256 {
			spill.Close()
			os.Remove(spill.Name())
			return errors.New("临时发布原件的大小或哈希不匹配")
		}
		contentType = f.Mime
	}
	defer func() {
		spill.Close()
		os.Remove(spill.Name())
	}()
	// 每次物化使用新的随机路径：旧签名链接无法因复用路径而复活。
	expires := time.Now().Unix() + int64(s.RetentionHours())*3600
	key := fmt.Sprintf("tmp/%s/%s.%s", f.ID, ids.NewShort(), f.Ext)
	if f.Naming.Ready() {
		name := MediaFilename(f)
		if kind == "thumb" {
			name = strings.TrimSuffix(name, "."+f.Ext) + "_T.jpg"
		}
		key = fmt.Sprintf("tmp/%s/%s/%s", f.ID, ids.NewShort(), name)
	}
	obj := &dbx.TempObject{
		ID: ids.New(), FileID: f.ID, Kind: kind, ObjectKey: key,
		Size:      sql.NullInt64{Int64: size, Valid: true},
		ExpiresAt: expires,
	}
	obj.Backend = s.TemporaryStorage()
	publicTemp := s.publicR2Temporary(obj.Backend)
	if publicTemp {
		obj.ObjectKey = fmt.Sprintf("tmp/v1/%013d/%s/%s.%s", expires, f.ID, ids.New(), f.Ext)
		if f.Naming.Ready() {
			name := MediaFilename(f)
			if kind == "thumb" {
				name = strings.TrimSuffix(name, "."+f.Ext) + "_T.jpg"
			}
			obj.ObjectKey = fmt.Sprintf("tmp/v1/%013d/%s_%s/%s", expires, f.ID, ids.New(), name)
		}
	}
	if err := s.DB.InsertTempObject(obj); err != nil {
		return err
	}
	// Persist the cleanup intent before writing plaintext. An ambiguous remote
	// upload failure must never leave an object without a durable cleanup record.
	b, err := s.hot(obj.Backend)
	if err != nil {
		return err
	}
	var putErr error
	if publicTemp {
		putErr = b.PutObject(ctx, storage.Temporary, obj.ObjectKey, spill, size, contentType, "public, max-age=60")
	} else {
		putErr = b.PutObject(ctx, storage.Temporary, obj.ObjectKey, spill, size, contentType, r2x.TempCacheControl)
	}
	if putErr != nil {
		return fmt.Errorf("写入热存储临时区域失败: %w", putErr)
	}
	return s.DB.MarkTempReady(obj.ID)
}

// spillToTemp 把流落到受控临时文件（R2 SDK 上传需要可 seek 的流），
// 用后由调用方删除。
func (s *Service) spillToTemp(r io.Reader, h interface{ Write([]byte) (int, error) }) (*os.File, int64, error) {
	tmp, err := os.CreateTemp(s.ctDir, "spill-*.bin")
	if err != nil {
		return nil, 0, err
	}
	var n int64
	if h != nil {
		n, err = io.Copy(io.MultiWriter(tmp, h), r)
	} else {
		n, err = io.Copy(tmp, r)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	return tmp, n, nil
}

// ---------- 永久发布 ----------

// PublishKeys use immutable naming metadata inside an ID namespace. Recorded
// legacy paths remain authoritative; display-name edits never change a URL.
func origKey(f *dbx.File) string {
	if f.PublishedPath.Valid && f.PublishedPath.String != "" {
		return f.PublishedPath.String
	}
	if readableHotNames(f) {
		return fmt.Sprintf("pimg/%s/%s", f.ID, MediaFilename(f))
	}
	return fmt.Sprintf("pimg/%s.%s", f.ID, f.Ext)
}
func thumbKey(f *dbx.File) string {
	if readableHotNames(f) {
		return fmt.Sprintf("thumbs/%s/%s_T.jpg", f.ID, filenames.Stem(f.Kind, false, f.Naming))
	}
	return fmt.Sprintf("thumbs/%s.jpg", f.ID)
}
func bthumbKey(f *dbx.File) string {
	if readableHotNames(f) {
		return fmt.Sprintf("thumbs/%s/%s_B.jpg", f.ID, filenames.Stem(f.Kind, false, f.Naming))
	}
	return fmt.Sprintf("thumbs/%s.blur.jpg", f.ID)
}

func readableHotNames(f *dbx.File) bool {
	return f.Naming.Ready() && (!f.PublishedPath.Valid || f.PublishedPath.String == "" || strings.HasPrefix(f.PublishedPath.String, "pimg/"+f.ID+"/"))
}

// PublicURL 返回文件记录的发布方案提供的公开 URL。
func (s *Service) publicURL(f *dbx.File, key string) string {
	if f.MediaVersion == 2 {
		if f.Hidden || !f.Published || f.PurgeState.String != "" {
			return ""
		}
		return s.PublicBaseURL() + "/published/" + url.PathEscape(f.ID)
	}
	if f.Hidden || f.Storage != "direct" {
		return ""
	}
	if f.HotBackend == "" {
		if current, err := s.DB.GetFile(f.ID); err == nil {
			f = current
		}
	}
	b, err := s.hot(f.HotBackend)
	if err != nil {
		return ""
	}
	return b.PublicURL(key)
}
func (s *Service) PublicURL(f *dbx.File) string { return s.publicURL(f, origKey(f)) }
func (s *Service) ThumbURL(f *dbx.File) string {
	if f.Hidden || f.MediaVersion != 2 && f.Storage != "direct" || !f.Published || f.PurgeState.String != "" || !f.HasThumb {
		return ""
	}
	return s.PublicBaseURL() + "/previews/" + url.PathEscape(f.ID)
}
func (s *Service) BThumbURL(f *dbx.File) string { return "" }

// RunPublish 冷存储母本→热存储永久区，写读校验成功才算发布。
// autoHome=true（移入/直发首页相册）：发布成功后自动上首页。
func (s *Service) RunPublish(ctx context.Context, fileID string, autoHome bool) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.runPublish(ctx, fileID, autoHome)
}

// runPublish is used by direct archival while holding the same file lock.
func (s *Service) runPublish(ctx context.Context, fileID string, autoHome bool) error {
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if f.MediaVersion == 2 {
		if err := s.runMediaPublish(ctx, f, false); err != nil {
			return err
		}
		if autoHome {
			return s.DB.SetShowHome(fileID, true)
		}
		return nil
	}
	if f.Storage != "direct" {
		return dbx.ErrPermanentEncrypted
	}
	if f.ArchiveState != "done" {
		return errors.New("文件尚未归档完成，不能发布")
	}
	if f.Published {
		if autoHome && !f.ShowHome {
			s.DB.SetShowHome(fileID, true)
		}
		return nil // 幂等
	}
	if err := s.bindPublication(f); err != nil {
		return err
	}
	b, err := s.hot(f.HotBackend)
	if err != nil {
		return err
	}
	rc, err := s.ViewOriginal(ctx, f)
	if err != nil {
		s.DB.SetPublished(fileID, false, "", err.Error())
		return err
	}
	// 受控临时空间：落盘母本并计算 sha256，与索引一致才上传（写读校验前置）
	h := sha256New()
	spill, size, err := s.spillToTemp(rc, h)
	rc.Close()
	if err != nil {
		s.DB.SetPublished(fileID, false, "", err.Error())
		return err
	}
	defer func() { spill.Close(); os.Remove(spill.Name()) }()
	if hexOf(h) != f.SHA256 || size != f.Size {
		s.DB.SetPublished(fileID, false, "", "解密内容与索引不一致（哈希或大小）")
		return errors.New("发布中止：解密内容与原件不一致")
	}
	key := origKey(f)
	err = b.PutObject(ctx, storage.Permanent, key, spill, size, f.Mime, "public, max-age=31536000, immutable")
	if err != nil {
		s.DB.SetPublished(fileID, false, "", err.Error())
		return fmt.Errorf("写入热存储永久区域失败: %w", err)
	}
	// 确认永久对象可读且大小一致，才返回永久外链
	got, err := b.HeadSize(ctx, storage.Permanent, key)
	if err != nil || got != f.Size {
		s.DB.SetPublished(fileID, false, "", fmt.Sprintf("永久对象校验失败 size=%d err=%v", got, err))
		return errors.New("发布校验失败：永久对象不可读或大小不一致")
	}

	if err := s.DB.SetPublished(fileID, true, key, ""); err != nil {
		return errors.Join(err, s.removePermanent(ctx, f))
	}
	if autoHome {
		s.DB.SetShowHome(fileID, true)
	}
	return nil
}

// RunPublishAuto 兼容旧签名（不自动上首页）。
func (s *Service) RunPublishAuto(ctx context.Context, fileID string, autoHome bool) error {
	return s.RunPublish(ctx, fileID, autoHome)
}

// RunUnpublish 撤销发布：删除永久对象 + 可选 CDN 清理；不动冷存储原件。
func (s *Service) RunUnpublish(ctx context.Context, fileID string) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.DB.GetFile(fileID)
	if err != nil {
		return err
	}
	if f.Hidden {
		return dbx.ErrNotFound
	}
	if f.PurgeState.String != "" {
		return dbx.ErrDeleting
	}
	if err := s.removePermanent(ctx, f); err != nil {
		s.DB.SetPublishError(fileID, err.Error())
		return err
	}
	if err := s.DB.SetPublished(fileID, false, "", ""); err != nil {
		return err
	}
	if f.Storage != "direct" {
		_, err = s.DB.Exec(`UPDATE files SET hot_backend='' WHERE id=?`, fileID)
		return err
	}
	return nil
}

// Always delete stable keys, even for a previously failed publish/revoke whose
// published flag may not describe the actual remote objects.
func (s *Service) removePermanent(ctx context.Context, f *dbx.File) error {
	if f.MediaVersion == 2 {
		if err := s.DB.SetPublished(f.ID, false, "", ""); err != nil {
			return err
		}
		return s.removeMediaPublications(ctx, f)
	}
	if f.HotBackend == "" {
		return nil
	}
	b, err := s.hot(f.HotBackend)
	if err != nil {
		return unavailableHot(err)
	}
	var errs []error
	var purge []string
	for _, k := range []string{origKey(f)} {
		if err := b.Delete(ctx, storage.Permanent, k); err != nil {
			errs = append(errs, fmt.Errorf("删除永久对象 %s 失败: %w", k, err))
		}
		purge = append(purge, b.PublicURL(k))
	}
	if err := b.PurgeCDN(ctx, purge); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---------- 彻底删除（高风险） ----------

// RunPurgeFile 按记录位置删除冷热存储对象与索引记录。不可恢复。
func (s *Service) RunPurgeFile(ctx context.Context, fileID string) (retErr error) {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	if moving, err := s.DB.TelegramMoveActive(fileID); err != nil {
		return err
	} else if moving {
		return dbx.ErrTelegramMoving
	}
	f, err := s.DB.GetFile(fileID)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			return nil
		}
		return err
	}
	if f.Hidden {
		return dbx.ErrNotFound
	}
	if active, err := s.DB.ConversionActive(fileID); err != nil {
		return err
	} else if active {
		return dbx.ErrConverting
	}
	if refs, err := s.DB.FileTaskReferences(fileID, currentTaskID(ctx)); err != nil {
		return err
	} else if len(refs) > 0 {
		return dbx.ErrTaskReferenced
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err := s.DB.SetPurgeState(fileID, "running"); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, s.DB.SetPurgeState(fileID, "failed"))
		}
	}()
	var errs []error
	// Remove public and temporary copies before Telegram. Telegram can reject
	s.ForgetMediaViews(f)
	// old messages; that must not keep a public URL alive or erase cleanup state.
	if err := s.removePermanent(ctx, f); err != nil {
		errs = append(errs, err)
		if e := s.DB.SetPublishError(fileID, err.Error()); e != nil {
			errs = append(errs, e)
		}
	} else if err := s.DB.SetPublished(fileID, false, "", ""); err != nil {
		errs = append(errs, err)
	}
	objs, err := s.DB.FileTempObjects(fileID)
	if err != nil {
		errs = append(errs, err)
	}
	for _, o := range objs {
		if err := s.deleteTemp(ctx, o); err != nil {
			errs = append(errs, fmt.Errorf("删除临时副本失败: %w", err))
			continue
		}
		if err := s.DB.MarkTempDeleted(o.ID); err != nil {
			errs = append(errs, err)
		}
	}
	// Revoke public copies first even when a secondary is offline. Keep the
	// primary original until its secondary cleanup can finish safely.
	if f.MediaVersion == 2 {
		if err := s.purgeMedia(ctx, f, ""); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	if err := s.purgeReplicaCopies(ctx, fileID); err != nil {
		return errors.Join(append(errs, err)...)
	}
	// Persist per-message progress. A missing message is already deleted; other
	// failures remain visible and can be resumed after a restart.
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, err := s.DB.ListChunks(fileID, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range chunks {
			b, err := s.cold(f)
			if err == nil {
				err = b.Delete(ctx, chunkRef(c))
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s 冷存储原件尚未删除，请处理后重试: %w", f.ColdBackend, err))
				continue
			}
			if err := s.DB.DeleteChunk(c.ID); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := s.DB.Exec(`DELETE FROM archive_objects WHERE file_id=? AND instance_id=? AND kind=? AND idx=? AND reference=?`, fileID, f.ColdBackend, c.Kind, c.Idx, referenceJSON(chunkRef(c))); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := s.purgeArchivePeers(ctx, f); err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}
	// 4. 本地临时文件
	for _, path := range []string{s.plainPath(fileID), s.ctPath(fileID), s.ctDonePath(fileID)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除本地工作副本失败: %w", err)
		}
	}
	if err := s.removeMediaCipherWork(fileID); err != nil {
		return err
	}
	// 5. 记录（chunks/temp_objects 级联删除）
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.DB.DeleteFileAfterTask(fileID, currentTaskID(ctx))
}

// ---------- PNG 载体导入导出 ----------

// WriteExport 把文件导出为"模糊预览 PNG + 附加加密原件"，流式写出。
func (s *Service) WriteExport(ctx context.Context, f *dbx.File, w io.Writer) error {
	if f.Storage == "direct" {
		return errors.New("公开直发文件不支持导出 PNG 载体，仅支持加密归档文件")
	}
	if f.ArchiveState != "done" {
		return errors.New("文件尚未归档完成，不能导出")
	}
	chunks, err := s.DB.ListChunks(f.ID, "data")
	if err != nil {
		return err
	}
	if len(chunks) != 1 || chunks[0].Idx != 0 {
		return errors.New("原件不是独立完整文件")
	}
	c := chunks[0]
	prefix, err := carrier.PartPrefix(privacyPlaceholder(), telegramPartHeader(f, "data", 0), c.Size, c.SHA256)
	if err != nil {
		return err
	}
	if _, err = w.Write(prefix); err != nil {
		return err
	}
	input, err := s.openChunk(ctx, f, c)
	if err != nil {
		return err
	}
	defer input.Close()
	hash := sha256New()
	n, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(input, c.Size+1))
	if err != nil {
		return err
	}
	if n != c.Size || hexOf(hash) != c.SHA256 {
		return errors.New("导出原件校验失败")
	}
	return nil
}

func (s *Service) exportCoverPNG(ctx context.Context, f *dbx.File) ([]byte, error) {
	if f.HasBThumb {
		if plain, err := s.readThumbPlain(ctx, f, "bthumb"); err == nil {
			if pngB, err := toPNG(plain); err == nil {
				return pngB, nil
			}
		}
	}
	// A missing blurred cover must never expose the clear thumbnail.
	// 占位：512x512 深灰
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for i := range img.Pix {
		img.Pix[i] = 18
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Service) readThumbPlain(ctx context.Context, f *dbx.File, kind string) ([]byte, error) {
	if kind != "thumb" {
		return nil, errors.New("不再单独保存模糊封面")
	}
	return s.PreviewThumbnail(ctx, f)
}

// ImportPNG 解析导出的 PNG 载体并注册新文件（复用其封装密钥与密文分片，
// 分片重新写入所选冷存储）。同一主密钥的实例才能导入。
func (s *Service) ImportPNG(data []byte, albumID sql.NullInt64) (*dbx.File, error) {
	_, part, blob, err := carrier.DecodePart(data)
	if err != nil {
		return nil, err
	}
	if part.Kind != "data" || part.Index != 0 {
		return nil, errors.New("请选择完整原件载体")
	}
	if _, err = carrier.Recover(bytes.NewReader(data), io.Discard, s.Master); err != nil {
		return nil, err
	}
	part.File.HasThumb = false
	part.File.HasBThumb = false
	hdr := &carrier.Header{V: 1, File: part.File, Chunks: []carrier.CarrierChunk{{Kind: "data", Idx: 0, Size: part.Size, SHA256: part.SHA256}}}
	if hdr.V != 1 {
		return nil, fmt.Errorf("不支持的载体版本 v=%d", hdr.V)
	}
	if len(hdr.Chunks) == 0 || hdr.Chunks[0].Kind != "data" {
		return nil, errors.New("载体没有数据分片")
	}
	// 校验密文完整性与哈希
	var off int64
	for _, c := range hdr.Chunks {
		if off+c.Size > int64(len(blob)) {
			return nil, errors.New("载体密文不完整")
		}
		off += c.Size
	}
	if off != int64(len(blob)) {
		return nil, errors.New("载体密文长度与分片表不一致")
	}
	// 预检：载体密钥可被当前主密钥解封（否则导入后永远无法解密）
	key, err := s.Master.UnwrapFileKey(hdr.File.KeyWrapped, hdr.File.KeyNonce)
	if err != nil {
		return nil, fmt.Errorf("无法解封载体密钥（主密钥不匹配？）: %w", err)
	}
	_ = key
	// 密文落盘（import 任务从此续传）
	id := ids.New()
	ct := s.ctPath(id)
	if err := os.WriteFile(ct, blob, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.ctDonePath(id), []byte(fmt.Sprintf("%d", len(blob))), 0o600); err != nil {
		return nil, err
	}
	f := &dbx.File{
		ID: id, Name: hdr.File.Name, OrigName: hdr.File.OrigName, Ext: hdr.File.Ext,
		Mime: hdr.File.Mime, Size: hdr.File.Size, SHA256: hdr.File.SHA256, Kind: hdr.File.Kind,
		Width: hdr.File.Width, Height: hdr.File.Height, Duration: hdr.File.Duration,
		KeyWrapped: hdr.File.KeyWrapped, KeyNonce: hdr.File.KeyNonce, CryptoVer: hdr.File.CryptoVer,
		AlbumID: albumID, ArchiveState: "pending",
		Naming: hdr.File.Naming,
	}
	f.ColdBackend = s.initialArchive()
	if err := s.DB.InsertFile(f); err != nil {
		return nil, err
	}
	jobPayload, _ := json.Marshal(hdr)
	if _, err := s.DB.EnqueueTask(dbx.TaskImport, id, string(jobPayload)); err != nil {
		return nil, err
	}
	_ = key
	return s.DB.GetFile(id)
}

// RunImport 执行导入任务：从密文临时文件把分片写入冷存储。
func (s *Service) RunImport(ctx context.Context, fileID string, hdr *carrier.Header) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if f.ArchiveState == "done" {
		return nil
	}
	s.DB.SetArchiveState(fileID, "running", "")
	ct := s.ctPath(fileID)
	if err := s.uploadCarrierChunks(ctx, f, ct, hdr); err != nil {
		s.setArchiveFailure(ctx, fileID, err)
		return err
	}
	if _, err := s.Verify(ctx, fileID); err != nil {
		s.setArchiveFailure(ctx, fileID, err)
		return err
	}
	if err := s.DB.SetThumbInfo(fileID, f.Kind == "image" || f.Kind == "video", f.ThumbW, f.ThumbH, "image/jpeg", 0, false); err != nil {
		return err
	}
	if err := s.DB.SetArchiveState(fileID, "done", ""); err != nil {
		return err
	}
	if err := s.completeArchive(ctx, f); err != nil {
		return err
	}
	os.Remove(ct)
	os.Remove(s.ctDonePath(fileID))
	return nil
}

func (s *Service) uploadCarrierChunks(ctx context.Context, f *dbx.File, ctPath string, hdr *carrier.Header) error {
	if len(hdr.Chunks) != 1 || hdr.Chunks[0].Kind != "data" || hdr.Chunks[0].Idx != 0 {
		return errors.New("载体必须包含一个完整独立原件")
	}
	return s.uploadChunksFromFile(ctx, f, ctPath, "data")
}

// ---------- 清理 ----------

// CleanupExpiredTemp 删除已过期临时对象。
func (s *Service) CleanupExpiredTemp(ctx context.Context) (int, error) {
	objs, err := s.DB.ExpiredTempObjects(time.Now().Unix(), 200)
	if err != nil || len(objs) == 0 {
		return 0, err
	}
	// A cleanup waiting on an idle file lock must be able to make that transfer
	// yield. Empty/quick sweeps do not interrupt useful synchronization work.
	finish := s.BeginForegroundTransfer()
	defer finish()
	n := 0
	var errs []error
	failed := map[string]bool{}
	busyFiles := map[string]bool{}
	for _, o := range objs {
		if ctx.Err() != nil {
			return n, errors.Join(append(errs, ctx.Err())...)
		}
		// Rotate failed objects behind entries not yet attempted, including after
		// restart. One unavailable instance cannot occupy every expiry batch.
		if _, err := s.DB.Exec(`UPDATE temp_objects SET cleanup_attempted_at=? WHERE id=? AND deleted_at IS NULL`, time.Now().UnixNano(), o.ID); err != nil {
			return n, err
		}
		if failed[o.Backend] || busyFiles[o.FileID] {
			continue
		}
		// Active foreground writers may outlive an expiry sweep. Wait once per
		// file, then rotate its objects behind other work instead of waiting for
		// the same lock up to 200 times. Remote deletion has its own timeout.
		lockCtx, stopLock := context.WithTimeout(ctx, time.Second)
		unlock, err := s.lockFile(lockCtx, o.FileID)
		stopLock()
		if err != nil {
			busyFiles[o.FileID] = true
			errs = append(errs, err)
			continue
		}
		child, cancel := context.WithTimeout(ctx, 10*time.Second)
		var due bool
		err = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM temp_objects WHERE id=? AND deleted_at IS NULL AND expires_at<=?)`, o.ID, time.Now().Unix()).Scan(&due)
		if err != nil {
			unlock()
			cancel()
			return n, err
		}
		if !due {
			unlock()
			cancel()
			continue
		}
		err = s.deleteTemp(child, o)
		if err != nil {
			unlock()
			cancel()
			failed[o.Backend] = true
			errs = append(errs, err)
			s.Log.Warn("清理临时对象失败（稍后重试）", "key", o.ObjectKey, "err", err)
			continue
		}
		if err := s.DB.MarkTempDeleted(o.ID); err != nil {
			unlock()
			cancel()
			return n, err
		}
		unlock()
		cancel()
		n++
	}
	return n, errors.Join(errs...)
}

// CleanupLocalTemps 启动时清理孤儿临时文件：
//   - uploads: 归档已完成或记录不存在的明文
//   - tmp:     记录不存在或归档已完成的密文；失败归档保留以便安全续传
//   - spill/peer: 崩溃遗留的工作副本；无持久化续传状态，重新执行时重建
//
// 仅在启动新任务之前调用，不能与工作文件的读写并发执行。
func (s *Service) CleanupLocalTemps() {
	entries, _ := os.ReadDir(s.upDir)
	for _, e := range entries {
		id := e.Name()
		if i := indexByte(id, '.'); i > 0 {
			id = id[:i]
		}
		f, err := s.DB.GetFile(id)
		if err != nil && !errors.Is(err, dbx.ErrNotFound) {
			continue
		}
		if err == nil && f.Hidden {
			if active, lookupErr := s.DB.ConversionActive(id); lookupErr != nil || active {
				continue
			}
		}
		if err != nil || f.ArchiveState == "done" || f.ArchiveState == "failed" {
			// 归档失败的保留明文以便重试
			if err == nil && f.ArchiveState == "failed" {
				continue
			}
			os.Remove(filepath.Join(s.upDir, e.Name()))
		}
	}
	ctEntries, _ := os.ReadDir(s.ctDir)
	if s.cipherDir != s.ctDir {
		cipherEntries, _ := os.ReadDir(s.cipherDir)
		ctEntries = append(ctEntries, cipherEntries...)
	}
	for _, e := range ctEntries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(name, "spill-") && strings.HasSuffix(name, ".bin") ||
			strings.HasPrefix(name, "peer-") && strings.HasSuffix(name, ".part") {
			if err := os.Remove(filepath.Join(s.ctDir, name)); err != nil && !os.IsNotExist(err) {
				s.Log.Warn("清理中断的工作文件失败", "name", name, "err", err)
			}
			continue
		}
		if !hasSuffix(name, ".ct") {
			continue
		}
		id := name[:len(name)-3]
		if active, err := s.DB.ConversionActive(id); err != nil || active {
			continue
		}
		// Failed imports and archives are still retryable. Their ciphertext is
		// the only safe way to resume previously uploaded random AEAD streams.
		if f, err := s.DB.GetFile(id); err == nil && f.ArchiveState != "done" {
			continue
		} else if err != nil && !errors.Is(err, dbx.ErrNotFound) {
			continue
		}
		t, err := s.DB.FindActiveTask(dbx.TaskImport, id)
		if err == nil && t != nil {
			continue
		}
		if t2, err := s.DB.FindActiveTask(dbx.TaskArchive, id); err == nil && t2 != nil {
			continue
		}
		os.Remove(filepath.Join(s.cipherDir, name))
		os.Remove(filepath.Join(s.cipherDir, name+".done"))
	}
}

// ---------- 小工具 ----------

func decryptAll(rc io.ReadCloser, key []byte) ([]byte, error) {
	defer rc.Close()
	dr, err := cryptoxDecryptReader(rc, key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(dr)
}

func toPNG(jpegOrAny []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(jpegOrAny))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
