// Package store 是图床的核心编排层：上传接收、加密归档、临时解密、
// 永久发布、撤销、校验、彻底删除、PNG 载体导入导出与过期清理。
//
// 设计要点：
//   - 冷存储保留全部母本，热存储保留展示副本；上传工作文件仅存活到归档完成。
//   - 密文临时文件带 .done 标记，任务重试可安全续传分片而不重加密。
//   - 所有任务幂等：已上传分片跳过、已发布对象覆盖写、删除幂等。
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/mediax"
	"pimg/internal/r2x"
	"pimg/internal/storage"
	"pimg/internal/tguser"
	"pimg/internal/tgx"
)

type Service struct {
	Cfg          *config.Config
	DB           *dbx.DB
	Master       *cryptox.Master
	TG           *tgx.Client
	UserTG       tguser.Transport
	telegram     *telegramRuntime
	R2           *r2x.Client
	Log          *slog.Logger
	coldBackends map[string]storage.Cold
	hotBackends  map[string]storage.Hot
	localHot     *storage.LocalHot

	upDir          string // 明文上传临时（生命周期到归档完成）
	ctDir          string // 密文工作临时（生命周期到分片上传完成）
	cipherDir      string // 可续传密文，必须随上传原件保留
	dyn            dynamic
	locks          fileLocks
	previews       *previewCache
	media          *mediaCache
	replicaMu      sync.RWMutex
	replicaDrivers map[string]ReplicaDriver
	extraDrivers   []DriverDefinition
	connection     connectionState
	storages       storageRuntime
	activity       syncActivity
	localImportMu  sync.Mutex
	browser        browserCache
}

