package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"path/filepath"
	"strings"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

const telegramReferencePrefix = "tguser:"

type telegramObject struct {
	tguser.Reference
	Carrier    bool   `json:"carrier,omitempty"`
	WireSHA256 string `json:"wire_sha256"`
}

func encodeTelegramObject(obj telegramObject) string {
	b, _ := json.Marshal(obj)
	var compact map[string]json.RawMessage
	_ = json.Unmarshal(b, &compact)
	// Persist immutable remote identity; reads/deletes must not follow a new group.
	b, _ = json.Marshal(compact)
	return telegramReferencePrefix + string(b)
}
func decodeTelegramObject(key string) (telegramObject, error) {
	var obj telegramObject
	if !strings.HasPrefix(key, telegramReferencePrefix) {
		return obj, errors.New("此测试归档使用已移除的 Bot 格式，请重新上传")
	}
	err := json.Unmarshal([]byte(strings.TrimPrefix(key, telegramReferencePrefix)), &obj)
	if err != nil || obj.TopicID <= 0 || obj.MessageID <= 0 {
		return obj, errors.New("TG 用户归档定位损坏")
	}
	return obj, nil
}
func (s *Service) telegramObject(key string) (telegramObject, error) {
	obj, err := decodeTelegramObject(key)
	if err != nil {
		return obj, err
	}
	if err = s.TGReady(); err != nil {
		return obj, err
	}
	status := s.UserTG.Status()
	if obj.AccountID == 0 {
		obj.AccountID = status.AccountID
	}
	if obj.ChatID == 0 {
		obj.ChatID = s.TGChatID()
	}
	if obj.AccountID != status.AccountID || obj.ChatID <= 0 {
		return obj, errors.New("TG 归档身份不一致")
	}
	return obj, nil
}
func (s *Service) TGReference(c *dbx.Chunk) (tguser.Reference, error) {
	obj, err := s.telegramObject(c.ObjectKey)
	return obj.Reference, err
}

type userCold struct{ s *Service }

func (u userCold) Put(context.Context, string, []byte, string) (storage.Reference, error) {
	return storage.Reference{}, errors.New("用户归档必须记录文件和相册定位")
}
func (u userCold) Open(ctx context.Context, ref storage.Reference) (io.ReadCloser, error) {
	obj, err := u.s.telegramObject(ref.Key)
	if err != nil {
		return nil, err
	}
	wire, err := u.s.openTelegramWire(ctx, obj)
	if err != nil {
		return nil, err
	}
	if obj.Carrier {
		if _, err := carrier.OpenPart(wire); err != nil {
			wire.Close()
			return nil, err
		}
	}
	return wire, nil
}
func (u userCold) Delete(ctx context.Context, ref storage.Reference) error {
	obj, err := u.s.telegramObject(ref.Key)
	if err != nil {
		return err
	}
	return u.s.UserTG.Delete(ctx, obj.Reference)
}
func (u userCold) Check(ctx context.Context) error { return u.s.TGReady() }

func stableTelegramRandomID(parts ...string) int64 {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	id := int64(binary.LittleEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
	if id == 0 {
		return 1
	}
	return id
}

func privacyPlaceholder() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{R: 18, G: 21, B: 29, A: 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// carrierPreview never falls back to a clear thumbnail. A missing preview is a
// neutral image, and does not prevent lossless archival of the ciphertext.
func (s *Service) carrierPreview(ctx context.Context, f *dbx.File) ([]byte, []byte) {
	return s.carrierPreviewFromSource(ctx, f, s.plainPath(f.ID))
}

func (s *Service) carrierPreviewFromSource(ctx context.Context, f *dbx.File, sourcePath string) ([]byte, []byte) {
	var blur []byte
	if t, err := mediax.MakeThumb(sourcePath, f.Kind, 256); err == nil && t.HasBlur {
		blur = t.Blur
	}
	// Replication and resumed imports may only have the locally managed preview.
	// Generate a blurred derivative; never send the clear preview as the cover.
	if len(blur) == 0 {
		if t, err := mediax.MakeThumb(s.previewPath(f), "image", 256); err == nil && t.HasBlur {
			blur = t.Blur
		}
	}
	if len(blur) == 0 && f.HasBThumb {
		blur, _ = s.readThumbPlain(ctx, f, "bthumb")
	}
	if len(blur) == 0 {
		blur = privacyPlaceholder()
	}
	img, _, err := image.Decode(bytes.NewReader(blur))
	if err != nil {
		blur = privacyPlaceholder()
		img, _, _ = image.Decode(bytes.NewReader(blur))
	}
	var p, j bytes.Buffer
	_ = png.Encode(&p, img)
	_ = jpeg.Encode(&j, img, &jpeg.Options{Quality: 70})
	return p.Bytes(), j.Bytes()
}

func (s *Service) putTelegramChunk(ctx context.Context, f *dbx.File, kind string, idx int, data []byte) error {
	return s.putTelegramStream(ctx, f, kind, idx, bytes.NewReader(data), int64(len(data)), sha256Hex(data))
}

type telegramSourceKey struct{}

// ReceiveTelegramBytes assigns a durable source key before InsertFile, so a
// restart between receiving bytes and acknowledging the message cannot duplicate.
func (s *Service) ReceiveTelegramBytes(ctx context.Context, data []byte, name, mime string, albumID int64, sourceKey string) (*dbx.File, error) {
	return s.ReceiveTelegramStream(ctx, bytes.NewReader(data), name, mime, albumID, sourceKey)
}

func (s *Service) ReceiveTelegramStream(ctx context.Context, input io.Reader, name, mime string, albumID int64, sourceKey string) (*dbx.File, error) {
	if sourceKey != "" {
		if f, err := s.DB.FileByTelegramSource(sourceKey); err == nil {
			return f, nil
		} else if !errors.Is(err, dbx.ErrNotFound) {
			return nil, err
		}
	}
	buffer := bufio.NewReaderSize(input, 2<<20)
	if signature, _ := buffer.Peek(8); bytes.Equal(signature, []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		prefix, _ := buffer.Peek(2 << 20)
		if _, err := carrier.OpenPart(bytes.NewReader(prefix)); err == nil {
			return nil, errors.New("Pimg 归档载体请通过 PNG 导入或离线恢复工具处理，不能作为新媒体重复归档")
		}
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ext == "" {
		switch mime {
		case "image/jpeg":
			ext = "jpg"
		case "image/png":
			ext = "png"
		case "image/webp":
			ext = "webp"
		default:
			if strings.HasPrefix(mime, "video/") {
				ext = strings.TrimPrefix(telegramExtension(mime), ".")
			} else {
				return nil, errors.New("媒体文件名缺少有效扩展名")
			}
		}
		name = "telegram." + ext
	}
	// Intake is private by default; it never creates a permanent public copy.
	ctx = context.WithValue(ctx, telegramSourceKey{}, sourceKey)
	return s.ReceiveUpload(ctx, buffer, name, ext, sql.NullInt64{Int64: albumID, Valid: albumID > 0}, nil, 0, false)
}
