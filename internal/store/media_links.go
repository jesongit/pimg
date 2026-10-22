package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

func (s *Service) recordedMediaLinks(f *dbx.File, objects []*dbx.TempObject) *AvailableLinks {
	all, _ := s.DB.MediaReplicas(f.ID)
	return s.recordedMediaLinksFrom(f, objects, all)
}

func (s *Service) recordedMediaLinksFrom(f *dbx.File, objects []*dbx.TempObject, all []dbx.MediaReplica) *AvailableLinks {
	out := &AvailableLinks{Items: make([]AvailableLink, 0)}
	{
		for _, replica := range all {
			for _, p := range replica.Parts {
				if !p.Verified {
					continue
				}
				out.Items = append(out.Items, AvailableLink{ID: fmt.Sprintf("archive:%s:%d", replica.InstanceID, p.Index), Role: "archive", Backend: replica.InstanceID, Kind: "original", URL: fmt.Sprintf("%s/media/archive/%s/%s/%d", s.PublicBaseURL(), url.PathEscape(f.ID), url.PathEscape(replica.InstanceID), p.Index), Access: "api_token", Representation: "stored"})
			}
		}
	}
	if address := s.PublicURL(f); address != "" {
		out.Items = append(out.Items, AvailableLink{ID: "published:view", Role: "published", Backend: f.HotBackend, Kind: "original", URL: address, Representation: "compatible"})
	}
	if address := s.ThumbURL(f); address != "" {
		out.Items = append(out.Items, AvailableLink{ID: "published:thumb", Role: "published", Backend: "pimg", Kind: "thumb", URL: address})
	}
	for _, obj := range objects {
		if obj.FileID != f.ID || !obj.ReadyAt.Valid || obj.DeletedAt.Valid {
			continue
		}
		address, expires, err := s.mediaTemporaryURL(obj)
		if err != nil || expires <= 0 {
			continue
		}
		out.Items = append(out.Items, AvailableLink{ID: "temporary:" + obj.ID, Role: "temporary", Backend: obj.Backend, Kind: obj.Kind, URL: address, ExpiresAt: expires, ObjectExpiresAt: obj.ExpiresAt, Representation: "compatible"})
	}
	return out
}

// ServeStoredMedia is called behind token-only administrator authentication.
// It returns the stored representation, including encryption/carrier bytes.
func (s *Service) ServeStoredMedia(w http.ResponseWriter, r *http.Request) {
	unlock, err := s.lockFile(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "读取归档失败", 503)
		return
	}
	defer unlock()
	f, err := s.mutableFile(r.PathValue("id"))
	if err != nil || f.MediaVersion != 2 {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(r.PathValue("part"))
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		http.Error(w, "读取归档索引失败", 503)
		return
	}
	var replica dbx.MediaReplica
	var part dbx.MediaPart
	found := false
	for _, copy := range all {
		if copy.InstanceID != r.PathValue("instance") {
			continue
		}
		for _, p := range copy.Parts {
			if p.Index == index && p.Verified {
				replica, part, found = copy, p, true
				break
			}
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	var ref storage.Reference
	if json.Unmarshal([]byte(part.Reference), &ref) != nil {
		http.NotFound(w, r)
		return
	}
	var input io.ReadCloser
	if s.telegramInstance(replica.InstanceID) && s.UserTG != nil {
		var obj telegramObject
		obj, err = s.telegramObject(ref.Key)
		if err == nil {
			input, err = s.openTelegramWire(r.Context(), obj)
		}
	} else {
		var b storage.Cold
		b, err = s.archiveBackend(replica.InstanceID)
		if err == nil {
			input, err = b.Open(r.Context(), ref)
		}
	}
	if err != nil {
		http.Error(w, "归档存储暂不可用", 503)
		return
	}
	defer input.Close()
	name, contentType := MediaPartName(f, part), part.MIME
	if replica.Encrypted {
		name = f.ID + ".part-" + strconv.Itoa(index) + ".pimg.png"
		contentType = "image/png"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(part.StoredSize, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	_, _ = io.CopyN(w, input, part.StoredSize)
}

// Remove a broken persisted part from the ready set without losing its cleanup reference.
func (s *Service) markMediaPartUnavailable(f *dbx.File, replica dbx.MediaReplica, part dbx.MediaPart, reason error) error {
	part.Verified = false
	return errors.Join(s.DB.SaveMediaPart(f.ID, replica.InstanceID, part), s.DB.SetMediaReplicaState(f.ID, replica.InstanceID, "failed", reason.Error()))
}

func (s *Service) openMatchingMediaPart(ctx context.Context, f *dbx.File, p dbx.MediaPart) (io.ReadCloser, error) {
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return nil, err
	}
	for _, replica := range all {
		for _, part := range replica.Parts {
			if !part.Verified || part.Size != p.Size || part.SHA256 != p.SHA256 {
				continue
			}
			input, e := s.openMediaPart(ctx, f, replica, part)
			if e == nil {
				return input, nil
			}
			err = errors.Join(err, e)
		}
	}
	if err == nil {
		err = errors.New("没有可读取的相同片段副本")
	}
	return nil, err
}
