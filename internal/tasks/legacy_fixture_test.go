package tasks

import (
	"context"
	"database/sql"
	"io"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"pimg/internal/testmedia"
)

func legacyUploadFixture(s *store.Service, ctx context.Context, r io.Reader, name, ext string, album sql.NullInt64, allow []string, max int64, direct bool) (*dbx.File, error) {
	f, err := s.ReceiveUpload(ctx, r, name, ext, album, allow, max, direct)
	if err != nil {
		return nil, err
	}
	if err = testmedia.LegacyRecord(s.DB, f); err != nil {
		return nil, err
	}
	return f, nil
}
