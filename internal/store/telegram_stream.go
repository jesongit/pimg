package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/tguser"
)

// checkedTelegramStream validates complete wire bytes without buffering media.
type checkedTelegramStream struct {
	io.ReadCloser
	want       string
	size, read int64
	hash       hashWriter
	checked    bool
}
type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (r *checkedTelegramStream) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.read += int64(n)
	}
	if r.read > r.size {
		return n, errors.New("TG 文件长度超出归档记录")
	}
	if err == io.EOF && !r.checked {
		r.checked = true
		if r.read != r.size || hex.EncodeToString(r.hash.Sum(nil)) != r.want {
			return n, errors.New("TG 载体完整性校验失败")
		}
	}
	return n, err
}

func (s *Service) openTelegramWire(ctx context.Context, obj telegramObject) (io.ReadCloser, error) {
	var rc io.ReadCloser
	var err error
	if transport, ok := s.UserTG.(tguser.StreamTransport); ok {
		rc, err = transport.OpenStream(ctx, obj.Reference)
	} else {
		var data []byte
		data, err = s.UserTG.Download(ctx, obj.Reference)
		if err == nil {
			rc = io.NopCloser(bytes.NewReader(data))
		}
	}
	if err != nil {
		return nil, err
	}
	return &checkedTelegramStream{ReadCloser: rc, want: obj.WireSHA256, size: obj.Size, hash: sha256.New()}, nil
}

func (s *Service) uploadTelegramFile(ctx context.Context, f *dbx.File, path, kind string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	limit := s.telegramUploadLimit()
	var prefix []byte
	if f.Storage != "direct" {
		preview, _ := s.carrierPreview(ctx, f)
		prefix, err = carrier.PartPrefix(preview, telegramPartHeader(f, kind, 0), st.Size(), strings.Repeat("0", 64))
		if err != nil {
			return err
		}
	}
	capacity := limit - int64(len(prefix)) - 16 // additional digits in a later index
	if capacity <= 0 {
		return errors.New("TG 文件上限不足以保存归档载体")
	}
	if kind == "data" && st.Size() > capacity {
		return errors.New("原件超过单文件上限；视频需先切成独立可播放片段，图片请缩小后重试")
	}
	existing, err := s.DB.ListChunks(f.ID, kind)
	if err != nil {
		return err
	}
	done := map[int]*dbx.Chunk{}
	for _, c := range existing {
		done[c.Idx] = c
	}
	for offset, idx := int64(0), 0; offset < st.Size(); offset, idx = offset+capacity, idx+1 {
		size := min(capacity, st.Size()-offset)
		hash := sha256.New()
		if _, err := io.Copy(hash, conversionReader{ctx: ctx, source: io.NewSectionReader(in, offset, size)}); err != nil {
			return err
		}
		sum := hex.EncodeToString(hash.Sum(nil))
		if prior := done[idx]; prior != nil && chunkLocated(prior) {
			if prior.Size != size || prior.SHA256 != sum {
				return errors.New("归档内容与已上传片段不一致，已保留原件")
			}
			continue
		}
		if err := s.putTelegramStream(ctx, f, kind, idx, io.NewSectionReader(in, offset, size), size, sum); err != nil {
			return err
		}
	}
	return nil
}

// Deletion freezes intake and moves, while already accepted archive tasks must
// still finish. Otherwise deletion and the pending archive wait on each other
// forever. The file lock keeps deletion from switching/removing its messages.
func (s *Service) telegramArchiveBinding(ctx context.Context, f *dbx.File) (TopicBinding, error) {
	b, err := s.DB.GetTopicBinding(f.AlbumID.Int64)
	if err == nil && b.State == "deleting" {
		if err := s.TGReady(); err != nil {
			return TopicBinding{}, err
		}
		current, e := s.DB.GetFile(f.ID)
		if e != nil || current.AlbumID.Int64 != b.AlbumID || current.ColdBackend != "telegram" || current.PurgeState.String != "" {
			return TopicBinding{}, dbx.ErrAlbumFrozen
		}
		if b.AccountID != s.UserTG.Status().AccountID || (!s.DB.TelegramChatAllowed(b.AccountID, b.ChatID) && b.ChatID != s.TGChatID()) || b.TopicID <= 1 {
			return TopicBinding{}, dbx.ErrAlbumFrozen
		}
		return *b, nil
	}
	return s.EnsureTelegramAlbum(ctx, f.AlbumID.Int64)
}

