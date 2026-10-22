package dbx

import (
	"database/sql"
	"errors"
	"strconv"
)

func recordFollowupOperationItem(tx *sql.Tx, operation int64, itemKey, fileID, name, state string) error {
	// Automatic waves have no browser-declared input count. Grow only their
	// boundary; an explicit user operation keeps its original capacity guard.
	if _, err := tx.Exec(`UPDATE task_operations SET expected_files=MAX(expected_files,(SELECT COUNT(*)+1 FROM task_operation_items WHERE operation_id=?))
 WHERE id=? AND request_key='' AND NOT EXISTS(SELECT 1 FROM task_operation_items WHERE operation_id=? AND item_key=?)`, operation, operation, operation, itemKey); err != nil {
		return err
	}
	return recordTaskOperationItem(tx, operation, itemKey, fileID, name, state, "")
}

// ensureFollowupOperation gives an older standalone job a durable operation
// before it creates a follow-up. Existing membership always wins: a later phase
// must not detach work from the operation the user already sees or canceled.
func ensureFollowupOperation(tx *sql.Tx, parentID int64, kind string) (int64, string, error) {
	var operation int64
	var itemKey string
	err := tx.QueryRow(`SELECT operation_id,item_key FROM task_operation_jobs WHERE task_id=?`, parentID).Scan(&operation, &itemKey)
	if err == nil {
		return operation, itemKey, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	var fileID, state, typ string
	var created int64
	if err = tx.QueryRow(`SELECT COALESCE(file_id,''),state,created_at,type FROM tasks WHERE id=?`, parentID).Scan(&fileID, &state, &created, &typ); err != nil {
		return 0, "", err
	}
	itemKey = fileID
	if itemKey == "" {
		itemKey = "task:" + strconv.FormatInt(parentID, 10)
	}
	if typ == TaskTelegramScan {
		// A history scan is the intake boundary, not an extra media file.
		// Each successfully accepted archive supplies its own item below.
		itemKey = ""
	}
	r, err := tx.Exec(`INSERT INTO task_operations(kind,scope,created_at,updated_at) VALUES(?,?,?,?)`, kind, "task:"+strconv.FormatInt(parentID, 10), created, now())
	if err != nil {
		return 0, "", err
	}
	operation, err = r.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?)`, parentID, operation, itemKey); err != nil {
		return 0, "", err
	}
	if typ == TaskTelegramScan {
		return operation, itemKey, nil
	}
	var name string
	if fileID != "" {
		if err = tx.QueryRow(`SELECT COALESCE(NULLIF(orig_name,''),name) FROM files WHERE id=?`, fileID).Scan(&name); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", err
		}
	}
	if state == "pending" || state == "running" {
		state = "queued"
	}
	if err = recordFollowupOperationItem(tx, operation, itemKey, fileID, name, state); err != nil {
		return 0, "", err
	}
	return operation, itemKey, nil
}

func attachTaskFollowup(tx *sql.Tx, childID, parentID int64, kind string) error {
	// In particular, a deduplicated cleanup from an earlier generation retains
	// its original cancellation/retry boundary rather than moving to a new wave.
	var existing int64
	err := tx.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, childID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	operation, itemKey, err := ensureFollowupOperation(tx, parentID, kind)
	if err != nil {
		return err
	}
	var childType, fileID, name string
	if err = tx.QueryRow(`SELECT t.type,COALESCE(t.file_id,''),COALESCE(NULLIF(f.orig_name,''),f.name,'') FROM tasks t LEFT JOIN files f ON f.id=t.file_id WHERE t.id=?`, childID).Scan(&childType, &fileID, &name); err != nil {
		return err
	}
	if (childType == TaskArchive || childType == TaskDirectArchive) && name != "" {
		var parentType string
		if err = tx.QueryRow(`SELECT type FROM tasks WHERE id=?`, parentID).Scan(&parentType); err != nil {
			return err
		}
		if itemKey == "" || parentType == TaskTelegramScan {
			itemKey = fileID
		}
		if err = recordFollowupOperationItem(tx, operation, itemKey, fileID, name, "queued"); err != nil {
			return err
		}
		// Older parents may carry a serialization key instead of a file id.
		// Replace only absent locators, while retaining the first useful name.
		if _, err = tx.Exec(`UPDATE task_operation_items SET file_id=?,name=CASE WHEN name='' THEN ? ELSE name END,updated_at=? WHERE operation_id=? AND item_key=? AND NOT EXISTS(SELECT 1 FROM files f WHERE f.id=task_operation_items.file_id)`, fileID, name, now(), operation, itemKey); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?)`, childID, operation, itemKey); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_operations SET updated_at=? WHERE id=?`, now(), operation)
	return err
}

func attachColdDeleteFollowup(tx *sql.Tx, childID, repositoryID int64, fileID string) error {
	var parent int64
	var kind string
	err := tx.QueryRow(`SELECT parent_id,kind FROM (
 SELECT c.task_id parent_id,'convert' kind,1 priority FROM file_conversions c JOIN tasks t ON t.id=c.task_id WHERE c.source_id=? AND c.phase='done'
 UNION ALL SELECT s.task_id,'sync',2 FROM cold_sync_files s JOIN tasks t ON t.id=s.task_id WHERE s.repository_id=? AND s.file_id=?
 UNION ALL SELECT t.id,CASE WHEN t.type='local_layout' THEN 'maintenance' ELSE 'sync' END,3 FROM tasks t WHERE t.file_id=? AND t.type IN ('local_layout','tg_move_file') AND t.state IN ('pending','running'))
 ORDER BY priority,parent_id DESC LIMIT 1`, fileID, repositoryID, fileID, fileID).Scan(&parent, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		_, _, err = ensureFollowupOperation(tx, childID, "sync")
		return err
	}
	if err != nil {
		return err
	}
	return attachTaskFollowup(tx, childID, parent, kind)
}

// Push delivery is per message for crash recovery, but consecutive arrivals
// while this album is still receiving belong to one visible operation. Another
// album, a finished wave or a canceled wave always starts a separate operation.
func attachTelegramReceiveOperation(tx *sql.Tx, taskID, albumID int64, message *TelegramMessage) error {
	scope := "album:" + strconv.FormatInt(albumID, 10)
	var operation int64
	err := tx.QueryRow(`SELECT o.id FROM task_operations o WHERE o.kind='telegram_receive' AND o.scope=?
 AND EXISTS(SELECT 1 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=o.id AND t.state IN ('pending','running') AND t.cancel_requested=0)
 AND NOT EXISTS(SELECT 1 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=o.id AND t.cancel_requested=1)
 ORDER BY o.id DESC LIMIT 1`, scope).Scan(&operation)
	if errors.Is(err, sql.ErrNoRows) {
		r, e := tx.Exec(`INSERT INTO task_operations(kind,scope,created_at,updated_at) VALUES('telegram_receive',?,?,?)`, scope, now(), now())
		if e != nil {
			return e
		}
		operation, err = r.LastInsertId()
	}
	if err != nil {
		return err
	}
	itemKey := "message:" + strconv.FormatInt(int64(message.MessageID), 10)
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?)`, taskID, operation, itemKey); err != nil {
		return err
	}
	if err = recordFollowupOperationItem(tx, operation, itemKey, "", message.Name, "queued"); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_operations SET updated_at=? WHERE id=?`, now(), operation)
	return err
}

// AttachTaskFollowup is used after a resumable service phase has accepted its
// child job. It changes only operation membership, never the child's lease,
// cancellation or retry state.
func (db *DB) AttachTaskFollowup(childID, parentID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var typ string
	if err = tx.QueryRow(`SELECT type FROM tasks WHERE id=?`, parentID).Scan(&typ); err != nil {
		return err
	}
	kind := "maintenance"
	switch typ {
	case TaskTransferOwner:
		kind = "move_album"
	case TaskConvert, TaskCleanupConverted:
		kind = "convert"
	case TaskColdSync, TaskArchiveSync:
		kind = "sync"
	case TaskTelegramReceive, TaskTelegramScan:
		kind = "telegram_receive"
	}
	if err = attachTaskFollowup(tx, childID, parentID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

// EnqueueMaintenanceTasks commits one selected maintenance wave atomically.
// Recovery rows remain per file; repeated reconciliation cannot duplicate an
// active or failed item or attach it to a later wave.
func (db *DB) EnqueueMaintenanceTasks(typ string, fileIDs []string) error {
	if typ != "local_layout" || len(fileIDs) > 100 {
		return errors.New("维护任务类型或批次大小无效")
	}
	if len(fileIDs) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE tasks SET updated_at=updated_at WHERE 0`); err != nil {
		return err
	}
	var parent int64
	for _, fileID := range fileIDs {
		r, e := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,max_attempts,run_after,created_at,updated_at)
 SELECT ?,id,'{}','pending',1000000,?,?,? FROM files f WHERE id=? AND archive_state='done' AND purge_state IS NULL
 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.type=? AND t.file_id=f.id AND t.state IN ('pending','running','failed'))`, typ, now(), now(), now(), fileID, typ)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		id, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if parent == 0 {
			parent = id
			if _, _, e = ensureFollowupOperation(tx, parent, "maintenance"); e != nil {
				return e
			}
			continue
		}
		// A maintenance wave has one item per file, unlike multiple phases of
		// the same file which deliberately inherit their parent's item key.
		var operation int64
		if e = tx.QueryRow(`SELECT operation_id FROM task_operation_jobs WHERE task_id=?`, parent).Scan(&operation); e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?)`, id, operation, fileID); e != nil {
			return e
		}
		var name string
		if e = tx.QueryRow(`SELECT COALESCE(NULLIF(orig_name,''),name) FROM files WHERE id=?`, fileID).Scan(&name); e != nil {
			return e
		}
		if e = recordFollowupOperationItem(tx, operation, fileID, fileID, name, "queued"); e != nil {
			return e
		}
	}
	return tx.Commit()
}
