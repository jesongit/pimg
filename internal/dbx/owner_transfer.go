package dbx

import (
	"database/sql"
	"errors"
	"strconv"
)

// Ownership and archive paths are immutable. A transfer stages a separately
// keyed replacement; the existing conversion journal makes cutover resumable.
func (db *DB) BeginOwnerTransfer(sourceID, targetID string, albumID, operationID int64, wrapped, nonce []byte, cryptoVersion int) (*Conversion, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Serialize with account/album deletion before checking either endpoint.
	if _, err = tx.Exec(`UPDATE files SET updated_at=updated_at WHERE 0`); err != nil {
		return nil, err
	}
	var owner string
	if err = tx.QueryRow(`SELECT owner_id FROM albums WHERE id=?`, albumID).Scan(&owner); err != nil {
		return nil, ErrNotFound
	}
	if err = ownerTransferDestination(tx, owner, albumID); err != nil {
		return nil, err
	}
	if c, e := scanConversion(tx.QueryRow(`SELECT `+conversionCols+` FROM file_conversions c JOIN tasks t ON t.id=c.task_id WHERE c.source_id=?`, sourceID)); e == nil {
		if c.Kind != TaskTransferOwner || c.TargetAlbumID != albumID || c.Phase == "done" {
			return nil, ErrConverting
		}
		if operationID != 0 {
			var existing int64
			if err = tx.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, c.TaskID).Scan(&existing); err != nil {
				return nil, err
			}
			if existing != operationID {
				return nil, ErrTaskOperationConflict
			}
			// A replay never reopens a canceled/sealed input boundary.
			var open bool
			if err = tx.QueryRow(`SELECT input_state='open' AND expires_at>? FROM task_operations WHERE id=?`, now(), operationID).Scan(&open); err != nil {
				return nil, err
			}
			if !open {
				return nil, ErrTaskOperationClosed
			}
		}
		// Completed/failed task retries retain their original operation membership.
		if c.State != "running" {
			if err = retryTaskInTx(tx, c.TaskID); err != nil {
				return nil, err
			}
			if _, err = tx.Exec(`UPDATE file_conversions SET last_error='',updated_at=? WHERE source_id=?`, now(), sourceID); err != nil {
				return nil, err
			}
			c.State, c.LastError = "pending", ""
		}
		return c, tx.Commit()
	} else if !errors.Is(e, ErrNotFound) {
		return nil, e
	}
	var f *File
	if operationID != 0 {
		var actor string
		if err = tx.QueryRow(`SELECT owner_id FROM task_operations WHERE id=?`, operationID).Scan(&actor); err != nil {
			return nil, err
		}
		if actor != AdminOwner {
			return nil, ErrNotFound
		}
		f, err = operationMutationFile(tx, sourceID, operationID, "move_album", "album:"+strconv.FormatInt(albumID, 10))
	} else {
		f, err = scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, sourceID))
	}
	if err != nil {
		return nil, err
	}
	if f.Hidden {
		return nil, ErrNotFound
	}
	if f.OwnerID == owner {
		return nil, errors.New("同一用户内请使用普通相册移动")
	}
	if f.PurgeState.String != "" {
		return nil, ErrDeleting
	}
	if f.ArchiveState != "done" {
		return nil, errors.New("原件归档完成后才能转移归属")
	}
	if err = ownerTransferDestination(tx, f.OwnerID, f.AlbumID.Int64); err != nil {
		return nil, err
	}
	var moving, converting bool
	if err = tx.QueryRow(telegramMoveActiveSQL, sourceID, sourceID).Scan(&moving); err != nil {
		return nil, err
	}
	if moving {
		return nil, ErrTelegramMoving
	}
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?))`, sourceID, sourceID).Scan(&converting); err != nil {
		return nil, err
	}
	if converting {
		return nil, ErrConverting
	}
	var running int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=? AND state='running'`, sourceID).Scan(&running); err != nil {
		return nil, err
	}
	if running != 0 {
		return nil, errors.New("该文件另有任务正在执行，请完成后重试")
	}
	if _, err = tx.Exec(`UPDATE tasks SET state='canceled',updated_at=? WHERE file_id=? AND state='pending'`, now(), sourceID); err != nil {
		return nil, err
	}
	payload := TaskPayloadJSON(map[string]any{"album_id": albumID, "owner_id": owner})
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTransferOwner, sourceID, payload, now(), now(), now())
	if err != nil {
		return nil, err
	}
	taskID, err := r.LastInsertId()
	if err != nil {
		return nil, err
	}
	if operationID != 0 {
		err = AttachTaskOperationItem(tx, taskID, TaskTransferOwner, sourceID, payload, operationID, sourceID)
	} else {
		_, _, err = ensureFollowupOperation(tx, taskID, "move_album")
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,width,height,duration,key_wrapped,key_nonce,crypto_ver,album_id,archive_state,storage,cold_backend,hot_backend,hidden,created_at,updated_at,naming_json,owner_id,media_version,has_thumb,thumb_w,thumb_h,thumb_mime,thumb_size,has_bthumb,home_visibility)
 SELECT ?,name,orig_name,ext,mime,size,sha256,kind,width,height,duration,?,?,?,?,'pending',storage,cold_backend,'',1,created_at,?,naming_json,?,media_version,has_thumb,thumb_w,thumb_h,thumb_mime,thumb_size,has_bthumb,CASE WHEN home_visibility='private' THEN 'private' ELSE 'inherit' END FROM files WHERE id=?`, targetID, wrapped, nonce, cryptoVersion, albumID, now(), owner, sourceID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO file_conversions(source_id,target_id,target_encrypted,phase,task_id,created_at,updated_at) VALUES(?,?,?,'prepare',?,?,?)`, sourceID, targetID, f.Storage != "direct", taskID, now(), now()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetConversion(sourceID)
}