func New(cfg *config.Config, db *dbx.DB, master *cryptox.Master, tg *tgx.Client, r2c *r2x.Client, log *slog.Logger, options ...ServiceOption) (*Service, error) {
	s := &Service{
		Cfg: cfg, DB: db, Master: master, TG: tg, R2: r2c, Log: log,
		upDir:    filepath.Join(cfg.Paths.DataDir, "uploads"),
		ctDir:    cfg.Paths.TempDir,
		previews: newPreviewCache(32<<20, 5*time.Minute),
		media:    newMediaCache(),
	}
	if cfg.Paths.LegacyDir != "" {
		s.upDir = filepath.Join(cfg.Paths.DataDir, "work", "uploads")
	}
	s.cipherDir = s.ctDir
	if cfg.Paths.LegacyDir != "" || cfg.Paths.RuntimeDir != "" {
		s.cipherDir = filepath.Join(cfg.Paths.DataDir, "work", "cipher")
	}
	for _, option := range options {
		option(s)
	}
	if err := s.migrateStoragePolicies(); err != nil {
		return nil, err
	}
	if err := s.initStorageConnection(); err != nil {
		return nil, err
	}
	if err := s.initDynamic(); err != nil {
		return nil, err
	}
	s.initReplicaDrivers()
	if tg == nil {
		if err := s.initTelegramRuntime(); err != nil {
			return nil, err
		}
	}
	if err := s.backfillTelegramIdentity(); err != nil {
		return nil, err
	}
	if err := s.initBackends(); err != nil {
		return nil, err
	}
	if err := s.initInstances(); err != nil {
		return nil, err
	}
	if _, err := s.DB.EnsureDefaultAlbum(); err != nil {
		return nil, err
	}
	for _, d := range []string{s.upDir, s.ctDir, s.cipherDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := s.QueueLocalLayouts(); err != nil {
		return nil, err
	}
	if err := s.ReconcileAccountSpaces(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) plainPath(id string) string  { return filepath.Join(s.upDir, id+".bin") }
func (s *Service) ctPath(id string) string     { return filepath.Join(s.cipherDir, id+".ct") }
func (s *Service) ctDonePath(id string) string { return s.ctPath(id) + ".done" }

// TempPayload / VerifyPayload 任务 payload。
type TempPayload struct {
	Kind string `json:"kind"` // original|thumb
}

// ---------- 上传接收 ----------

// ReceiveUpload 流式接收一个上传文件：写明文临时文件、探测媒体信息、
// 建立索引记录并投递归档任务。direct=true 为「首页相册直发」：
// 冷存储保存明文分片（不加密），归档完成后自动发布。
func (s *Service) ReceiveUpload(ctx context.Context, r io.Reader, origName, ext string, albumID sql.NullInt64, allowedExts []string, maxSize int64, direct bool) (*dbx.File, error) {
	files, err := s.ReceiveUploadBatch(ctx, r, origName, ext, albumID, allowedExts, maxSize, direct)
	if err != nil {
		return nil, err
	}
	return files[0], nil
}

// ReceiveUploadBatch retains one logical file with per-storage representations.
// atomic database receipt for the entire incoming source.
func (s *Service) ReceiveUploadBatch(ctx context.Context, r io.Reader, origName, ext string, albumID sql.NullInt64, allowedExts []string, maxSize int64, direct bool) (received []*dbx.File, retErr error) {
	span := s.DB.Performance().Start(ctx, "stage", "receive_and_hash")
	var measuredBytes int64
	defer func() { span.FinishResult(retErr, 0, measuredBytes) }()
	if idle, _ := ctx.Value(idleIntakeContext{}).(bool); !idle {
		finishActivity := s.BeginForegroundTransfer()
		defer finishActivity()
	}
	owner := dbx.ContextOwner(ctx)
	if albumID.Valid {
		if _, err := s.DB.OwnedAlbum(owner, albumID.Int64); err != nil {
			return nil, err
		}
	} else {
		id, err := s.DB.EnsureOwnerDefaultAlbum(owner)
		if err != nil {
			return nil, err
		}
		albumID = sql.NullInt64{Int64: id, Valid: true}
	}
	initialArchive := s.initialArchiveForOwner(owner)
	if initialArchive == "" {
		_ = s.ReconcileAccountSpaces()
		return nil, errors.New("账号归档空间尚未就绪，请等待系统分配或联系管理员")
	}
	if err := s.AlbumWritable(albumID.Int64); err != nil {
		return nil, err
	}
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if allowedExts == nil {
		allowedExts = s.AllowedExts()
	}
	allowed := false
	for _, e := range allowedExts {
		if e == ext {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("不支持的文件类型 .%s（允许: %s）", ext, strings.Join(allowedExts, ", "))
	}
	if maxSize <= 0 {
		maxSize = int64(s.MaxSizeMB()) << 20
	}
	id := ids.New()
	accepted := false
	defer func() {
		if !accepted {
			_ = os.Remove(s.plainPath(id))
		}
	}()
	dst, err := os.OpenFile(s.plainPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	head := make([]byte, 0, 512)
	var size int64
	lastOperationTouch := time.Now()
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			dst.Close()
			return nil, err
		}
		if operation, ok := ctx.Value(uploadOperationKey{}).(uploadOperation); ok && time.Since(lastOperationTouch) >= time.Minute {
			if err := s.DB.TouchTaskOperation(operation.ID, owner); err != nil {
				dst.Close()
				return nil, err
			}
			lastOperationTouch = time.Now()
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if size == 0 && len(head) == 0 {
				head = append(head, buf[:min(n, 512)]...)
			}
			if maxSize > 0 && size+int64(n) > maxSize {
				dst.Close()
				os.Remove(s.plainPath(id))
				return nil, fmt.Errorf("文件超过大小限制 %d MB", maxSize>>20)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				dst.Close()
				os.Remove(s.plainPath(id))
				return nil, werr
			}
			h.Write(buf[:n])
			size += int64(n)
			measuredBytes += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			dst.Close()
			os.Remove(s.plainPath(id))
			return nil, rerr
		}
	}
	if err := dst.Close(); err != nil {
		return nil, err
	}
	if size == 0 {
		os.Remove(s.plainPath(id))
		return nil, errors.New("空文件")
	}

	sniffed := ""
	if len(head) >= 12 {
		sniffed = httpDetect(head)
	}
	info := mediax.Detect(ext, sniffed)
	name := strings.TrimSuffix(origName, filepath.Ext(origName))
	if name == "" {
		name = id
	}
	fileKey := cryptox.GenerateFileKey()
	wrapped, nonce, err := s.Master.WrapFileKey(fileKey)
	if err != nil {
		return nil, err
	}
	f := &dbx.File{
		OwnerID: owner, ID: id, Name: name, OrigName: origName, Ext: ext, Mime: info.Mime,
		Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), Kind: info.Kind,
		KeyWrapped: wrapped, KeyNonce: nonce, CryptoVer: cryptox.CryptoVer,
		AlbumID: albumID, ArchiveState: "pending",
	}
	if operation, ok := ctx.Value(uploadOperationKey{}).(uploadOperation); ok {
		f.OperationID, f.OperationItemKey = operation.ID, operation.ItemKey
	}
	f.ParentTaskID, _ = ctx.Value(intakeParentKey{}).(int64)
	if source, _ := ctx.Value(telegramSourceKey{}).(string); source != "" {
		f.TelegramSource = source
	}
	f.ColdBackend = initialArchive
	if direct {
		f.Storage = "direct"
		f.NoAutoPublish, _ = ctx.Value(privateUploadKey{}).(bool)
	}
	f.InputHash, f.InputSize = f.SHA256, f.Size
	if f.Kind == "video" {
		if existing, err := s.DB.InputDuplicate(f.OwnerID, f.InputHash, f.InputSize, !direct); err != nil {
			return nil, err
		} else if len(existing) > 0 {
			if err = s.DB.BindInputSource(f.TelegramSource, existing); err != nil {
				return nil, err
			}
			_ = os.Remove(s.plainPath(id))
			accepted = true
			return existing, nil
		}
	}
	if info.Kind == "image" {
		if w, hh, err := mediax.ImageInfo(s.plainPath(id)); err == nil {
			f.Width, f.Height = w, hh
		}
	} else if info.Kind == "video" {
		d, w, hh := mediax.ProbeVideo(s.plainPath(id))
		f.Duration, f.Width, f.Height = d, w, hh
	}
	f.Naming = captureNaming(ctx, s.plainPath(id), f.Kind)
	files, err := s.prepareUploadBatch(ctx, f)
	accepted = err == nil
	return files, err
}

// RunDirectArchive 直发归档：明文分片存冷存储 → 缩略图 → 自动发布。
// 冷存储始终保留一份母本：私有文件为密文，直发文件为明文。
func (s *Service) RunDirectArchive(ctx context.Context, fileID string) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if err = s.selectInitialArchive(f); err != nil {
		return err
	}
	started := time.Now()
	publish, err := s.DB.UploadAutoPublish(fileID)
	if err != nil {
		return err
	}
	err = s.runDirectArchive(ctx, f, publish)
	if f.MediaVersion != 2 {
		s.recordStorageResult(f.ColdBackend, f.Size, started, err)
	}
	return err
}

