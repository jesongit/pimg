package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

// First-copy archival ends the upload/import operation. Replica work has a
// separate progress/cancellation boundary shared by every store for that input.
// Only durable source identities are used here; timestamps never infer a batch.
func attachIntakeSyncOperation(tx *sql.Tx, taskID int64, typ, fileID, payload string) (bool, error) {
	if fileID == "" {
		return false, nil
	}
	var prior bool
	if typ == TaskColdSync {
		var p struct {
			RepositoryID int64 `json:"repository_id"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return false, err
		}
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE type=? AND file_id=? AND id!=? AND CASE WHEN json_valid(payload) THEN json_extract(payload,'$.repository_id') END=?)`, typ, fileID, taskID, p.RepositoryID).Scan(&prior); err != nil {
			return false, err
		}
	} else if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE type=? AND file_id=? AND id!=?)`, typ, fileID, taskID).Scan(&prior); err != nil {
		return false, err
	}
	if prior {
		// A later metadata/revision change is new synchronization work, not a
		// reopening of a possibly old, already completed upload/import result.
		return false, nil
	}
	var scope string
	var batch int64
	err := tx.QueryRow(`SELECT i.batch_id FROM local_import_items i WHERE i.state!='duplicate' AND (
 EXISTS(SELECT 1 FROM telegram_source_parts p WHERE p.file_id=? AND p.source_key='local-import:'||i.batch_id||':'||i.id)
 OR EXISTS(SELECT 1 FROM json_each(CASE WHEN json_valid(i.file_ids) THEN i.file_ids ELSE '[]' END) f WHERE f.type='text' AND f.value=?))
 ORDER BY i.id LIMIT 1`, fileID, fileID).Scan(&batch)
	if err == nil {
		scope = "after:import:" + strconv.FormatInt(batch, 10)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	} else {
		var parent int64
		err = tx.QueryRow(`SELECT o.id FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id JOIN task_operations o ON o.id=j.operation_id
 WHERE t.file_id=? AND t.type IN ('archive','direct_archive') AND o.kind IN ('upload','telegram_receive') ORDER BY t.id DESC LIMIT 1`, fileID).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		scope = "after:operation:" + strconv.FormatInt(parent, 10)
	}
	var owner, name string
	if err = tx.QueryRow(`SELECT owner_id,COALESCE(NULLIF(orig_name,''),name) FROM files WHERE id=?`, fileID).Scan(&owner, &name); err != nil {
		return false, err
	}
	var operation int64
	err = tx.QueryRow(`SELECT id FROM task_operations WHERE kind='sync' AND scope=? AND owner_id=? ORDER BY id LIMIT 1`, scope, owner).Scan(&operation)
	if errors.Is(err, sql.ErrNoRows) {
		r, e := tx.Exec(`INSERT INTO task_operations(kind,scope,owner_id,created_at,updated_at) VALUES('sync',?,?,?,?)`, scope, owner, now(), now())
		if e != nil {
			return false, e
		}
		operation, err = r.LastInsertId()
	}
	if err != nil {
		return false, err
	}
	// A canceled source wave includes later accepted files from that same
	// input. Keep their tombstones rather than silently scheduling a new wave.
	var canceled bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=? AND t.cancel_requested=1)`, operation).Scan(&canceled); err != nil {
		return false, err
	}
	state := "queued"
	if canceled {
		state = "canceled"
		if _, err = tx.Exec(`UPDATE tasks SET state='canceled',cancel_requested=1,done_at=?,updated_at=? WHERE id=? AND state='pending'`, now(), now(), taskID); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) VALUES(?,?,?)`, taskID, operation, fileID); err != nil {
		return false, err
	}
	if err = recordFollowupOperationItem(tx, operation, fileID, fileID, name, state); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=? WHERE id=?`, now(), operation); err != nil {
		return false, err
	}
	return true, nil
}
