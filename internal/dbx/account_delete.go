package dbx

import (
	"database/sql"
	"errors"
)

const TaskAccountDelete = "account_delete"

const accountDeleteSchema = `
ALTER TABLE accounts ADD COLUMN deleting INTEGER NOT NULL DEFAULT 0;
CREATE TABLE deleted_accounts(owner_id TEXT PRIMARY KEY);
CREATE TRIGGER deleting_account_file_insert BEFORE INSERT ON files
 WHEN EXISTS(SELECT 1 FROM accounts WHERE id=NEW.owner_id AND deleting=1) OR EXISTS(SELECT 1 FROM deleted_accounts WHERE owner_id=NEW.owner_id)
 BEGIN SELECT RAISE(ABORT,'账号已删除或正在删除'); END;
CREATE TRIGGER deleting_account_album_insert BEFORE INSERT ON albums
 WHEN EXISTS(SELECT 1 FROM accounts WHERE id=NEW.owner_id AND deleting=1) OR EXISTS(SELECT 1 FROM deleted_accounts WHERE owner_id=NEW.owner_id)
 BEGIN SELECT RAISE(ABORT,'账号已删除或正在删除'); END;
CREATE TRIGGER deleted_account_recreate BEFORE INSERT ON accounts
 WHEN EXISTS(SELECT 1 FROM deleted_accounts WHERE owner_id=NEW.id)
 BEGIN SELECT RAISE(ABORT,'账号标识已经删除，不能重新使用'); END;
`

func (db *DB) QueueAccountDelete(owner string) (int64, error) {
	if owner == AdminOwner {
		return 0, errors.New("不能删除管理员账号")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var transferring bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_conversions c JOIN tasks t ON t.id=c.task_id JOIN files f ON f.id=c.source_id OR f.id=c.target_id WHERE t.type='transfer_owner' AND c.phase!='done' AND f.owner_id=?)`, owner).Scan(&transferring); err != nil {
		return 0, err
	}
	if transferring {
		return 0, errors.New("该用户仍有归属转移，请完成或重试转移后再删除用户")
	}
	var name string
	if err = tx.QueryRow(`SELECT username FROM accounts WHERE id=?`, owner).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	for _, query := range []string{
		`UPDATE accounts SET disabled=1,deleting=1 WHERE id=?`,
		`DELETE FROM sessions WHERE owner_id=?`,
		`UPDATE api_tokens SET revoked_at=unixepoch() WHERE owner_id=? AND revoked_at IS NULL`,
		`UPDATE import_rules SET enabled=0 WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?)`,
	} {
		if _, err = tx.Exec(query, owner); err != nil {
			return 0, err
		}
	}
	var id int64
	err = tx.QueryRow(`SELECT id FROM tasks WHERE type=? AND file_id=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, TaskAccountDelete, "account-delete:"+owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		result, e := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',unixepoch(),unixepoch(),unixepoch())`, TaskAccountDelete, "account-delete:"+owner, JSONValue(map[string]string{"owner_id": owner, "username": name}))
		if e != nil {
			return 0, e
		}
		id, err = result.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Structural work keeps its recovery chain. Ordinary file jobs can be canceled
// because the account deletion itself now owns their recorded objects.
func (db *DB) CancelDeletingAccountFileTasks(owner string) error {
	_, err := db.Exec(`UPDATE tasks SET cancel_requested=1,state=CASE WHEN state='running' THEN state ELSE 'canceled' END,updated_at=unixepoch()
 WHERE file_id IN (SELECT id FROM files WHERE owner_id=?) AND state IN ('pending','running','failed')
 AND type NOT IN ('convert','cleanup_converted','tg_move_file','tg_delete_album')`, owner)
	return err
}

func (db *DB) FinishAccountDelete(owner string) error {
	if owner == AdminOwner {
		return errors.New("不能删除管理员账号")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deleting bool
	err = tx.QueryRow(`SELECT deleting FROM accounts WHERE id=?`, owner).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !deleting {
		return errors.New("用户没有提交删除")
	}
	var remaining int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM files WHERE owner_id=?`, owner).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return errors.New("用户文件尚未清理完成")
	}
	for _, query := range []string{
		`INSERT OR IGNORE INTO deleted_accounts(owner_id) VALUES(?)`,
		`DELETE FROM import_receipts WHERE rule_id IN (SELECT id FROM import_rules WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?))`,
		`DELETE FROM import_rules WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?)`,
		`DELETE FROM storage_album_topics WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?)`,
		`DELETE FROM cold_sync_albums WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?)`,
		`DELETE FROM album_topics WHERE album_id IN (SELECT id FROM albums WHERE owner_id=?)`,
		`DELETE FROM albums WHERE owner_id=?`,
		`DELETE FROM account_telegram WHERE owner_id=?`,
		`DELETE FROM file_name_counters WHERE owner_id=?`,
		`DELETE FROM sessions WHERE owner_id=?`,
		`DELETE FROM api_tokens WHERE owner_id=?`,
		`DELETE FROM accounts WHERE id=? AND deleting=1`,
	} {
		if _, err = tx.Exec(query, owner); err != nil {
			return err
		}
	}
	return tx.Commit()
}