func (s *Service) runDirectArchive(ctx context.Context, f *dbx.File, publish bool) error {
	if f.MediaVersion == 2 {
		return s.runMediaArchive(ctx, f, publish)
	}
	fileID := f.ID
	if f.Storage != "direct" {
		return errors.New("非直发文件，不能走直发归档")
	}
	if f.ArchiveState != "done" {
		if err := s.DB.SetArchiveState(fileID, "running", ""); err != nil {
			return err
		}
		plain := s.plainPath(fileID)
		// 1. 明文分片直传冷存储，复用同一分片通道。
		if err := s.uploadChunksFromFile(ctx, f, plain, "data"); err != nil {
			s.DB.SetArchiveState(fileID, "failed", err.Error())
			return err
		}
		// 2. 缩略图（同样明文存底）
		if err := s.archiveThumbs(ctx, f, plain); err != nil {
			s.Log.Warn("直发缩略图生成失败", "file", fileID, "err", err)
		}
		if _, err := s.Verify(ctx, fileID); err != nil {
			s.DB.SetArchiveState(fileID, "failed", err.Error())
			return err
		}
		if err := s.DB.SetArchiveState(fileID, "done", ""); err != nil {
			return err
		}
		if err := s.completeArchive(ctx, f); err != nil {
			return err
		}
		s.seedOriginalCache(f, plain)
		os.Remove(plain)
	}
	if err := s.completeArchive(ctx, f); err != nil {
		return err
	}
	if publish {
		return s.runPublish(ctx, fileID, true)
	}
	return nil
}

