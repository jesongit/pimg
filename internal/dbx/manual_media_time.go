package dbx

import (
	"errors"
	"strconv"
)

func MediaDateScope(stamp int64) string { return "media_at:" + strconv.FormatInt(stamp, 10) }

// Keep the edit and its receipt in one transaction, preserving original capture
// metadata, file names, intake timestamps and every storage reference.
func (db *DB) SetMediaTimeInOperation(fileID string, stamp, operationID int64) error {
	if !ValidMediaTime(stamp) {
		return errors.New("文件日期超出支持范围")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return err
	}
	f, err := operationMutationFile(tx, fileID, operationID, "set_media_time", MediaDateScope(stamp))
	if err != nil {
		return err
	}
	var object bool
	if err = tx.QueryRow(`SELECT json_type(naming_json)='object' FROM files WHERE id=?`, fileID).Scan(&object); err != nil {
		return err
	}
	if !object {
		return errors.New("文件日期元数据无效，未修改")
	}
	if err = recordTaskOperationItem(tx, operationID, fileID, fileID, f.OrigName, "done", ""); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE files SET naming_json=json_set(naming_json,'$.manual_time',?),updated_at=? WHERE id=?`, stamp, now(), fileID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=?,expires_at=? WHERE id=?`, now(), now()+TaskOperationIntakeTTL, operationID); err != nil {
		return err
	}
	return tx.Commit()
}
