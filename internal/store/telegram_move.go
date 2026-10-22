package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type telegramMovingPart struct {
	InstanceID string          `json:"instance_id,omitempty"`
	PartIndex  int             `json:"part_index,omitempty"`
	MediaPart  *dbx.MediaPart  `json:"media_part,omitempty"`
	ChunkID    int64           `json:"chunk_id"`
	Old        telegramObject  `json:"old"`
	New        *telegramObject `json:"new,omitempty"`
	Cleaned    bool            `json:"cleaned"`
}

func (s *Service) QueueTelegramMove(ctx context.Context, fileID string, albumID int64, operationIDs ...int64) (int64, error) {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return 0, err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return 0, err
	}
	if !s.HasTelegramMedia(f) {
		return 0, s.DB.UpdateFileMeta(fileID, "", sql.NullInt64{Int64: albumID, Valid: true}, nil)
	}
	if err = s.AlbumWritable(albumID); err != nil {
		return 0, err
	}
	if err = s.AlbumWritable(f.AlbumID.Int64); err != nil {
		return 0, err
	}
	if albumID <= 0 {
		albumID, err = s.DB.EnsureOwnerDefaultAlbum(f.OwnerID)
		if err != nil {
			return 0, err
		}
	}
	if _, err = s.DB.OwnedAlbum(f.OwnerID, albumID); err != nil {
		return 0, err
	}
	target, err := s.EnsureTelegramAlbum(ctx, albumID)
	if err != nil {
		return 0, err
	}
	albumID = target.AlbumID
	if old, e := s.DB.GetTelegramMove(fileID); e == nil && old.AlbumID != albumID {
		return 0, errors.New("此文件正在迁移到其他相册，请先完成原任务")
	} else if e != nil && !errors.Is(e, dbx.ErrNotFound) {
		return 0, e
	}
	if task, err := s.DB.FindActiveTask(dbx.TaskTelegramMoveFile, fileID); err == nil {
		if len(operationIDs) > 0 && operationIDs[0] != 0 {
			var existing int64
			if e := s.DB.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, task.ID).Scan(&existing); e != nil || existing != operationIDs[0] {
				return 0, dbx.ErrTaskOperationConflict
			}
		}
		var prior struct {
			AlbumID int64 `json:"album_id"`
		}
		if json.Unmarshal([]byte(task.Payload), &prior) != nil || prior.AlbumID != albumID {
			return 0, errors.New("此文件已有其他目标相册的迁移任务，请先完成原任务")
		}
		return task.ID, nil
	}
	return s.DB.EnqueueTelegramMove(fileID, albumID, operationIDs...)
}

