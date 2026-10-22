package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"pimg/internal/dbx"
	"pimg/internal/ids"
)

func (s *Service) UploaderIdentity() (instance, keyset string, err error) {
	if s.Master == nil {
		return "", "", errors.New("主密钥不可用")
	}
	instance, err = s.DB.EnsureSetting("uploader.instance_id", "pi_"+ids.New())
	if err != nil {
		return
	}
	mac := hmac.New(sha256.New, s.Master.Raw())
	mac.Write([]byte("pimg/uploader/keyset/v1\x00" + instance))
	keyset = "ks_" + hex.EncodeToString(mac.Sum(nil)[:16])
	return
}

// nil is the administrator's target inventory; an empty non-nil list grants none.
func (s *Service) UploaderTopics(ids []int) ([]*dbx.TopicBinding, error) {
	return s.UploaderTopicsForOwner(dbx.AdminOwner, ids)
}

func (s *Service) UploaderTopicsForOwner(owner string, ids []int) ([]*dbx.TopicBinding, error) {
	if err := s.TGReady(); err != nil {
		return nil, err
	}
	bs, err := s.DB.ListTopicBindings()
	if err != nil {
		return nil, err
	}
	out := []*dbx.TopicBinding{}
	for _, b := range bs {
		if _, e := s.DB.OwnedAlbum(owner, b.AlbumID); e != nil {
			continue
		}
		if b.State != "active" || b.TopicID <= 1 || b.AccountID != s.UserTG.Status().AccountID {
			continue
		}
		allowed := ids == nil
		for _, id := range ids {
			if id == b.TopicID {
				allowed = true
			}
		}
		if allowed {
			out = append(out, b)
		}
	}
	return out, nil
}
