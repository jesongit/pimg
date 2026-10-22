package store

import (
	"context"
	"os"
	"strings"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/mediax"
)

func (s *Service) telegramUploadLimit() int64 {
	if s.UserTG != nil {
		if n := s.UserTG.Status().UploadLimitBytes; n > 0 {
			return n
		}
	}
	return 2000 << 20
}

func telegramPartHeader(f *dbx.File, kind string, idx int) carrier.PartHeader {
	return carrier.PartHeader{FileID: f.ID, Kind: kind, Index: idx, File: carrier.CarrierFile{Name: f.Name, OrigName: f.OrigName, Ext: f.Ext, Mime: f.Mime, Size: f.Size, SHA256: f.SHA256, Kind: f.Kind, Width: f.Width, Height: f.Height, Duration: f.Duration, KeyWrapped: f.KeyWrapped, KeyNonce: f.KeyNonce, CryptoVer: f.CryptoVer, Naming: f.Naming}}
}

// Reserve a bounded 256px PNG preview plus the real metadata, including longer
// segment filenames. Both modes use this ceiling so later encryption fits too.
func videoPlainBudget(f *dbx.File, wireLimit int64) int64 {
	copyFile := *f
	copyFile.Name += ".part-99999-of-99999"
	copyFile.OrigName += ".part-99999-of-99999"
	copyFile.Naming.Sequence = 9999
	copyFile.Naming.PartIndex = 99999
	copyFile.Naming.PartCount = 99999
	prefix, err := carrier.PartPrefix(privacyPlaceholder(), telegramPartHeader(&copyFile, "data", 0), wireLimit, strings.Repeat("0", 64))
	if err != nil {
		return 0
	}
	overhead := int64(len(prefix)) + (256*256*4 + 4096)
	lo, hi := int64(0), wireLimit
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if cryptox.CiphertextSize(mid)+overhead <= wireLimit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// A media source is one logical file. Storage-specific representations are
// created by the archive worker, never by changing the uploaded bytes.
func (s *Service) prepareUploadBatch(ctx context.Context, source *dbx.File) ([]*dbx.File, error) {
	source.MediaVersion = 2
	for _, target := range s.StoragePolicy().Archives {
		policy, err := s.mediaPolicy(source, target)
		if err != nil {
			return nil, err
		}
		source.MediaPolicies = append(source.MediaPolicies, policy)
	}
	source.Naming.PartIndex, source.Naming.PartCount = 1, 1
	if source.Kind == "video" {
		info, err := mediax.InspectVideo(ctx, s.plainPath(source.ID))
		if err != nil {
			return nil, err
		}
		source.Width, source.Height, source.Duration = info.Width, info.Height, info.Duration
	}
	out := []*dbx.File{source}
	if err := s.DB.InsertUploadBatch(out); err != nil {
		return nil, err
	}
	if out[0].UploadDuplicate {
		_ = os.Remove(s.plainPath(source.ID))
	}
	return out, nil
}