// MoveTelegramFile holds the same file lock as content reads, conversion and
// deletion. Remote copies are verified before one transaction switches all refs.
func (s *Service) MoveTelegramFile(ctx context.Context, fileID string, albumID int64) (retErr error) {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(fileID)
	if err != nil {
		return err
	}
	if !s.HasTelegramMedia(f) {
		return s.DB.UpdateFileMeta(fileID, "", sql.NullInt64{Int64: albumID, Valid: true}, nil)
	}
	if f.ArchiveState != "done" {
		return errors.New("图片尚未完成归档，请稍后重试迁移")
	}
	if albumID <= 0 {
		albumID, err = s.DB.EnsureOwnerDefaultAlbum(f.OwnerID)
		if err != nil {
			return err
		}
	}
	if _, err = s.DB.OwnedAlbum(f.OwnerID, albumID); err != nil {
		return err
	}
	target, err := s.EnsureTelegramAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	move, err := s.DB.GetTelegramMove(fileID)
	if errors.Is(err, dbx.ErrNotFound) {
		move = &dbx.TelegramMove{FileID: fileID, AlbumID: target.AlbumID, Phase: "copy"}
		var parts []telegramMovingPart
		if f.MediaVersion == 2 {
			all, e := s.DB.MediaReplicas(fileID)
			if e != nil {
				return e
			}
			for _, replica := range all {
				if !s.telegramInstance(replica.InstanceID) {
					continue
				}
				for _, part := range replica.Parts {
					if !part.Verified {
						continue
					}
					var ref storage.Reference
					if e = json.Unmarshal([]byte(part.Reference), &ref); e != nil {
						return e
					}
					obj, e := s.telegramObject(ref.Key)
					if e != nil {
						return e
					}
					copy := part
					parts = append(parts, telegramMovingPart{InstanceID: replica.InstanceID, PartIndex: part.Index, MediaPart: &copy, Old: obj})
				}
			}
		}
		for _, kind := range []string{"data", "thumb", "bthumb"} {
			chunks, e := s.DB.ListChunks(fileID, kind)
			if e != nil {
				return e
			}
			for _, chunk := range chunks {
				obj, e := s.telegramObject(chunk.ObjectKey)
				if e != nil {
					return e
				}
				parts = append(parts, telegramMovingPart{ChunkID: chunk.ID, Old: obj})
			}
		}
		if len(parts) == 0 {
			return errors.New("归档没有可迁移的消息")
		}
		b, _ := json.Marshal(parts)
		move.References = string(b)
		if err = s.DB.SaveTelegramMove(move); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if move.AlbumID != target.AlbumID {
		return errors.New("迁移目标与已保存进度不一致")
	}
	var parts []telegramMovingPart
	if err = json.Unmarshal([]byte(move.References), &parts); err != nil {
		return err
	}
	save := func() error {
		b, e := json.Marshal(parts)
		if e != nil {
			return e
		}
		move.References = string(b)
		return s.DB.SaveTelegramMove(move)
	}
	defer func() {
		if retErr != nil {
			move.LastError = retErr.Error()
			_ = save()
		}
	}()
	if move.Phase != "cleanup" {
		for index := range parts {
			p := &parts[index]
			if p.New == nil {
				if p.Old.TopicID == target.TopicID {
					copy := p.Old
					p.New = &copy
				} else {
					ref, e := s.UserTG.Copy(ctx, p.Old.Reference, target.ChatID, target.TopicID, stableTelegramRandomID("move", fileID, p.InstanceID, fmt.Sprint(p.PartIndex), fmt.Sprint(p.ChunkID), fmt.Sprint(target.TopicID)))
					if e != nil {
						return e
					}
					fresh := p.Old
					fresh.Reference = ref
					p.New = &fresh
				}
				if err = save(); err != nil {
					return err
				}
			}
			wire, e := s.openTelegramWire(ctx, *p.New)
			if e != nil {
				return e
			}
			_, readErr := io.Copy(io.Discard, wire)
			closeErr := wire.Close()
			if readErr != nil || closeErr != nil {
				return fmt.Errorf("迁移后的 TG 文件校验失败，已保留原件：%w", errors.Join(readErr, closeErr))
			}
			if err = s.recordManagedTelegram(p.New.Reference, fileID, "", ""); err != nil {
				return err
			}
		}
		if e := s.DB.AlbumWritable(target.AlbumID); e != nil {
			return e
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		// The connection pool has one writer. Do not call another DB method after
		// Begin; the target freeze check is repeated on that transaction below.
		for _, p := range parts {
			key := encodeTelegramObject(*p.New)
			if p.MediaPart != nil {
				part := *p.MediaPart
				part.Reference = referenceJSON(storage.Reference{Key: key})
				if _, e = tx.Exec(`UPDATE media_parts SET metadata=? WHERE file_id=? AND instance_id=? AND idx=?`, dbx.JSONValue(part), fileID, p.InstanceID, p.PartIndex); e != nil {
					return e
				}
				continue
			}
			if _, e = tx.Exec(`UPDATE file_chunks SET object_key=?,message_id=?,tg_file_id=? WHERE id=? AND file_id=?`, key, p.New.MessageID, key, p.ChunkID, fileID); e != nil {
				return e
			}
		}
		result, e := tx.Exec(`UPDATE files SET album_id=?,updated_at=? WHERE id=? AND hidden=0 AND purge_state IS NULL AND NOT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`, target.AlbumID, time.Now().Unix(), fileID, target.AlbumID)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return dbx.ErrAlbumFrozen
		}
		if _, e = tx.Exec(`UPDATE tg_moves SET phase='cleanup',last_error='',updated_at=? WHERE file_id=?`, time.Now().Unix(), fileID); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		move.Phase = "cleanup"
	}
	if fresh, e := s.DB.GetFile(fileID); e != nil {
		return e
	} else if e = s.completeArchive(ctx, fresh); e != nil {
		return e
	}
	for index := range parts {
		p := &parts[index]
		if p.Cleaned {
			continue
		}
		if p.Old.MessageID != p.New.MessageID || p.Old.ChatID != p.New.ChatID {
			if err = s.MarkRetiredTelegram(p.Old.Reference); err != nil {
				return err
			}
			if err = s.UserTG.Delete(ctx, p.Old.Reference); err != nil {
				return fmt.Errorf("图片已迁移，旧消息待清理: %w", err)
			}
		}
		p.Cleaned = true
		if err = save(); err != nil {
			return err
		}
	}
	return s.DB.DeleteTelegramMove(fileID)
}

func (s *Service) HasTelegramMedia(f *dbx.File) bool {
	if s.UserTG == nil {
		return false
	}
	if f.MediaVersion != 2 {
		return f.ColdBackend == "telegram"
	}
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return false
	}
	for _, replica := range all {
		if s.telegramInstance(replica.InstanceID) && len(replica.Parts) > 0 {
			return true
		}
	}
	return false
}