func (s *Service) putTelegramStream(ctx context.Context, f *dbx.File, kind string, idx int, data io.Reader, size int64, sum string) error {
	if err := s.TGReady(); err != nil {
		return err
	}
	binding, err := s.telegramArchiveBinding(ctx, f)
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("%s-%s-%04d.bin", f.ID, kind, idx)
	mime := "application/octet-stream"
	var prefix, thumb []byte
	var video *tguser.VideoMetadata
	wrapped := f.Storage != "direct"
	if wrapped {
		preview, t := s.carrierPreview(ctx, f)
		thumb = t
		prefix, err = carrier.PartPrefix(preview, telegramPartHeader(f, kind, idx), size, sum)
		if err != nil {
			return err
		}
		filename = strings.TrimSuffix(filename, ".bin") + ".pimg.png"
		mime = "image/png"
	} else if kind == "data" && idx == 0 && size == f.Size {
		filename, mime = f.OrigName, f.Mime
		if f.Kind == "video" {
			video = &tguser.VideoMetadata{Duration: f.Duration, Width: f.Width, Height: f.Height, SupportsStreaming: true}
			if t, err := mediax.MakeThumb(s.plainPath(f.ID), "video", 256); err == nil {
				thumb = t.Data
			}
		}
	} else if kind == "thumb" || kind == "bthumb" {
		filename = strings.TrimSuffix(filename, ".bin") + ".jpg"
		mime = "image/jpeg"
	}
	if name := TelegramFilename(f, kind, idx); name != "" {
		filename = name
	}
	wireSize := int64(len(prefix)) + size
	if wireSize > s.telegramUploadLimit() {
		return errors.New("封装后的 TG 文件超过当前账号上限，已保留原件")
	}
	// Cover/metadata can change after a failed attempt (e.g. a user rename).
	// A Telegram random_id must never deduplicate different complete documents.
	random := stableTelegramRandomID("upload", f.ID, kind, fmt.Sprint(idx), sum, sha256Hex(prefix), fmt.Sprint(binding.AccountID), fmt.Sprint(binding.TopicID))
	var captionFields map[string]any
	_ = json.Unmarshal([]byte(chunkCaption(f, kind, idx, sum)), &captionFields)
	captionFields["op"] = strconv.FormatInt(random, 16)
	captionBytes, _ := json.Marshal(captionFields)
	caption := string(captionBytes)
	wireHash := sha256.New()
	input := io.TeeReader(io.MultiReader(bytes.NewReader(prefix), data), wireHash)
	var ref tguser.Reference
	if transport, ok := s.UserTG.(tguser.StreamTransport); ok {
		ref, err = transport.UploadStream(ctx, binding.ChatID, binding.TopicID, filename, mime, input, wireSize, thumb, caption, random, video)
	} else {
		// Explicit small in-memory transports support fixtures and embedding.
		if wireSize > 64<<20 {
			return errors.New("当前 TG 传输实现不支持大文件流式上传")
		}
		var payload []byte
		payload, err = io.ReadAll(input)
		if err == nil {
			if uploader, ok := s.UserTG.(tguser.VideoUploader); video != nil && ok {
				ref, err = uploader.UploadVideo(ctx, binding.ChatID, binding.TopicID, filename, mime, payload, thumb, caption, random, *video)
			} else {
				ref, err = s.UserTG.Upload(ctx, binding.ChatID, binding.TopicID, filename, mime, payload, thumb, caption, random)
			}
		}
	}
	if err != nil {
		return err
	}
	ref.Size = wireSize
	key := encodeTelegramObject(telegramObject{Reference: ref, Carrier: wrapped, WireSHA256: hex.EncodeToString(wireHash.Sum(nil))})
	if err := s.recordManagedTelegram(ref, f.ID, filename, mime); err != nil {
		return err
	}
	return s.DB.UpsertChunk(&dbx.Chunk{FileID: f.ID, Kind: kind, Idx: idx, ObjectKey: key, MessageID: sql.NullInt64{Int64: int64(ref.MessageID), Valid: true}, TgFileID: sql.NullString{String: key, Valid: true}, Size: size, SHA256: sum})
}
