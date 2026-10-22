package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pimg/internal/carrier"
)

func TestReadableNamesTimeSurvivesArchiveImportAndConversion(t *testing.T) {
	s, base := workflowEnv(t)
	plain, err := os.ReadFile(filepath.Join("..", "mediax", "testdata", "taken-20000101.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithFileCreatedTime(context.Background(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	f, err := s.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "unhelpful-camera-name.jpg", "jpg", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "PE_20000101_0001" || f.Naming.TimeSource != "exif" || f.OrigName != "unhelpful-camera-name.jpg" {
		t.Fatal("wrong timestamp precedence or original name lost", f.Name, f.Naming)
	}
	if err := s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	ref, _ := s.telegramObject(chunks[0].ObjectKey)
	if base.messages[ref.MessageID].Name != "PE_20000101_0001.png" {
		t.Fatal("TG carrier filename not applied")
	}
	_, header, _, err := carrier.DecodePart(base.data[ref.MessageID])
	if err != nil || header.File.Naming != f.Naming {
		t.Fatal("carrier lost source time", header, err)
	}
	var exported bytes.Buffer
	if err := s.WriteExport(ctx, f, &exported); err != nil {
		t.Fatal(err)
	}
	_, exportHeader, _, err := carrier.DecodePart(exported.Bytes())
	if err != nil || exportHeader.File.Naming != f.Naming {
		t.Fatal("export lost naming", err)
	}
	imported, err := s.ImportPNG(exported.Bytes(), sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Naming.Date != f.Naming.Date || imported.Naming.OriginalTime != f.Naming.OriginalTime || imported.Naming.Sequence == f.Naming.Sequence {
		t.Fatal("import did not preserve time and allocate a distinct local name", imported.Naming)
	}
	conversion, err := s.QueueConversion(ctx, f.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := s.RunConversion(ctx, f.ID)
	if err != nil || newID != conversion.TargetID {
		t.Fatal(err)
	}
	target, _ := s.DB.GetFile(newID)
	if target.Name != "PR_20000101_0001" || target.Naming != f.Naming {
		t.Fatal("conversion changed original identity", target.Name, target.Naming)
	}
	r, err := s.SourceStream(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(restored, plain) {
		t.Fatal("JPEG/EXIF bytes changed", err)
	}
	if name := MediaFilename(target); name != "PR_20000101_0001.jpg" {
		t.Fatal(name)
	}
	if err := s.RunDirectArchive(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	if key := origKey(target); !strings.HasSuffix(key, "/PR_20000101_0001.jpg") {
		t.Fatal("public object filename not readable", key)
	}
}

func TestNamingCreationTimeThenIntakeFallback(t *testing.T) {
	s, _ := workflowEnv(t)
	plain := workflowPNG(t)
	created := time.Date(2001, 2, 3, 4, 5, 6, 0, time.FixedZone("test", 8*3600))
	f, err := s.receiveLegacyUploadFixture(WithFileCreatedTime(context.Background(), created), bytes.NewReader(plain), "x.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil || f.Name != "PR_20010203_0001" || f.Naming.TimeSource != "file_created" {
		t.Fatal("creation fallback wrong", f, err)
	}
	f, err = s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(plain), "x.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil || f.Naming.TimeSource != "ingested" || f.Naming.Date != time.Now().In(created.Location()).Format("20060102") {
		t.Fatal("intake fallback wrong", f, err)
	}
}
