package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"pimg/internal/dbx"
)

var ErrArchiveLinkUnavailable = errors.New("归档链接不可用")

// telegramArchiveReady checks durable Telegram locators without fetching files
// or creating hot copies merely to enumerate available links.
func (s *Service) telegramArchiveReady(f *dbx.File, kind string, knownChunks ...map[string][]*dbx.Chunk) (bool, error) {
	if f.Hidden || f.ArchiveState != "done" || f.ColdBackend != "telegram" || f.PurgeState.String != "" {
		return false, nil
	}
	if _, err := s.cold(f); err != nil {
		return false, nil
	}
	chunkKind := "data"
	switch kind {
	case "original":
	case "thumb":
		if !f.HasThumb {
			return false, nil
		}
		chunkKind = "thumb"
	case "blur":
		if !f.HasBThumb {
			return false, nil
		}
		chunkKind = "bthumb"
	default:
		return false, nil
	}
	var chunks []*dbx.Chunk
	var err error
	if len(knownChunks) > 0 {
		chunks = knownChunks[0][chunkKind]
	} else {
		chunks, err = s.DB.ListChunks(f.ID, chunkKind)
	}
	if err != nil {
		return false, err
	}
	if len(chunks) == 0 {
		return false, nil
	}
	for i, chunk := range chunks {
		if s.UserTG != nil {
			if chunk.Idx != i {
				return false, nil
			}
			if _, err := s.TGReference(chunk); err != nil {
				return false, nil
			}
			continue
		}
		if chunk.Idx != i || chunk.MessageID.Int64 == 0 || !chunk.TgFileID.Valid || chunk.TgFileID.String == "" {
			return false, nil
		}
	}
	return true, nil
}

// TelegramStoredPart returns the exact remote object without decoding its PNG
// envelope or touching any media encryption key.
func (s *Service) TelegramStoredPart(ctx context.Context, f *dbx.File, kind string, index int) ([]byte, bool, error) {
	r, size, wrapped, err := s.OpenTelegramStoredPart(ctx, f, kind, index)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	if size > 64<<20 {
		return nil, false, errors.New("大文件请使用 TG 流式读取接口")
	}
	data, err := io.ReadAll(r)
	return data, wrapped, err
}

func (s *Service) storedTelegramObject(f *dbx.File, kind string, index int) (telegramObject, error) {
	ck, err := rawArchiveKind(kind)
	if err != nil {
		return telegramObject{}, err
	}
	parts, err := s.DB.ListChunks(f.ID, ck)
	if err != nil {
		return telegramObject{}, err
	}
	if index < 0 || index >= len(parts) {
		return telegramObject{}, ErrArchiveLinkUnavailable
	}
	return s.telegramObject(parts[index].ObjectKey)
}

func (s *Service) TelegramStoredPartSize(f *dbx.File, kind string, index int) (int64, bool, error) {
	obj, err := s.storedTelegramObject(f, kind, index)
	return obj.Size, obj.Carrier, err
}

func (s *Service) OpenTelegramStoredPart(ctx context.Context, f *dbx.File, kind string, index int) (io.ReadCloser, int64, bool, error) {
	obj, err := s.storedTelegramObject(f, kind, index)
	if err != nil {
		return nil, 0, false, err
	}
	r, err := s.openTelegramWire(ctx, obj)
	return r, obj.Size, obj.Carrier, err
}

func (s *Service) UserTelegramEnabled() bool { return s.UserTG != nil }

func (s *Service) telegramArchiveURL(f *dbx.File, kind string) string {
	path := "/media/tg/" + url.PathEscape(f.ID) + "/" + kind
	return s.PublicBaseURL() + path
}

// LockTelegramArchive validates the current archive after token authentication.
// The caller retains the release function until its response completes so
// deletion cannot remove chunks underneath an active read.
func (s *Service) LockTelegramArchive(ctx context.Context, id, kind string) (*dbx.File, func(), error) {
	release, err := s.lockFile(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	f, err := s.mutableFile(id)
	if err != nil {
		release()
		if errors.Is(err, dbx.ErrNotFound) || errors.Is(err, dbx.ErrDeleting) {
			return nil, nil, ErrArchiveLinkUnavailable
		}
		return nil, nil, err
	}
	ready, err := s.telegramArchiveReady(f, kind)
	if err != nil {
		release()
		return nil, nil, err
	}
	if !ready {
		release()
		return nil, nil, ErrArchiveLinkUnavailable
	}
	return f, release, nil
}

func rawArchiveKind(kind string) (string, error) {
	switch kind {
	case "original":
		return "data", nil
	case "thumb":
		return "thumb", nil
	case "blur":
		return "bthumb", nil
	default:
		return "", ErrArchiveLinkUnavailable
	}
}

// RawArchiveSize is the stored byte length, including encryption headers and
// authentication data. The plaintext File.Size must not be used for raw ranges.
func (s *Service) RawArchiveSize(f *dbx.File, kind string) (int64, error) {
	chunkKind, err := rawArchiveKind(kind)
	if err != nil {
		return 0, err
	}
	chunks, err := s.DB.ListChunks(f.ID, chunkKind)
	if err != nil {
		return 0, err
	}
	if len(chunks) == 0 {
		return 0, ErrArchiveLinkUnavailable
	}
	var size int64
	for _, chunk := range chunks {
		n := chunk.Size
		if s.UserTG == nil {
			var meta struct {
				Size int64 `json:"size"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(chunk.ObjectKey, "tgwire:")), &meta) != nil {
				return 0, ErrArchiveLinkUnavailable
			}
			n = meta.Size
		}
		if n < 0 || size > int64(^uint64(0)>>1)-n {
			return 0, ErrArchiveLinkUnavailable
		}
		size += n
	}
	return size, nil
}

// RawArchiveStream concatenates the recorded stored chunks exactly as written.
// It never unwraps a key, decrypts content, or creates a plaintext serving copy.
// Existing per-chunk SHA checks occur when the underlying chunk reaches EOF.
func (s *Service) RawArchiveStream(ctx context.Context, f *dbx.File, kind string) (io.ReadCloser, error) {
	chunkKind, err := rawArchiveKind(kind)
	if err != nil {
		return nil, err
	}
	if s.UserTG == nil {
		chunks, err := s.DB.ListChunks(f.ID, chunkKind)
		if err != nil {
			return nil, err
		}
		if len(chunks) != 1 {
			return nil, ErrArchiveLinkUnavailable
		}
		var meta struct {
			SHA256 string `json:"sha256"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(chunks[0].ObjectKey, "tgwire:")), &meta) != nil || len(meta.SHA256) != 64 {
			return nil, ErrArchiveLinkUnavailable
		}
		input, err := s.TG.Download(ctx, chunks[0].TgFileID.String)
		if err != nil {
			return nil, err
		}
		return &verifyReader{rc: input, want: meta.SHA256}, nil
	}
	return s.openChunks(ctx, f, chunkKind)
}
