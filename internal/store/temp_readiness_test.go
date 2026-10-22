package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"io"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type readinessHot struct {
	storage.Hot
	writeBeforeError bool
	onPut            func()
}

func (b readinessHot) PutObject(ctx context.Context, area, key string, r io.Reader, size int64, contentType, cacheControl string) error {
	b.onPut()
	if b.writeBeforeError {
		if err := b.Hot.PutObject(ctx, area, key, r, size, contentType, cacheControl); err != nil {
			return err
		}
	}
	return errors.New("injected ambiguous upload failure")
}

func readinessFixture(t *testing.T) (*Service, *dbx.File) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := png.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(plain.Bytes()), "readiness.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestFailedTempUploadRetainsUnconfirmedCleanupIntent(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		name := "before-write"
		if ambiguous {
			name = "after-write"
		}
		t.Run(name, func(t *testing.T) {
			s, f := readinessFixture(t)
			s.hotBackends["local"] = readinessHot{Hot: s.localHot, writeBeforeError: ambiguous, onPut: func() {
				obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
				if err != nil || obj.ReadyAt.Valid {
					t.Fatal("upload began without an unconfirmed durable cleanup record", err)
				}
			}}
			if err := s.RunTempMaterialize(context.Background(), f.ID, "original"); err == nil {
				t.Fatal("fault injection did not fail upload")
			}
			obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
			if err != nil || obj.ReadyAt.Valid {
				t.Fatal("failed upload was marked confirmed or lost its cleanup record", err)
			}
			view, err := s.GetTempView(context.Background(), f, "original")
			if err != nil {
				t.Fatal(err)
			}
			obj, err = s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
			if err != nil || obj.ReadyAt.Valid != ambiguous {
				t.Fatal("confirmation did not follow the actual verification result", err)
			}
			if ambiguous && view.Status != "ready" || !ambiguous && view.Status != "pending" {
				t.Fatalf("unexpected preview status: %#v", view)
			}
		})
	}
}

func TestTempUploadAndLegacyReuseConfirmation(t *testing.T) {
	for _, preview := range []bool{false, true} {
		name := "materialize-reuse"
		if preview {
			name = "preview-reuse"
		}
		t.Run(name, func(t *testing.T) {
			s, f := readinessFixture(t)
			ctx := context.Background()
			if err := s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
				t.Fatal(err)
			}
			obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
			if err != nil || !obj.ReadyAt.Valid {
				t.Fatal("successful upload was not confirmed before preview", err)
			}
			// Migrated records start unconfirmed even when the stored bytes exist.
			if err := s.DB.ClearTempReady(obj.ID); err != nil {
				t.Fatal(err)
			}
			if preview {
				view, err := s.GetTempView(ctx, f, "original")
				if err != nil || view.Status != "ready" {
					t.Fatal("existing preview verification failed", err)
				}
			} else if err := s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
				t.Fatal(err)
			}
			next, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
			if err != nil || next.ID != obj.ID || !next.ReadyAt.Valid {
				t.Fatal("legacy copy was replaced or not confirmed after verification", err)
			}
			if err := s.localHot.Delete(ctx, storage.Temporary, obj.ObjectKey); err != nil {
				t.Fatal(err)
			}
			view, err := s.GetTempView(ctx, f, "original")
			if err != nil || view.Status != "pending" {
				t.Fatal("missing copy was still offered for preview", err)
			}
			next, err = s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
			if err != nil || next.ReadyAt.Valid {
				t.Fatal("missing copy retained stale confirmation", err)
			}
		})
	}
}