// httpDetect 内容嗅探。
func httpDetect(b []byte) string {
	return http.DetectContentType(b)
}

// ---------- 归档（加密 → 冷存储分片） ----------

// RunArchive 执行归档任务：明文 → Tink 流式加密 → 密文分片写入冷存储。
func (s *Service) RunArchive(ctx context.Context, fileID string) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if err = s.selectInitialArchive(f); err != nil {
		return err
	}
	started := time.Now()
	err = s.runEncryptedArchive(ctx, f)
	if f.MediaVersion != 2 {
		s.recordStorageResult(f.ColdBackend, f.Size, started, err)
	}
	return err
}

func (s *Service) runEncryptedArchive(ctx context.Context, f *dbx.File) error {
	if f.MediaVersion == 2 {
		return s.runMediaArchive(ctx, f, false)
	}
	fileID := f.ID
	if f.ArchiveState == "done" {
		return s.completeArchive(ctx, f)
	}
	if f.Storage == "direct" {
		return errors.New("直发文件不能使用加密归档任务，请重试直发归档")
	}
	if err := s.DB.SetArchiveState(fileID, "running", ""); err != nil {
		return err
	}
	plain := s.plainPath(fileID)
	ct := s.ctPath(fileID)
	done := s.ctDonePath(fileID)

	// 阶段 1：加密（若上次已完整加密则复用密文文件，保证分片续传一致）。
	if err := s.ensureCiphertext(f, plain, ct, done); err != nil {
		s.DB.SetArchiveState(fileID, "failed", err.Error())
		return err
	}

	// 阶段 2：分片上传。
	if err := s.uploadChunksFromFile(ctx, f, ct, "data"); err != nil {
		s.DB.SetArchiveState(fileID, "failed", err.Error())
		return err
	}

	// 阶段 3：缩略图（明文仍在本地，直接生成后同样加密上传）。
	if err := s.archiveThumbs(ctx, f, plain); err != nil {
		// 缩略图失败不阻断原件归档——记录后继续
		s.Log.Warn("缩略图生成失败", "file", fileID, "err", err)
	}

	// Do not discard the only local original until the remote archive can be
	// recovered with the recorded hash, including after a resumed transfer.
	if _, err := s.Verify(ctx, fileID); err != nil {
		s.DB.SetArchiveState(fileID, "failed", err.Error())
		return err
	}
	if err := s.DB.SetArchiveState(fileID, "done", ""); err != nil {
		return err
	}
	if err := s.completeArchive(ctx, f); err != nil {
		return err
	}
	s.seedOriginalCache(f, plain)
	os.Remove(plain)
	os.Remove(ct)
	os.Remove(done)
	return nil
}

