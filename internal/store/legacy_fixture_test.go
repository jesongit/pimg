package store

import (
	"context"
	"database/sql"
	"io"
	"pimg/internal/dbx"
	"pimg/internal/testmedia"
)

// These callers test the retained legacy wire/index contracts, rather than
// changing the model of new uploads. Media-v2 tests use ReceiveUpload directly.
func (s *Service) receiveLegacyUploadFixture(ctx context.Context, r io.Reader, name, ext string, album sql.NullInt64, allow []string, max int64, direct bool) (*dbx.File, error) {
	f, err := s.ReceiveUpload(ctx, r, name, ext, album, allow, max, direct)
	if err != nil {
		return nil, err
	}
	if err = testmedia.LegacyRecord(s.DB, f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Service) receiveLegacyTelegramFixture(ctx context.Context, data []byte, name, mime string, album int64, source string) (*dbx.File, error) {
	f, err := s.ReceiveTelegramBytes(ctx, data, name, mime, album, source)
	if err != nil {
		return nil, err
	}
	if err = testmedia.LegacyRecord(s.DB, f); err != nil {
		return nil, err
	}
	return f, nil
}
