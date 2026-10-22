package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf16"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/filenames"
	"pimg/internal/mediax"
	"pimg/internal/tguser"
)

var uploaderName = regexp.MustCompile(`^T([PV])([ER])_([0-9]{8})_([0-9]{4})(?:_([TB]))?(?:_D([0-9]{4}))?\.([a-z0-9]+)$`)
var uploaderID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,96}$`)
var uploaderHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type toolCaption struct {
	App        string               `json:"app"`
	V          int                  `json:"v"`
	Tool       bool                 `json:"tool"`
	InstanceID string               `json:"instance_id"`
	KeysetID   string               `json:"keyset_id"`
	UploadID   string               `json:"upload_id"`
	File       string               `json:"file"`
	Kind       string               `json:"kind"`
	Index      int                  `json:"idx"`
	Parts      int                  `json:"parts"`
	SHA256     string               `json:"sha256"`
	WireSHA256 string               `json:"wire_sha256"`
	WireSize   int64                `json:"wire_size"`
	Op         string               `json:"op"`
	Meta       *carrier.CarrierFile `json:"meta"`
}
type countedReader struct {
	io.Reader
	N int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.N += int64(n)
	return n, e
}

// Opening a large original always requires streaming; adapters without streaming
// can only handle small fixtures/covers. No hidden whole-video Download fallback.
func (s *Service) openToolPart(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	if tr, ok := s.UserTG.(tguser.StreamTransport); ok {
		return tr.OpenStream(ctx, ref)
	}
	if ref.Size > 6<<20 {
		return nil, errors.New("TG 传输不支持有界流读取，无法快速接管大文件")
	}
	data, err := s.UserTG.Download(ctx, ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *Service) receiveUploaderMessage(ctx context.Context, msg tguser.Message, album int64) (id string, ignored bool, retErr error) {
	ref := msg.Reference
	p := dbx.UploaderPart{AccountID: ref.AccountID, ChatID: ref.ChatID, TopicID: ref.TopicID, MessageID: ref.MessageID, AlbumID: album}
	p.Name = msg.Name
	defer func() {
		if retErr != nil {
			_ = s.DB.SetTelegramMessageState(ref.AccountID, ref.ChatID, ref.MessageID, "failed", "", retErr.Error())
			if p.ToolFileID == "" {
				p.ToolFileID = fmt.Sprintf("message-%d", ref.MessageID)
				p.InstanceID = "invalid"
				p.KeysetID = fmt.Sprint(ref.AccountID, ":", ref.ChatID)
			}
			_ = s.DB.FailUploader(p, retErr.Error())
		}
	}()
	var c toolCaption
	if len(utf16.Encode([]rune(msg.Caption))) > 1024 || json.Unmarshal([]byte(msg.Caption), &c) != nil || c.App != "pimg" || c.V != 1 || !c.Tool {
		return "", false, errors.New("工具消息说明或版本无效")
	}
	p.InstanceID, p.KeysetID, p.UploadID, p.ToolFileID = c.InstanceID, c.KeysetID, c.UploadID, c.File
	instance, keyset, err := s.UploaderIdentity()
	if err != nil {
		return "", false, err
	}
	if c.InstanceID != instance || c.KeysetID != keyset {
		return "", false, errors.New("工具文件不属于当前实例或密钥域")
	}
	if !uploaderID.MatchString(c.UploadID) || !uploaderID.MatchString(c.File) || !uploaderHash.MatchString(c.SHA256) || !uploaderHash.MatchString(c.WireSHA256) || c.Parts < 1 || c.Parts > 9999 || c.Index < 0 || c.Index >= c.Parts || c.WireSize <= 0 || c.WireSize != ref.Size || ref.DocumentID <= 0 {
		return "", false, errors.New("工具文件身份、哈希、大小或分块范围无效")
	}
	if _, err = strconv.ParseUint(c.Op, 16, 64); err != nil {
		return "", false, errors.New("工具消息操作身份无效")
	}
	receipt, e := s.DB.UploaderReceipt(instance, keyset, c.File)
	if e == nil && receipt.State == "retired" {
		err = s.DB.SetTelegramMessageState(ref.AccountID, ref.ChatID, ref.MessageID, "retired", "", "")
		return "", true, err
	}
	if e != nil && !errors.Is(e, dbx.ErrNotFound) {
		return "", false, e
	}
	name := uploaderName.FindStringSubmatch(msg.Name)
	if name == nil {
		return "", false, errors.New("工具文件名无效")
	}
	expectedKind := "data"
	if name[5] == "T" {
		expectedKind = "thumb"
	}
	if name[5] == "B" {
		expectedKind = "bthumb"
	}
	expectedIndex := 0
	if name[6] != "" {
		n, _ := strconv.Atoi(name[6])
		if n < 2 {
			return "", false, errors.New("工具分块编号无效")
		}
		expectedIndex = n - 1
	}
	if c.Kind != expectedKind || c.Index != expectedIndex {
		return "", false, errors.New("工具文件名与分块身份不一致")
	}
	p.Mode = "direct"
	if name[2] == "E" {
		p.Mode = "tg"
	}
	if p.Mode == "tg" && name[7] != "png" {
		return "", false, errors.New("加密附件必须是 PNG 载体")
	}
	p.Kind, p.Index, p.Parts, p.PayloadSHA256, p.WireSHA256, p.WireSize = c.Kind, c.Index, c.Parts, c.SHA256, c.WireSHA256, c.WireSize
	p.PayloadSize = ref.Size
	var meta *carrier.CarrierFile
	if p.Mode == "tg" {
		rc, e := s.openToolPart(ctx, ref)
		if e != nil {
			return "", false, e
		}
		if rc == nil {
			return "", false, errors.New("TG 未返回载体流")
		}
		var prefix bytes.Buffer
		counter := &countedReader{Reader: io.TeeReader(io.LimitReader(rc, 6<<20), &prefix)}
		h, e := carrier.OpenPart(counter)
		_ = rc.Close()
		if e != nil {
			return "", false, e
		}
		preview, format, e := image.DecodeConfig(bytes.NewReader(prefix.Bytes()))
		if e != nil || format != "png" || preview.Width > 256 || preview.Height > 256 {
			return "", false, errors.New("载体可见预览不符合 256 像素 PNG 规则")
		}
		if _, _, e = image.Decode(bytes.NewReader(prefix.Bytes())); e != nil {
			return "", false, errors.New("载体可见 PNG 损坏")
		}
		if h.FileID != c.File || h.Kind != c.Kind || h.Index != c.Index || h.SHA256 != c.SHA256 || h.Size <= 0 || counter.N+h.Size != ref.Size {
			return "", false, errors.New("载体头与消息说明或附件大小不一致")
		}
		p.PayloadSize = h.Size
		meta = &h.File
	} else {
		if c.WireSHA256 != c.SHA256 {
			return "", false, errors.New("未加密附件的完整哈希与载荷哈希必须相同")
		}
		if c.Meta != nil && (c.Kind != "data" || c.Index != 0) {
			return "", false, errors.New("未加密元数据只能位于首个原件块")
		}
		meta = c.Meta
		if c.Kind == "data" && c.Index == 0 && meta == nil {
			return "", false, errors.New("未加密原件首块缺少恢复元数据")
		}
	}
	partIndex, partCount := 0, 0
	if meta != nil {
		if err = s.validateToolMetadata(meta, p.Mode, name); err != nil {
			return "", false, err
		}
		partIndex, partCount = meta.Naming.PartIndex, meta.Naming.PartCount
		meta.Name = ""
		b, e := json.Marshal(meta)
		if e != nil {
			return "", false, e
		}
		p.Metadata = string(b)
	}
	if p.Kind != "data" && p.PayloadSize > 1<<20 {
		return "", false, errors.New("工具封面过大")
	}
	obj := telegramObject{Reference: ref, Carrier: p.Mode != "direct", WireSHA256: p.WireSHA256}
	object := encodeTelegramObject(obj)
	p.Chunk = dbx.Chunk{Kind: p.Kind, Idx: p.Index, MessageID: sql.NullInt64{Int64: int64(ref.MessageID), Valid: true}, ObjectKey: object, TgFileID: sql.NullString{String: object, Valid: true}, Size: p.PayloadSize, SHA256: p.PayloadSHA256}
	raw, _ := json.Marshal(ref)
	p.Reference = string(raw)
	if err = s.DB.StageUploaderPart(p, partIndex, partCount); err != nil {
		return "", false, err
	}
	if e == nil && (receipt.State == "done" || receipt.State == "duplicate") {
		err = s.DB.SetTelegramMessageState(ref.AccountID, ref.ChatID, ref.MessageID, "managed", receipt.CanonicalID, "")
		return receipt.CanonicalID, false, err
	}
	parts, err := s.DB.UploaderGroup(p)
	if err != nil {
		return "", false, err
	}
	files, previews, err := s.prepareToolGroup(ctx, parts)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, nil
	}
	result, err := s.DB.AdoptUploaderGroup(parts, files)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if result[f.ID] == f.ID {
			own, e := s.DB.UploaderReceipt(p.InstanceID, p.KeysetID, f.ID)
			if e != nil {
				return "", false, e
			}
			if own.State != "done" {
				continue
			}
			if err = s.completeArchive(ctx, f); err != nil {
				return "", false, err
			}
			if err = s.saveLocalPreview(f, previews[f.ID]); err != nil {
				return "", false, err
			}
		}
	}
	return result[c.File], false, nil
}

func (s *Service) validateToolMetadata(m *carrier.CarrierFile, mode string, name []string) error {
	kind := "image"
	if name[1] == "V" {
		kind = "video"
	}
	sequence, _ := strconv.Atoi(name[4])
	if m.Kind != kind || m.Size <= 0 || !uploaderHash.MatchString(m.SHA256) || m.Width <= 0 || m.Height <= 0 || m.Naming.Date != name[3] || m.Naming.Sequence != sequence || !m.Naming.Ready() || m.Naming.PartIndex < 1 || m.Naming.PartCount < m.Naming.PartIndex || m.Naming.PartCount > 9999 {
		return errors.New("工具原件元数据不完整或与文件名不一致")
	}
	if m.Naming.Date != filenames.Capture(time.Unix(m.Naming.OriginalTime, 0), m.Naming.TimeSource).Date {
		return errors.New("原始时间与北京时间日期不一致")
	}
	switch m.Naming.TimeSource {
	case "exif", "video_metadata", "file_created", "ingested":
	default:
		return errors.New("原始时间来源无效")
	}
	if kind == "image" && (m.Width > mediax.MaxPixels || m.Height > mediax.MaxPixels || int64(m.Width)*int64(m.Height) > mediax.MaxPixels || m.Naming.PartCount != 1) {
		return errors.New("图片尺寸或分组无效")
	}
	if kind == "video" && (m.Duration <= 0 || math.IsNaN(m.Duration) || math.IsInf(m.Duration, 0)) {
		return errors.New("视频时长无效")
	}
	info := mediax.Detect(m.Ext, "")
	if info.Kind != kind || info.Mime != m.Mime || m.OrigName == "" {
		return errors.New("原件扩展名或 MIME 不受支持")
	}
	if mode == "direct" {
		if len(m.KeyWrapped) != 0 || len(m.KeyNonce) != 0 {
			return errors.New("未加密元数据不能携带密钥")
		}
		if name[5] == "" && name[7] != m.Ext {
			return errors.New("原件扩展名与文件名不一致")
		}
	} else {
		if m.CryptoVer != cryptox.CryptoVer || len(m.KeyWrapped) != 48 || len(m.KeyNonce) != 12 {
			return errors.New("加密封装版本或长度无效")
		}
		key, err := s.Master.UnwrapFileKey(m.KeyWrapped, m.KeyNonce)
		defer clear(key)
		if err != nil || len(key) != 32 {
			return errors.New("工具封装密钥不能由当前主密钥解封")
		}
	}
	return nil
}

func (s *Service) prepareToolGroup(ctx context.Context, parts []dbx.UploaderPart) ([]*dbx.File, map[string][]byte, error) {
	groups := map[string][]dbx.UploaderPart{}
	for _, p := range parts {
		groups[p.ToolFileID] = append(groups[p.ToolFileID], p)
	}
	previews := map[string][]byte{}
	files := []*dbx.File{}
	expectedCount := 0
	seenIndexes := map[int]bool{}
	var groupTime int64
	var groupDate, groupKind string
	for fileID, ps := range groups {
		if ps[0].Metadata == "" {
			return nil, nil, nil
		}
		var m carrier.CarrierFile
		if err := json.Unmarshal([]byte(ps[0].Metadata), &m); err != nil {
			return nil, nil, err
		}
		if expectedCount == 0 {
			expectedCount = m.Naming.PartCount
			groupTime = m.Naming.OriginalTime
			groupDate = m.Naming.Date
			groupKind = m.Kind
		}
		if m.Naming.PartCount != expectedCount || m.Naming.OriginalTime != groupTime || m.Naming.Date != groupDate || m.Kind != groupKind || seenIndexes[m.Naming.PartIndex] {
			return nil, nil, dbx.ErrUploaderConflict
		}
		seenIndexes[m.Naming.PartIndex] = true
		byKind := map[string][]dbx.UploaderPart{}
		for _, p := range ps {
			name := uploaderName.FindStringSubmatch(p.Name)
			if name == nil {
				return nil, nil, errors.New("工具文件名无效")
			}
			if err := s.validateToolMetadata(&m, p.Mode, name); err != nil {
				return nil, nil, err
			}
			byKind[p.Kind] = append(byKind[p.Kind], p)
		}
		for _, kind := range []string{"data", "thumb", "bthumb"} {
			want := kind == "data" || kind == "thumb" && m.HasThumb || kind == "bthumb" && m.HasBThumb
			ks := byKind[kind]
			if !want && len(ks) > 0 {
				return nil, nil, errors.New("出现元数据未声明的封面")
			}
			if want && (len(ks) == 0 || len(ks) != ks[0].Parts) {
				return nil, nil, nil
			}
		}
		if m.Kind == "video" && len(byKind["data"]) != 1 {
			return nil, nil, errors.New("视频必须按独立可播放片段归档，不支持二进制拆块")
		}
		var dataSize int64
		for _, p := range byKind["data"] {
			dataSize += p.PayloadSize
		}
		expectedSize := m.Size
		if ps[0].Mode != "direct" {
			expectedSize = cryptox.CiphertextSize(m.Size)
		}
		if dataSize != expectedSize {
			return nil, nil, errors.New("原件分块载荷总大小与元数据不一致")
		}
		if ps[0].Mode == "direct" && len(byKind["data"]) == 1 && byKind["data"][0].PayloadSHA256 != m.SHA256 {
			return nil, nil, errors.New("未加密原件的消息哈希与完整原件指纹不一致")
		}
		f := &dbx.File{ID: fileID, OrigName: m.OrigName, Ext: m.Ext, Mime: m.Mime, Kind: m.Kind, Size: m.Size, SHA256: m.SHA256, Width: m.Width, Height: m.Height, Duration: m.Duration, KeyWrapped: m.KeyWrapped, KeyNonce: m.KeyNonce, CryptoVer: m.CryptoVer, AlbumID: sql.NullInt64{Int64: ps[0].AlbumID, Valid: true}, Storage: ps[0].Mode, ColdBackend: "telegram", ArchiveState: "done", Naming: m.Naming, HasThumb: true, HasBThumb: false}
		if f.KeyWrapped == nil {
			f.KeyWrapped = []byte{}
		}
		if f.KeyNonce == nil {
			f.KeyNonce = []byte{}
		}
		for _, kind := range []string{"thumb", "bthumb"} {
			if len(byKind[kind]) == 0 {
				continue
			}
			plain, err := s.readToolCover(ctx, byKind[kind], f)
			if err != nil {
				return nil, nil, err
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(plain))
			if err != nil || format != "jpeg" || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 1280 || cfg.Height > 1280 {
				return nil, nil, errors.New("工具封面不是符合尺寸规则的 JPEG")
			}
			if kind == "thumb" {
				if len(plain) > mediax.PreviewTargetBytes {
					return nil, nil, errors.New("清晰封面超过 100 KiB")
				}
				previews[fileID] = plain
				f.ThumbW = cfg.Width
				f.ThumbH = cfg.Height
				f.ThumbSize = int64(len(plain))
			}
		}
		files = append(files, f)
	}
	if expectedCount == 0 || len(files) != expectedCount {
		return nil, nil, nil
	}
	return files, previews, nil
}

func (s *Service) readToolCover(ctx context.Context, parts []dbx.UploaderPart, f *dbx.File) ([]byte, error) {
	sort.Slice(parts, func(i, j int) bool { return parts[i].Index < parts[j].Index })
	var payload bytes.Buffer
	for _, p := range parts {
		if p.WireSize > 6<<20 {
			return nil, errors.New("工具封面附件超限")
		}
		var ref tguser.Reference
		if json.Unmarshal([]byte(p.Reference), &ref) != nil {
			return nil, errors.New("封面来源定位无效")
		}
		rc, err := s.openToolPart(ctx, ref)
		if err != nil {
			return nil, err
		}
		if rc == nil {
			return nil, errors.New("封面流不可用")
		}
		wire, err := io.ReadAll(io.LimitReader(rc, (6<<20)+1))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(wire)) != p.WireSize || sha256Hex(wire) != p.WireSHA256 {
			return nil, errors.New("封面附件完整性校验失败")
		}
		reader := bytes.NewReader(wire)
		if p.Mode != "direct" {
			if _, err = carrier.OpenPart(reader); err != nil {
				return nil, err
			}
		}
		block, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		if int64(len(block)) != p.PayloadSize || sha256Hex(block) != p.PayloadSHA256 {
			return nil, errors.New("封面载荷校验失败")
		}
		if payload.Len()+len(block) > 1<<20 {
			return nil, errors.New("封面载荷总大小超限")
		}
		payload.Write(block)
	}
	if f.Storage == "direct" {
		return payload.Bytes(), nil
	}
	key, err := s.Master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return cryptox.DecryptBytes(payload.Bytes(), key)
}
