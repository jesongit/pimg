package store

import (
	"context"
	"fmt"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

// AvailableLink is a shareable URL, never an authenticated admin content route
// or a Telegram download URL containing bot credentials.
type AvailableLink struct {
	ID              string `json:"id"`
	Role            string `json:"role"`
	Backend         string `json:"backend"`
	Kind            string `json:"kind"`
	URL             string `json:"url"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	ObjectExpiresAt int64  `json:"object_expires_at,omitempty"`
	Access          string `json:"access,omitempty"`
	Representation  string `json:"representation,omitempty"`
}

type AvailableLinks struct {
	Items    []AvailableLink `json:"items"`
	Warnings []string        `json:"warnings,omitempty"`
}

// FileLinks enumerates existing copies and token-protected Telegram raw URLs.
// One unavailable provider must not hide links on another recorded provider.
func (s *Service) FileLinks(ctx context.Context, fileID string) (*AvailableLinks, error) {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return nil, err
	}
	objects, err := s.DB.ReadyTempObjects(fileID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	out := s.recordedFileLinks(ctx, f, objects, true)
	// Purge can be requested while HEAD requests are in flight.
	if _, err := s.mutableFile(fileID); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordedFileLinks builds addresses from durable copy records, without remote
// HEAD/download requests. Publication/materialization already verified the copy.
// Temporary signatures are local calculations and retain their actual expiry.
func (s *Service) RecordedFileLinks(ctx context.Context, f *dbx.File, objects ...*dbx.TempObject) *AvailableLinks {
	current, err := s.mutableFile(f.ID)
	if err != nil {
		return &AvailableLinks{Items: make([]AvailableLink, 0)}
	}
	return s.recordedFileLinks(ctx, current, objects, false)
}

// The page caller has already selected and authorized the record and checked
// conversion/purge state. Use its metadata snapshot, without rereading each file.
func (s *Service) RecordedPageLinks(ctx context.Context, f *dbx.File, objects []*dbx.TempObject, chunks map[string][]*dbx.Chunk) *AvailableLinks {
	return s.recordedFileLinks(ctx, f, objects, false, chunks)
}

func (s *Service) recordedFileLinks(ctx context.Context, f *dbx.File, objects []*dbx.TempObject, verify bool, knownChunks ...map[string][]*dbx.Chunk) *AvailableLinks {
	if f.MediaVersion == 2 {
		return s.recordedMediaLinks(f, objects)
	}
	out := &AvailableLinks{Items: make([]AvailableLink, 0)}
	warnings := make(map[string]bool)
	warn := func(message string) {
		if !warnings[message] {
			warnings[message] = true
			out.Warnings = append(out.Warnings, message)
		}
	}
	for _, kind := range []string{"original", "thumb", "blur"} {
		ready, err := s.telegramArchiveReady(f, kind, knownChunks...)
		if err != nil {
			warn("部分 TG 归档链接暂时无法读取，请稍后重试。")
			continue
		}
		if ready {
			if s.UserTG != nil {
				ck, _ := rawArchiveKind(kind)
				var parts []*dbx.Chunk
				if len(knownChunks) > 0 {
					parts = knownChunks[0][ck]
				} else {
					parts, _ = s.DB.ListChunks(f.ID, ck)
				}
				for i := range parts {
					address := s.telegramArchiveURL(f, kind)
					if len(parts) > 1 {
						address += fmt.Sprintf("?part=%d", i)
					}
					out.Items = append(out.Items, AvailableLink{ID: fmt.Sprintf("archive:%s:%d", kind, i), Role: "archive", Backend: "telegram", Kind: kind, URL: address, Access: "api_token", Representation: "stored"})
				}
			} else {
				out.Items = append(out.Items, AvailableLink{ID: "archive:" + kind, Role: "archive", Backend: "telegram", Kind: kind, URL: s.telegramArchiveURL(f, kind), Access: "api_token", Representation: "stored"})
			}
		}
	}
	if f.Storage == "direct" && f.Published && f.PublishedPath.Valid && f.PublishedPath.String != "" && f.HotBackend != "" {
		b, err := s.hot(f.HotBackend)
		if err != nil {
			warn("发布存储暂时不可用，部分链接未能读取。")
		} else {
			objects := []struct{ kind, key string }{{"original", f.PublishedPath.String}}
			for _, obj := range objects {
				if verify {
					size, err := b.HeadSize(ctx, storage.Permanent, obj.key)
					if err != nil || size <= 0 || obj.kind == "original" && size != f.Size {
						warn("部分已发布副本暂时不可用，已保留其他可用链接。")
						continue
					}
				}
				if address := b.PublicURL(obj.key); address != "" {
					out.Items = append(out.Items, AvailableLink{ID: "published:" + obj.kind, Role: "published", Backend: f.HotBackend, Kind: obj.kind, URL: address})
				}
			}
		}
	}
	if address := s.ThumbURL(f); address != "" {
		out.Items = append(out.Items, AvailableLink{ID: "published:thumb", Role: "published", Backend: "pimg", Kind: "thumb", URL: address})
	}
	for _, obj := range objects {
		if obj.FileID != f.ID || !obj.ReadyAt.Valid || obj.DeletedAt.Valid || obj.ExpiresAt <= time.Now().Unix() || obj.ObjectKey == "" {
			continue
		}
		if verify && !s.tempAvailable(ctx, obj) {
			warn("部分临时副本暂时不可用，已保留其他可用链接。")
			continue
		}
		address, expires, err := s.presignTemp(ctx, obj)
		if err != nil || address == "" {
			warn("部分临时副本链接暂时无法生成，已保留其他可用链接。")
			continue
		}
		out.Items = append(out.Items, AvailableLink{ID: "temporary:" + obj.ID, Role: "temporary", Backend: obj.Backend, Kind: obj.Kind, URL: address, ExpiresAt: expires, ObjectExpiresAt: obj.ExpiresAt})
	}
	return out
}
