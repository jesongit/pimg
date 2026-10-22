package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/filenames"
	"pimg/internal/mediax"
)

type sourceCreatedTimeKey struct{}

// WithFileCreatedTime accepts an original creation time supplied by an uploader
// that can read it. Browsers cannot supply filesystem birth time automatically.
func WithFileCreatedTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, sourceCreatedTimeKey{}, t)
}

func captureNaming(ctx context.Context, path, kind string) filenames.Metadata {
	t, source := mediax.OriginalTime(ctx, path, kind)
	if source == "" {
		if created, ok := ctx.Value(sourceCreatedTimeKey{}).(time.Time); ok && !created.IsZero() {
			t, source = created, "file_created"
		} else {
			t, source = time.Now(), "ingested"
		}
	}
	return filenames.Capture(t, source)
}

// MediaFilename names restored plaintext. IDs remain authoritative locators;
// original upload names remain in OrigName for reference.
func MediaFilename(f *dbx.File) string {
	if name := filenames.Name(f.Kind, false, f.Ext, f.Naming); name != "" {
		return name
	}
	return f.OrigName
}

func CarrierFilename(f *dbx.File) string {
	if name := filenames.Name(f.Kind, true, "png", f.Naming); name != "" {
		return name
	}
	return f.Name + ".pimg.png"
}

func TelegramFilename(f *dbx.File, kind string, index int) string {
	if kind == "original" {
		kind = "data"
	}
	if kind == "blur" {
		kind = "bthumb"
	}
	if !f.Naming.Ready() {
		return ""
	}
	encrypted := f.Storage != "direct"
	ext := f.Ext
	if encrypted {
		ext = "png"
	} else if kind != "data" {
		ext = "jpg"
	}
	stem := filenames.Stem(f.Kind, encrypted, f.Naming)
	if kind == "thumb" {
		stem += "_T"
	} else if kind == "bthumb" {
		stem += "_B"
	}
	if index > 0 {
		stem += fmt.Sprintf("_D%04d", index+1)
	}
	return stem + "." + strings.TrimPrefix(ext, ".")
}
