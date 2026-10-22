package dbx

import "errors"

// The empty check and deletion share a transaction so concurrent intake cannot
// turn an empty-album action into destructive file deletion.
func (db *DB) DeleteEmptyOwnerAlbum(owner string, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var storedOwner string
	var fixed bool
	var count, bindings int
	err = tx.QueryRow(`SELECT owner_id,is_default OR is_home,(SELECT COUNT(*) FROM files WHERE album_id=a.id),(SELECT COUNT(*) FROM album_topics WHERE album_id=a.id) FROM albums a WHERE id=?`, id).Scan(&storedOwner, &fixed, &count, &bindings)
	if err != nil || storedOwner != owner {
		return ErrNotFound
	}
	if fixed || bindings != 0 {
		return errors.New("固定或已关联的相册不能直接删除")
	}
	if count != 0 {
		return errors.New("请先移走或删除相册内的图片")
	}
	var running bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE file_id=? AND type=? AND state='running')`, TelegramAlbumTaskKey(id), TaskTelegramRenameAlbum).Scan(&running); err != nil {
		return err
	}
	if running {
		return errors.New("相册归档正在初始化，请稍后重试删除")
	}
	if _, err = tx.Exec(`UPDATE tasks SET state='canceled',updated_at=? WHERE file_id=? AND type=? AND state='pending'`, now(), TelegramAlbumTaskKey(id), TaskTelegramRenameAlbum); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM albums WHERE id=? AND owner_id=?`, id, owner); err != nil {
		return err
	}
	return tx.Commit()
}