func (s *Service) ensureCiphertext(f *dbx.File, plain, ct, done string) error {
	if st, err := os.Stat(done); err == nil && st.Size() > 0 {
		if cst, err := os.Stat(ct); err == nil {
			var want int64
			data, _ := os.ReadFile(done)
			fmt.Sscanf(string(data), "%d", &want)
			if want == cst.Size() {
				return nil // 复用
			}
		}
	}
	// A new Tink stream has fresh randomness. Old chunks cannot be reused with
	// regenerated ciphertext; preserve the original and fail safely instead.
	chunks, err := s.DB.ListChunks(f.ID, "data")
	if err != nil {
		return err
	}
	if len(chunks) > 0 {
		return errors.New("已有归档分片，但完整密文工作文件缺失或损坏；已停止归档且不会清理本地原件，请恢复密文工作文件或重新上传原件")
	}
	os.Remove(ct)
	os.Remove(done)

	fileKey, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return err
	}
	src, err := os.Open(plain)
	if err != nil {
		return fmt.Errorf("明文临时文件缺失（可能已被清理，请重新上传）: %w", err)
	}
	defer src.Close()
	er, err := cryptox.EncryptReader(src, fileKey)
	if err != nil {
		return err
	}
	if closer, ok := er.(io.Closer); ok {
		defer closer.Close()
	}
	dst, err := os.Create(ct)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, er)
	cerr := dst.Close()
	if err != nil || cerr != nil {
		os.Remove(ct)
		if err == nil {
			err = cerr
		}
		return err
	}
	if err := os.WriteFile(done, []byte(fmt.Sprintf("%d", n)), 0o600); err != nil {
		return err
	}
	return nil
}

func chunkCaption(f *dbx.File, kind string, idx int, sum string) string {
	b, _ := json.Marshal(map[string]any{"app": "pimg", "v": 1, "file": f.ID, "kind": kind, "idx": idx, "sha256": sum})
	return string(b)
}

// uploadChunksFromFile 把归档内容分片写入冷存储（跳过已记录分片）。
func (s *Service) uploadChunksFromFile(ctx context.Context, f *dbx.File, ctPath, kind string) error {
	if f.ColdBackend == "telegram" && s.UserTG != nil {
		return s.uploadTelegramFile(ctx, f, ctPath, kind)
	}
	if kind == "data" {
		return s.uploadWholeArchive(ctx, f, ctPath)
	}
	return errors.New("归档只保存独立原件")
}

func (s *Service) archiveThumbs(ctx context.Context, f *dbx.File, plain string) error {
	t, err := mediax.MakePreview(plain, f.Kind, mediax.PreviewMaxDimension)
	if err != nil {
		return err
	}
	if err := s.saveLocalPreview(f, t.Data); err != nil {
		return err
	}
	return s.DB.SetThumbInfo(f.ID, true, t.W, t.H, "image/jpeg", int64(len(t.Data)), false)

}

// uploadPlainBytes 明文字节直接存冷存储（仅用于直发文件）。
func (s *Service) uploadPlainBytes(ctx context.Context, f *dbx.File, kind string, idx int, data []byte) error {
	return s.putChunk(ctx, f, kind, idx, data)
}

func (s *Service) uploadEncryptedBytes(ctx context.Context, f *dbx.File, fileKey []byte, kind string, idx int, plain []byte) error {
	enc, err := cryptox.EncryptBytes(plain, fileKey)
	if err != nil {
		return err
	}
	return s.putChunk(ctx, f, kind, idx, enc)
}

// ---------- 从冷存储读取归档 ----------

// chunkSource 顺序下载并校验多个分片。
type chunkSource struct {
	ctx    context.Context
	svc    *Service
	file   *dbx.File
	chunks []*dbx.Chunk
	cur    *verifyReader
	i      int
}

