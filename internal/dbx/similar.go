package dbx

import (
	"errors"
)

var ErrSimilarChanged = errors.New("图片状态或所属相册已改变，请重新扫描后再处理")

// SimilarPurgeConflict exposes a stable reason to read-only diagnostics without
// logging image hashes or request bodies.
type SimilarPurgeConflict struct {
	Reason  string
	message string
}

func (e *SimilarPurgeConflict) Error() string { return e.message }
func (e *SimilarPurgeConflict) Unwrap() error { return ErrSimilarChanged }

func similarConflict(reason, message string) error {
	return &SimilarPurgeConflict{Reason: reason, message: message}
}

// EnqueueSimilarPurge rechecks both sides and queues only the explicitly removed
// file in the same transaction. A stale scan cannot delete a moved file, or
// leave its retained counterpart already scheduled for deletion/conversion.
func (db *DB) EnqueueSimilarPurge(actor, owner string, album int64, keep, remove, keepSHA, removeSHA, comparison string, operationID int64, albumSnapshots ...int64) (int64, error) {
	kind, label := "image", "图片"
	if comparison == "video_size" {
		kind, label = "video", "视频"
	} else if comparison != "" && comparison != "image" {
		return 0, similarConflict("invalid_comparison", "不支持的对比方式")
	}
	changed := label + "状态或所属相册已改变，请重新扫描后再处理"
	if owner == "" || actor != AdminOwner && actor != owner || keep == remove || keepSHA == "" || removeSHA == "" || len(albumSnapshots) != 0 && len(albumSnapshots) != 2 {
		return 0, similarConflict("invalid_selection", changed)
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Reserve the writer before reading either side, so a background worker
	// cannot change their state between validation and enqueueing the purge.
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return 0, err
	}
	var isDefault bool
	if len(albumSnapshots) == 0 {
		if err = tx.QueryRow(`SELECT is_default FROM albums WHERE id=? AND owner_id=?`, album, owner).Scan(&isDefault); err != nil {
			return 0, similarConflict("album_changed", changed)
		}
	} else if album != 0 || albumSnapshots[0] < 0 || albumSnapshots[1] < 0 {
		return 0, similarConflict("album_changed", changed)
	}
	var size int64
	for index, item := range []struct{ id, sha string }{{keep, keepSHA}, {remove, removeSHA}} {
		f, e := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=? AND hidden=0`, item.id))
		if e != nil || f.OwnerID != owner || f.Kind != kind {
			return 0, similarConflict("file_or_album_changed", changed)
		}
		if len(albumSnapshots) == 0 {
			if f.AlbumID.Valid && f.AlbumID.Int64 != album || !f.AlbumID.Valid && !isDefault {
				return 0, similarConflict("file_or_album_changed", changed)
			}
		} else {
			// Each side keeps its own observed album, including legacy NULL.
			// Cross-album comparison never relaxes ownership or stale-scan checks.
			expected := albumSnapshots[index]
			if f.AlbumID.Valid && f.AlbumID.Int64 != expected || !f.AlbumID.Valid && expected != 0 {
				return 0, similarConflict("file_or_album_changed", changed)
			}
			if expected > 0 {
				var exists bool
				if e = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM albums WHERE id=? AND owner_id=?)`, expected, owner).Scan(&exists); e != nil {
					return 0, e
				}
				if !exists {
					return 0, similarConflict("album_changed", changed)
				}
			}
		}
		if kind == "video" {
			if f.Size <= 0 || size != 0 && f.Size != size {
				return 0, similarConflict("size_changed", "视频大小不一致，请重新扫描后再处理")
			}
			size = f.Size
		}
		if f.SHA256 != item.sha {
			return 0, similarConflict("content_changed", label+"内容已改变，请重新扫描后再处理")
		}
		if f.ArchiveState != "done" || f.PurgeState.String != "" {
			return 0, similarConflict("file_not_ready", label+"尚未就绪或正在删除，请完成后重新扫描")
		}
		var moving bool
		if e = tx.QueryRow(telegramMoveActiveSQL, item.id, item.id).Scan(&moving); e != nil {
			return 0, e
		}
		if moving {
			return 0, similarConflict("album_move_active", label+"正在移动相册，请完成后重新扫描")
		}
		var conflicts int
		// Replica synchronization does not change the selected image or album.
		// enqueuePurgeTask cancels the removed side's sync, retaining any running
		// lease until the worker stops; the kept side's synchronization continues.
		if e = tx.QueryRow(`SELECT (SELECT COUNT(*) FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?)) + (SELECT COUNT(*) FROM tasks WHERE file_id=? AND state IN('pending','running') AND cancel_requested=0 AND type NOT IN ('archive_sync','cold_sync'))`, item.id, item.id, item.id).Scan(&conflicts); e != nil {
			return 0, e
		}
		if conflicts != 0 {
			return 0, similarConflict("file_busy", label+"还有转换、校验或其他操作未完成，请完成后重新扫描")
		}
	}
	id, err := enqueuePurgeTask(tx, remove, operationID)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
