package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func (s *Service) telegramInstance(id string) bool {
	if id == "telegram" {
		return true
	}
	i, err := s.instance(id)
	return err == nil && i.row.Driver == "telegram"
}

func originalArchiveKey(f *dbx.File) string {
	name := MediaFilename(f)
	if f.Storage != "direct" {
		name = CarrierFilename(f)
	}
	return filepath.ToSlash(filepath.Join("accounts", dbx.OwnerDirectory(f.OwnerID), "albums", strconv.FormatInt(f.AlbumID.Int64, 10), f.ID, filepath.Base(name)))
}

func (s *Service) writeArchiveOriginal(ctx context.Context, b storage.Cold, f *dbx.File, c *dbx.Chunk, input io.Reader) (storage.Reference, error) {
	reader, size := input, c.Size
	if f.Storage != "direct" {
		preview, _ := s.carrierPreview(ctx, f)
		prefix, err := carrier.PartPrefix(preview, telegramPartHeader(f, "data", 0), c.Size, c.SHA256)
		if err != nil {
			return storage.Reference{}, err
		}
		reader, size = io.MultiReader(bytes.NewReader(prefix), input), size+int64(len(prefix))
	}
	return s.putArchiveReader(ctx, b, originalArchiveKey(f), reader, size)
}

// Cold transports store complete wire files. Canonical chunk hashes describe
// their original/ciphertext payload, so replication and source verification
// remain independent of each provider's transport representation.
func (s *Service) archivePayload(f *dbx.File, c *dbx.Chunk, instance string, input io.ReadCloser) (io.ReadCloser, error) {
	if f.Storage == "direct" || s.telegramInstance(instance) && s.UserTG != nil {
		return input, nil
	}
	h, err := carrier.OpenPart(input)
	if err == nil && (h.Kind != "data" || h.Index != 0 || h.FileID != f.ID || h.Size != c.Size || h.SHA256 != c.SHA256 || h.File.SHA256 != f.SHA256 || !bytes.Equal(h.File.KeyWrapped, f.KeyWrapped) || !bytes.Equal(h.File.KeyNonce, f.KeyNonce)) {
		err = errors.New("归档载体与文件记录不一致")
	}
	if err != nil {
		input.Close()
		return nil, err
	}
	return input, nil
}

func (s *Service) uploadWholeArchive(ctx context.Context, f *dbx.File, path string) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	hash := sha256New()
	size, err := io.Copy(hash, conversionReader{ctx: ctx, source: input})
	if err != nil {
		return err
	}
	if size <= 0 {
		return errors.New("归档原件为空")
	}
	sum := hexOf(hash)
	existing, err := s.DB.ListChunks(f.ID, "data")
	if err != nil {
		return err
	}
	if len(existing) != 0 {
		if len(existing) == 1 && existing[0].Idx == 0 && existing[0].Size == size && existing[0].SHA256 == sum && chunkLocated(existing[0]) {
			return nil
		}
		return errors.New("已有归档内容与当前原件不一致")
	}
	b, err := s.cold(f)
	if err != nil {
		return err
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	c := &dbx.Chunk{FileID: f.ID, Kind: "data", Idx: 0, Size: size, SHA256: sum}
	ref, err := s.writeArchiveOriginal(ctx, b, f, c, input)
	if err != nil {
		return err
	}
	c.ObjectKey, c.MessageID, c.TgFileID = ref.Key, sql.NullInt64{Int64: ref.MessageID, Valid: ref.MessageID != 0}, sql.NullString{String: ref.FileID, Valid: ref.FileID != ""}
	return s.DB.UpsertChunk(c)
}
