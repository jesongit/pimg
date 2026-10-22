package store

import (
	"errors"

	"pimg/internal/dbx"
)

// CheckConversion reads metadata only: selection/preview never enqueues work,
// creates replacement files, reads providers or changes the original.
func (s *Service) CheckConversion(sourceID string, encrypted bool) error {
	f, err := s.DB.MutationFile(sourceID)
	if err != nil {
		return err
	}
	if f.PurgeState.String != "" {
		return dbx.ErrDeleting
	}
	var blocked bool
	if err = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE file_id=? AND state='running') OR EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`, sourceID, f.AlbumID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return errors.New("文件另有工作正在执行或相册正在删除，请稍后重试")
	}
	if moving, e := s.DB.TelegramMoveActive(sourceID); e != nil {
		return e
	} else if moving {
		return dbx.ErrTelegramMoving
	}
	_, err = s.conversionPolicies(f, encrypted)
	return err
}

func (s *Service) conversionPolicies(source *dbx.File, encrypted bool) ([]dbx.MediaReplica, error) {
	if source.ArchiveState != "done" {
		return nil, errors.New("归档尚未就绪")
	}
	intentChanged := (source.Storage != "direct") != encrypted
	if source.MediaVersion != 2 {
		if !intentChanged {
			return nil, dbx.ErrConversionNotNeeded
		}
		return nil, nil
	}
	replicas, err := s.DB.MediaReplicas(source.ID)
	if err != nil {
		return nil, err
	}
	changed := intentChanged
	var policies []dbx.MediaReplica
	for _, r := range replicas {
		if r.State != "ready" {
			continue
		}
		i, err := s.instance(r.InstanceID)
		if err != nil {
			return nil, err
		}
		destination := r
		destination.Parts = nil
		destination.Encrypted = encrypted && i.config.Encrypt
		changed = changed || destination.Encrypted != r.Encrypted
		policies = append(policies, destination)
	}
	if len(policies) == 0 {
		return nil, errors.New("没有已就绪的归档副本，无法转换")
	}
	if !changed {
		return nil, dbx.ErrConversionNotNeeded
	}
	return policies, nil
}
