package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

func operationMutationFile(tx *sql.Tx, fileID string, operationID int64, kind, scope string) (*File, error) {
	var owner, storedKind, storedScope, input string
	var expiry int64
	if err := tx.QueryRow(`SELECT owner_id,kind,scope,input_state,expires_at FROM task_operations WHERE id=? AND request_key!=''`, operationID).Scan(&owner, &storedKind, &storedScope, &input, &expiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if storedKind != kind || storedScope != scope {
		return nil, ErrTaskOperationConflict
	}
	if input != "open" || expiry <= now() {
		return nil, ErrTaskOperationClosed
	}
	f, err := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, fileID))
	if err != nil {
		return nil, err
	}
	if f.Hidden || owner != AdminOwner && f.OwnerID != owner {
		return nil, ErrNotFound
	}
	if f.PurgeState.String != "" {
		return nil, ErrDeleting
	}
	var converting, frozen bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?)),EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`, fileID, fileID, f.AlbumID).Scan(&converting, &frozen); err != nil {
		return nil, err
	}
	if converting {
		return nil, ErrConverting
	}
	if frozen {
		return nil, ErrAlbumFrozen
	}
	return f, nil
}

// MoveFileInOperation keeps synchronous local moves inside the same input and
// cancellation boundary as queued moves. Rejected/over-capacity input cannot
// change the album first and discover the rejection while writing its receipt.
func (db *DB) MoveFileInOperation(fileID string, albumID, operationID int64) error {
	if albumID < 0 {
		return errors.New("目标相册无效")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return err
	}
	f, err := operationMutationFile(tx, fileID, operationID, "move_album", "album:"+strconv.FormatInt(albumID, 10))
	if err != nil {
		return err
	}
	var moving bool
	if err = tx.QueryRow(telegramMoveActiveSQL, fileID, fileID).Scan(&moving); err != nil {
		return err
	}
	if moving {
		return ErrTelegramMoving
	}
	var album any
	if albumID > 0 {
		var targetOwner string
		var frozen bool
		if err = tx.QueryRow(`SELECT a.owner_id,EXISTS(SELECT 1 FROM album_topics t WHERE t.album_id=a.id AND t.state IN ('deleting','deleted')) FROM albums a WHERE a.id=?`, albumID).Scan(&targetOwner, &frozen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if frozen {
			return ErrAlbumFrozen
		}
		if targetOwner != f.OwnerID {
			return ErrNotFound
		}
		album = albumID
	}
	if err = recordTaskOperationItem(tx, operationID, fileID, fileID, f.OrigName, "done", ""); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE files SET album_id=?,updated_at=? WHERE id=?`, album, now(), fileID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=?`, now(), now()+TaskOperationIntakeTTL, operationID); err != nil {
		return err
	}
	return tx.Commit()
}

// EnqueueOperationTask serializes active-job discovery with attachment. A
// repeated chunk may reuse its own worker, but cannot borrow another operation's
// worker or create a second active worker after racing another HTTP request.
func (db *DB) EnqueueOperationTask(typ, fileID, payload string, operationID int64) (int64, error) {
	if typ != TaskPublish && typ != TaskUnpublish {
		return 0, errors.New("操作任务类型无效")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE tasks SET updated_at=updated_at WHERE 0`); err != nil {
		return 0, err
	}
	f, err := operationMutationFile(tx, fileID, operationID, typ, "")
	if err != nil {
		return 0, err
	}
	var id, existingOperation int64
	err = tx.QueryRow(`SELECT t.id,COALESCE(j.operation_id,0) FROM tasks t LEFT JOIN task_operation_jobs j ON j.task_id=t.id WHERE t.type=? AND t.file_id=? AND t.state IN ('pending','running') ORDER BY t.id DESC LIMIT 1`, typ, fileID).Scan(&id, &existingOperation)
	if err == nil {
		if existingOperation != operationID {
			return 0, ErrTaskOperationConflict
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if typ == TaskPublish && f.MediaVersion != 2 && f.Storage != "direct" {
		return 0, ErrPermanentEncrypted
	}
	if typ == TaskPublish && (f.ArchiveState != "done" || f.Published) {
		return 0, errors.New("尚未归档完成或已经发布")
	}
	if typ == TaskUnpublish && !f.Published {
		return 0, errors.New("文件尚未发布")
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, typ, fileID, payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = AttachTaskOperationItem(tx, id, typ, fileID, payload, operationID, fileID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// The selected attempts and their state changes share a write transaction, so a
// concurrent completion/new attempt cannot turn an obsolete failure into work.
func (db *DB) RetryLatestTaskGroup(id int64) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE tasks SET updated_at=updated_at WHERE 0`); err != nil {
		return 0, err
	}
	var key string
	if err = tx.QueryRow(taskGroupsCTE+` SELECT group_key FROM task_members WHERE id=(SELECT id FROM grouped_tasks WHERE id=?)`, id).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	rows, err := tx.Query(taskGroupsCTE+` SELECT id FROM ranked_members WHERE group_key=? AND relevance=1 AND state IN ('failed','canceled') AND type!='operation_intake' ORDER BY id`, key)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var task int64
		if err = rows.Scan(&task); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, task := range ids {
		if err = retryTaskInTx(tx, task); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

func retryTaskInTx(tx *sql.Tx, id int64) error {
	var err error
	var fileID sql.NullString
	var typ string
	if err := tx.QueryRow(`SELECT file_id,type FROM tasks WHERE id=?`, id).Scan(&fileID, &typ); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	albumTask := typ == TaskTelegramScan || typ == TaskTelegramReceive || typ == TaskTelegramDeleteAlbum || typ == TaskTelegramRenameAlbum
	if typ == TaskSystemSchedule {
		return ErrSystemScheduleManaged
	}
	if albumTask {
		var payload string
		if err = tx.QueryRow(`SELECT payload FROM tasks WHERE id=?`, id).Scan(&payload); err != nil {
			return err
		}
		var p struct {
			AlbumID   int64 `json:"album_id"`
			AccountID int64 `json:"account_id"`
			ChatID    int64 `json:"chat_id"`
			MessageID int   `json:"message_id"`
		}
		if err = json.Unmarshal([]byte(payload), &p); err != nil || p.AlbumID <= 0 {
			return errors.New("相册任务参数无效")
		}
		b, e := scanTopic(tx.QueryRow(`SELECT `+topicCols+` FROM album_topics WHERE album_id=?`, p.AlbumID))
		if errors.Is(e, ErrNotFound) && typ == TaskTelegramRenameAlbum {
			// New ordinary-user albums do not have a topic until this task runs.
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM albums WHERE id=?)`, p.AlbumID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
		} else if e != nil {
			return e
		}
		if b != nil && typ == TaskTelegramDeleteAlbum {
			if b.IsDefault {
				return errors.New("固定相册不能删除")
			}
			if b.State != "deleting" && b.State != "deleted" {
				return ErrAlbumFrozen
			}
		} else if b != nil && b.State != "active" && !(typ == TaskTelegramRenameAlbum && b.State == "creating") {
			return ErrAlbumFrozen
		}
		if typ == TaskTelegramReceive {
			if b == nil || fileID.String != TelegramAlbumTaskKey(p.AlbumID) || b.AccountID != p.AccountID || b.ChatID != p.ChatID || p.MessageID <= 0 {
				return errors.New("接收消息与当前相册绑定不一致")
			}
			var topicID int
			if err = tx.QueryRow(`SELECT topic_id FROM tg_messages WHERE account_id=? AND chat_id=? AND message_id=?`, p.AccountID, p.ChatID, p.MessageID).Scan(&topicID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			if topicID != b.TopicID {
				return errors.New("接收消息与当前相册话题不一致")
			}
		}
	}
	if !albumTask && fileID.Valid && fileID.String != "" {
		if typ == TaskConvert || typ == TaskTransferOwner || typ == TaskPurgeFile {
			var moving bool
			if err = tx.QueryRow(telegramMoveActiveSQL, fileID.String, fileID.String).Scan(&moving); err != nil {
				return err
			}
			if moving {
				return ErrTelegramMoving
			}
		}
		var purge sql.NullString
		var hidden bool
		var storageMode string
		var mediaVersion int
		if err := tx.QueryRow(`SELECT purge_state,hidden,storage,media_version FROM files WHERE id=?`, fileID.String).Scan(&purge, &hidden, &storageMode, &mediaVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if typ == TaskPublish && mediaVersion != 2 && storageMode != "direct" {
			return ErrPermanentEncrypted
		}
		if hidden && typ != TaskCleanupConverted && typ != TaskColdDelete {
			return ErrNotFound
		}
		if typ != TaskConvert && typ != TaskTransferOwner && typ != TaskCleanupConverted && typ != TaskUnpublish {
			var conversionCount int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?)`, fileID.String, fileID.String).Scan(&conversionCount); err != nil {
				return err
			}
			if conversionCount != 0 {
				return ErrConverting
			}
		}
		if typ != TaskPurgeFile && purge.String != "" {
			return ErrDeleting
		}
		if typ == TaskPurgeFile {
			var references bool
			if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE file_id=? AND id!=? AND state IN ('pending','running','failed'))`, fileID.String, id).Scan(&references); err != nil {
				return err
			}
			if references {
				return ErrTaskReferenced
			}
			if _, err := tx.Exec(`UPDATE files SET purge_state='pending',updated_at=? WHERE id=?`, now(), fileID.String); err != nil {
				return err
			}
		}
	}
	res, err := tx.Exec(`UPDATE tasks SET state='pending',cancel_requested=0,attempts=0,last_error=NULL,result=NULL,run_after=?,updated_at=?,lease_until=NULL WHERE id=? AND state IN ('failed','pending','canceled')`, now(), now(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("任务正在执行或已结束，不能重试")
	}
	return nil
}
