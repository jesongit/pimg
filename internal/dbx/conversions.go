package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"pimg/internal/filenames"
	"strconv"
	"strings"
)

var ErrConversionNotNeeded = errors.New("已经符合目标加密方式，无需转换")

func ConversionScope(encrypted bool) string { return "encrypted:" + strconv.FormatBool(encrypted) }

const conversionCols = `c.source_id,c.target_id,c.target_encrypted,c.phase,c.task_id,
CASE WHEN c.phase='done' THEN 'done' ELSE t.state END,
CASE WHEN c.last_error!='' THEN c.last_error ELSE COALESCE(t.last_error,'') END,c.cleanup_pending,c.cleanup_error,
t.type,COALESCE((SELECT owner_id FROM files WHERE id=c.target_id),''),COALESCE((SELECT album_id FROM files WHERE id=c.target_id),0)`

func scanConversion(row interface{ Scan(...any) error }) (*Conversion, error) {
	c := &Conversion{}
	if err := row.Scan(&c.SourceID, &c.TargetID, &c.TargetEncrypted, &c.Phase, &c.TaskID, &c.State, &c.LastError, &c.CleanupPending, &c.CleanupError, &c.Kind, &c.TargetOwnerID, &c.TargetAlbumID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

func (db *DB) GetConversion(sourceID string) (*Conversion, error) {
	return scanConversion(db.QueryRow(`SELECT `+conversionCols+` FROM file_conversions c JOIN tasks t ON t.id=c.task_id WHERE c.source_id=?`, sourceID))
}

func (db *DB) FileConversions(ids []string) (map[string]*Conversion, error) {
	out := make(map[string]*Conversion)
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(`SELECT `+conversionCols+` FROM file_conversions c JOIN tasks t ON t.id=c.task_id WHERE c.source_id IN (`+strings.TrimRight(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanConversion(rows)
		if err != nil {
			return nil, err
		}
		out[c.SourceID] = c
	}
	return out, rows.Err()
}

func (db *DB) ConversionActive(id string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?)`, id, id).Scan(&count)
	return count != 0, err
}

// BeginConversion commits the hidden target, durable conversion state and task
// together. Existing work resumes its current phase instead of starting over.
func (db *DB) BeginConversion(sourceID, targetID string, encrypted bool, wrapped, nonce []byte, cryptoVersion int, mediaPolicies ...MediaReplica) (*Conversion, error) {
	return db.BeginConversionInOperation(sourceID, targetID, encrypted, wrapped, nonce, cryptoVersion, 0, mediaPolicies...)
}

func (db *DB) BeginConversionInOperation(sourceID, targetID string, encrypted bool, wrapped, nonce []byte, cryptoVersion int, operationID int64, mediaPolicies ...MediaReplica) (*Conversion, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return nil, err
	}
	var moving bool
	if err = tx.QueryRow(telegramMoveActiveSQL, sourceID, sourceID).Scan(&moving); err != nil {
		return nil, err
	}
	if moving {
		return nil, ErrTelegramMoving
	}
	if c, err := scanConversion(tx.QueryRow(`SELECT `+conversionCols+` FROM file_conversions c JOIN tasks t ON t.id=c.task_id WHERE c.source_id=?`, sourceID)); err == nil {
		if c.Kind != TaskConvert || c.Phase == "done" || c.TargetEncrypted != encrypted {
			return nil, ErrConverting
		}
		if operationID != 0 {
			var existing int64
			var kind, scope, input string
			var expiry int64
			if err = tx.QueryRow(`SELECT j.operation_id,o.kind,o.scope,o.input_state,o.expires_at FROM task_operation_jobs j JOIN task_operations o ON o.id=j.operation_id WHERE j.task_id=?`, c.TaskID).Scan(&existing, &kind, &scope, &input, &expiry); err != nil {
				return nil, err
			}
			if existing != operationID || kind != "convert" || scope != ConversionScope(encrypted) {
				return nil, ErrTaskOperationConflict
			}
			if input != "open" || expiry <= now() {
				return nil, ErrTaskOperationClosed
			}
		}
		if c.State != "running" {
			if _, err := tx.Exec(`UPDATE tasks SET state='pending',attempts=0,last_error=NULL,result=NULL,run_after=?,updated_at=?,lease_until=NULL WHERE id=?`, now(), now(), c.TaskID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE file_conversions SET last_error='',updated_at=? WHERE source_id=?`, now(), sourceID); err != nil {
				return nil, err
			}
			c.State, c.LastError = "pending", ""
		}
		return c, tx.Commit()
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var f *File
	if operationID != 0 {
		f, err = operationMutationFile(tx, sourceID, operationID, "convert", ConversionScope(encrypted))
	} else {
		f, err = scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, sourceID))
	}
	if err != nil {
		return nil, err
	}
	if f.Hidden {
		return nil, ErrNotFound
	}
	if f.PurgeState.String != "" {
		return nil, ErrDeleting
	}
	var frozen bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`, f.AlbumID).Scan(&frozen); err != nil {
		return nil, err
	}
	if frozen {
		return nil, ErrAlbumFrozen
	}
	if f.ArchiveState != "done" {
		return nil, errors.New("归档尚未就绪")
	}
	if f.MediaVersion != 2 && (f.Storage != "direct") == encrypted {
		return nil, ErrConversionNotNeeded
	}
	if f.MediaVersion == 2 && len(mediaPolicies) == 0 {
		return nil, errors.New("缺少待转换的归档副本策略")
	}
	var running int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=? AND state='running'`, sourceID).Scan(&running); err != nil {
		return nil, err
	}
	if running != 0 {
		return nil, errors.New("该文件另有任务正在执行，请完成后重试")
	}
	if _, err := tx.Exec(`UPDATE tasks SET state='canceled',updated_at=? WHERE file_id=? AND type!='unpublish' AND state='pending'`, now(), sourceID); err != nil {
		return nil, err
	}
	result, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('convert',?,'{}','pending',?,?,?)`, sourceID, now(), now(), now())
	if err != nil {
		return nil, err
	}
	taskID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if operationID != 0 {
		err = AttachTaskOperationItem(tx, taskID, TaskConvert, sourceID, "{}", operationID, sourceID)
	} else {
		_, _, err = ensureFollowupOperation(tx, taskID, "convert")
	}
	if err != nil {
		return nil, err
	}
	mode := "direct"
	if encrypted {
		mode = "tg"
	}
	targetName := f.Name
	if old := filenames.Stem(f.Kind, f.Storage != "direct", f.Naming); f.MediaVersion != 2 && old != "" && old == targetName {
		targetName = filenames.Stem(f.Kind, encrypted, f.Naming)
	}
	if _, err := tx.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,width,height,duration,key_wrapped,key_nonce,crypto_ver,album_id,archive_state,storage,cold_backend,hot_backend,hidden,created_at,updated_at,naming_json,owner_id,media_version,has_thumb,thumb_w,thumb_h,thumb_mime,thumb_size,has_bthumb,home_visibility)
SELECT ?,?,orig_name,ext,mime,size,sha256,kind,width,height,duration,?,?,?,album_id,'pending',?,cold_backend,'',1,created_at,?,naming_json,owner_id,media_version,has_thumb,thumb_w,thumb_h,thumb_mime,thumb_size,has_bthumb,home_visibility FROM files WHERE id=?`, targetID, targetName, wrapped, nonce, cryptoVersion, mode, now(), sourceID); err != nil {
		return nil, err
	}
	for _, r := range mediaPolicies {
		policy := JSONValue(map[string]any{"encrypted": r.Encrypted, "split": r.Split, "part_mb": r.PartMB})
		if _, err = tx.Exec(`INSERT INTO media_replicas(file_id,instance_id,policy) VALUES(?,?,?)`, targetID, r.InstanceID, policy); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO file_conversions(source_id,target_id,target_encrypted,phase,task_id,created_at,updated_at) VALUES(?,?,?,'prepare',?,?,?)`, sourceID, targetID, encrypted, taskID, now(), now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetConversion(sourceID)
}

func (db *DB) ConversionPhase(sourceID, phase string) error {
	_, err := db.Exec(`UPDATE file_conversions SET phase=?,last_error='',updated_at=? WHERE source_id=?`, phase, now(), sourceID)
	return err
}

func (db *DB) ConversionError(sourceID, message string) error {
	_, err := db.Exec(`UPDATE file_conversions SET last_error=?,updated_at=? WHERE source_id=?`, message, now(), sourceID)
	return err
}

func (db *DB) CompleteConversion(sourceID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var targetID string
	var taskID int64
	if err := tx.QueryRow(`SELECT target_id,task_id FROM file_conversions WHERE source_id=? AND phase IN ('archive','cleanup')`, sourceID).Scan(&targetID, &taskID); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(`SELECT type FROM tasks WHERE id=?`, taskID).Scan(&kind); err != nil {
		return err
	}
	if kind == TaskTransferOwner {
		if err := validateOwnerTransfer(tx, sourceID, targetID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE files SET hidden=1,published=0,published_at=NULL,show_home=0,updated_at=? WHERE id=?`, now(), sourceID); err != nil {
		return err
	}
	promoted, err := tx.Exec(`UPDATE files SET hidden=0,published=0,published_path=NULL,published_at=NULL,show_home=0,updated_at=? WHERE id=? AND archive_state='done'`, now(), targetID)
	if err != nil {
		return err
	}
	if count, err := promoted.RowsAffected(); err != nil || count != 1 {
		return errors.New("已校验的新归档记录缺失，未切换")
	}
	if _, err := tx.Exec(`UPDATE file_conversions SET phase='done',last_error='',cleanup_pending=1,cleanup_error='',updated_at=? WHERE source_id=?`, now(), sourceID); err != nil {
		return err
	}
	if kind != TaskTransferOwner {
		if _, err := tx.Exec(`UPDATE telegram_source_parts SET file_id=? WHERE file_id=? AND purged_at IS NULL`, targetID, sourceID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE tasks SET state='canceled',updated_at=? WHERE file_id=? AND type='unpublish' AND state='pending'`, now(), sourceID); err != nil {
		return err
	}
	cleanup, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('cleanup_converted',?,'{}','pending',?,?,?)`, sourceID, now(), now(), now())
	if err != nil {
		return err
	}
	cleanupID, err := cleanup.LastInsertId()
	if err != nil {
		return err
	}
	operationKind := "convert"
	if kind == TaskTransferOwner {
		operationKind = "move_album"
	}
	if err = attachTaskFollowup(tx, cleanupID, taskID, operationKind); err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]any{"new_file_id": targetID, "cleanup_pending": true})
	if _, err := tx.Exec(`UPDATE tasks SET state='done',result=?,last_error=NULL,done_at=?,updated_at=?,lease_until=NULL WHERE id=?`, string(result), now(), now(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) ConversionCleanup(sourceID string, pending bool, message string) error {
	_, err := db.Exec(`UPDATE file_conversions SET cleanup_pending=?,cleanup_error=?,updated_at=? WHERE source_id=? AND phase='done'`, pending, message, now(), sourceID)
	return err
}

// QueueEncryptedUnpublishes withdraws legacy plaintext serving copies without
// altering their encrypted cold originals or legitimate temporary copies.
func (db *DB) QueueEncryptedUnpublishes() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tasks SET state='canceled',last_error=?,updated_at=? WHERE type='publish' AND state='pending' AND file_id IN (SELECT id FROM files WHERE storage!='direct')`, ErrPermanentEncrypted.Error(), now()); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at)
SELECT 'unpublish',f.id,'{}','pending',?,?,? FROM files f
WHERE f.hidden=0 AND f.storage!='direct' AND f.purge_state IS NULL
AND (f.published=1 OR f.hot_backend!='')
AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.file_id=f.id AND t.type='unpublish' AND t.state IN ('pending','running'))`, now(), now(), now())
	if err != nil {
		return err
	}
	return tx.Commit()
}