func (cs *chunkSource) Read(p []byte) (int, error) {
	for {
		if cs.cur == nil {
			if cs.i >= len(cs.chunks) {
				return 0, io.EOF
			}
			c := cs.chunks[cs.i]
			rc, err := cs.svc.openChunk(cs.ctx, cs.file, c)
			if err != nil {
				return 0, fmt.Errorf("下载分片 %d(%s) 失败: %w", c.Idx, c.Kind, err)
			}
			cs.cur = &verifyReader{rc: rc, want: c.SHA256}
			cs.i++
		}
		n, err := cs.cur.Read(p)
		if err == io.EOF {
			if !cs.cur.verified {
				cs.cur.Close()
				return 0, fmt.Errorf("分片 %d 密文 sha256 校验失败", cs.chunks[cs.i-1].Idx)
			}
			cs.cur.Close()
			cs.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (cs *chunkSource) Close() error {
	if cs.cur != nil {
		return cs.cur.Close()
	}
	return nil
}

// verifyReader 包一层 sha256 校验。
type verifyReader struct {
	rc       io.ReadCloser
	h        hash.Hash
	want     string
	verified bool
}

func (hv *verifyReader) Read(p []byte) (int, error) {
	if hv.h == nil {
		hv.h = sha256.New()
	}
	n, err := hv.rc.Read(p)
	if n > 0 {
		hv.h.Write(p[:n])
	}
	if err == io.EOF && !hv.verified {
		hv.verified = true
		if hex.EncodeToString(hv.h.Sum(nil)) != hv.want {
			hv.verified = false
		}
	}
	return n, err
}
func (hv *verifyReader) Close() error { return hv.rc.Close() }

// openChunks 打开某类分片的原始字节流（逐片 sha256 校验）。
func (s *Service) openChunks(ctx context.Context, f *dbx.File, kind string) (*chunkSource, error) {
	chunks, err := s.DB.ListChunks(f.ID, kind)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("文件 %s 没有 %s 分片", f.ID, kind)
	}
	for _, c := range chunks {
		if !chunkLocated(c) {
			return nil, fmt.Errorf("分片 %d 缺少冷存储定位信息", c.Idx)
		}
	}
	return &chunkSource{ctx: ctx, svc: s, file: f, chunks: chunks}, nil
}

// SourceStream 返回文件母本流：私有上传=解密流；direct=明文分片流。
// 两条路径都带逐片 sha256 校验；调用方负责整流哈希终验。
func (s *Service) SourceStream(ctx context.Context, f *dbx.File) (io.ReadCloser, error) {
	if f.MediaVersion == 2 {
		return s.openWholeMedia(ctx, f)
	}
	if f.Storage == "direct" {
		src, err := s.openChunks(ctx, f, "data")
		if err != nil {
			return nil, err
		}
		return src, nil
	}
	return s.DecryptStream(ctx, f, "data")
}

// DecryptStream 返回解密后的明文流（仅用于密文归档文件）。
func (s *Service) DecryptStream(ctx context.Context, f *dbx.File, kind string) (io.ReadCloser, error) {
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return nil, err
	}
	src, err := s.openChunks(ctx, f, kind)
	if err != nil {
		return nil, err
	}
	pr, err := cryptox.DecryptReader(src, key)
	if err != nil {
		src.Close()
		return nil, err
	}
	return &readCloser{r: pr, closer: src}, nil
}

type readCloser struct {
	r      io.Reader
	closer io.Closer
}

func (rc *readCloser) Read(p []byte) (int, error) { return rc.r.Read(p) }
func (rc *readCloser) Close() error               { return rc.closer.Close() }

// Verify 校验冷存储母本可完整恢复为与原件一致的文件（不落盘）。
// 密文文件=解密后比对；直发文件=明文分片流直接比对。
func (s *Service) Verify(ctx context.Context, fileID string) (result string, retErr error) {
	f, err := s.DB.GetFile(fileID)
	if err != nil {
		return "", err
	}
	defer func() {
		if ctx.Err() == nil {
			if e := s.DB.RecordFileVerification(f, retErr); e != nil {
				s.Log.Warn("记录恢复校验结果失败", "file", fileID)
			}
		}
	}()
	return s.verifyArchivedOriginal(ctx, f, false)
}