func ownerTransferDestination(tx *sql.Tx, owner string, albumID int64) error {
	if owner != AdminOwner {
		var active bool
		if err := tx.QueryRow(`SELECT disabled=0 AND deleting=0 FROM accounts WHERE id=?`, owner).Scan(&active); err != nil || !active {
			return errors.New("用户不存在、已停用或正在删除")
		}
	}
	if albumID > 0 {
		var valid bool
		if err := tx.QueryRow(`SELECT owner_id=? AND NOT EXISTS(SELECT 1 FROM album_topics WHERE album_id=albums.id AND state IN ('deleting','deleted')) FROM albums WHERE id=?`, owner, albumID).Scan(&valid); err != nil || !valid {
			return ErrAlbumFrozen
		}
	}
	return nil
}

func validateOwnerTransfer(tx *sql.Tx, sourceID, targetID string) error {
	var accepted bool
	if err := tx.QueryRow(`SELECT t.cancel_requested=0 AND t.state IN ('pending','running') FROM tasks t JOIN file_conversions c ON c.task_id=t.id WHERE c.source_id=?`, sourceID).Scan(&accepted); err != nil {
		return err
	}
	if !accepted {
		return errors.New("归属转移已停止，原文件归属保持不变")
	}
	source, err := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, sourceID))
	if err != nil {
		return err
	}
	target, err := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, targetID))
	if err != nil {
		return err
	}
	if source.Hidden || !target.Hidden || source.PurgeState.String != "" || target.PurgeState.String != "" || source.OwnerID == target.OwnerID || source.Size != target.Size || source.SHA256 != target.SHA256 || source.MediaVersion != target.MediaVersion || target.ArchiveState != "done" {
		return errors.New("转移校验未完成，原文件归属保持不变")
	}
	if err = ownerTransferDestination(tx, source.OwnerID, source.AlbumID.Int64); err != nil {
		return err
	}
	return ownerTransferDestination(tx, target.OwnerID, target.AlbumID.Int64)
}

func ownerTransferAlbumGuard(tx *sql.Tx, albumID int64) error {
	var transferring bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_conversions c JOIN tasks t ON t.id=c.task_id JOIN files f ON f.id=c.source_id OR f.id=c.target_id WHERE t.type='transfer_owner' AND c.phase!='done' AND f.album_id=?)`, albumID).Scan(&transferring); err != nil {
		return err
	}
	if transferring {
		return errors.New("相册仍有归属转移，请完成或重试转移后再删除相册")
	}
	return nil
}
