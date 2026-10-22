package dbx

import (
	"database/sql"
	"errors"
	"fmt"
)

const (
	TaskTelegramScan        = "tg_scan"
	TaskTelegramReceive     = "tg_receive"
	TaskTelegramDeleteAlbum = "tg_delete_album"
	TaskTelegramMoveFile    = "tg_move_file"
	TaskTelegramRenameAlbum = "tg_rename_album"
)

var ErrAlbumFrozen = errors.New("相册正在删除，已暂停写入，请等待迁移完成")
var ErrTelegramMoving = errors.New("文件正在迁移或仍有旧消息待清理，请先完成或重试迁移")
var ErrTelegramSourceRetired = errors.New("该 Telegram 来源已接收或删除，不能重复导入")

const telegramMoveActiveSQL = `SELECT EXISTS(SELECT 1 FROM tg_moves WHERE file_id=?) OR EXISTS(SELECT 1 FROM tasks WHERE file_id=? AND type='tg_move_file' AND state IN ('pending','running'))`

func (db *DB) TelegramMoveActive(fileID string) (bool, error) {
	var active bool
	err := db.QueryRow(telegramMoveActiveSQL, fileID, fileID).Scan(&active)
	return active, err
}

func (db *DB) GetAlbum(id int64) (*Album, error) {
	a := &Album{}
	err := db.QueryRow(`SELECT id,name,created_at,is_home,is_default,owner_id,displayed FROM albums WHERE id=?`, id).Scan(&a.ID, &a.Name, &a.CreatedAt, &a.IsHome, &a.IsDefault, &a.OwnerID, &a.Displayed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (db *DB) EnsureDefaultAlbum() (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`SELECT id FROM albums WHERE is_default=1 AND owner_id='admin:1'`).Scan(&id)
	if err == nil {
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO albums(name,created_at,is_default) VALUES('未分类',?,1)`, now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (db *DB) AlbumWritable(id int64) error {
	if id <= 0 {
		return nil
	}
	var state string
	err := db.QueryRow(`SELECT COALESCE(t.state,'active') FROM albums a LEFT JOIN album_topics t ON t.album_id=a.id WHERE a.id=?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "deleting" || state == "deleted" {
		return ErrAlbumFrozen
	}
	return nil
}

const topicCols = `album_id,account_id,chat_id,topic_id,name,create_name,is_default,state,delete_phase,last_error,scan_date,scan_message_id,scan_offset,scan_task_id`

func scanTopic(row interface{ Scan(...any) error }) (*TopicBinding, error) {
	b := &TopicBinding{}
	err := row.Scan(&b.AlbumID, &b.AccountID, &b.ChatID, &b.TopicID, &b.Name, &b.CreateName, &b.IsDefault, &b.State, &b.DeletePhase, &b.LastError, &b.ScanDate, &b.ScanMessageID, &b.ScanOffset, &b.ScanTaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
func (db *DB) GetTopicBinding(albumID int64) (*TopicBinding, error) {
	return scanTopic(db.QueryRow(`SELECT `+topicCols+` FROM album_topics WHERE album_id=?`, albumID))
}
func (db *DB) TopicBindingBySource(account, chat int64, topic int) (*TopicBinding, error) {
	return scanTopic(db.QueryRow(`SELECT `+topicCols+` FROM album_topics WHERE account_id=? AND chat_id=? AND topic_id=?`, account, chat, topic))
}
func (db *DB) ListTopicBindings() ([]*TopicBinding, error) {
	rows, err := db.Query(`SELECT ` + topicCols + ` FROM album_topics ORDER BY album_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TopicBinding
	for rows.Next() {
		b, e := scanTopic(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (db *DB) InsertTopicBinding(b *TopicBinding) error {
	_, err := db.Exec(`INSERT INTO album_topics(album_id,account_id,chat_id,topic_id,name,create_name,is_default,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(album_id) DO NOTHING`, b.AlbumID, b.AccountID, b.ChatID, b.TopicID, b.Name, b.CreateName, b.IsDefault, b.State, now(), now())
	return err
}

// CreateLinkedTelegramAlbum commits the local intent and its synchronization
// task together. A failed remote call can never leave an unowned empty album.
func (db *DB) CreateLinkedTelegramAlbum(name string, account, chat int64, createName string) (int64, int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO albums(name,created_at) VALUES(?,?)`, name, now())
	if err != nil {
		return 0, 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if _, err = tx.Exec(`INSERT INTO album_topics(album_id,account_id,chat_id,name,create_name,state,created_at,updated_at) VALUES(?,?,?,?,?,'creating',?,?)`, id, account, chat, name, createName, now(), now()); err != nil {
		return 0, 0, err
	}
	r, err = tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTelegramRenameAlbum, TelegramAlbumTaskKey(id), TaskPayloadJSON(map[string]any{"album_id": id, "name": name}), now(), now(), now())
	if err != nil {
		return 0, 0, err
	}
	taskID, err := r.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	return id, taskID, tx.Commit()
}

// CreateAlbumFromTelegramTopic validates uniqueness before writing either row.
// Existing topic names are authoritative, as with binding an existing album.
func (db *DB) CreateAlbumFromTelegramTopic(b *TopicBinding) (int64, error) {
	if b.TopicID <= 1 {
		return 0, errors.New("请选择独立话题，不能绑定 General")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM album_topics WHERE account_id=? AND chat_id=? AND (topic_id=? OR (state='creating' AND create_name=?))`, b.AccountID, b.ChatID, b.TopicID, b.Name).Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 {
		return 0, errors.New("该 Telegram 话题已经绑定或正在创建，不能重复绑定")
	}
	r, err := tx.Exec(`INSERT INTO albums(name,created_at) VALUES(?,?)`, b.Name, now())
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO album_topics(album_id,account_id,chat_id,topic_id,name,create_name,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'active',?,?)`, id, b.AccountID, b.ChatID, b.TopicID, b.Name, b.Name, now(), now()); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (db *DB) LatestTelegramAlbumTask(albumID int64, typ string) (*Task, error) {
	return scanTaskRow(db.QueryRow(`SELECT id,type,file_id,payload,state,attempts,max_attempts,last_error,result,run_after,lease_until,created_at,updated_at,done_at FROM tasks WHERE file_id=? AND type=? ORDER BY id DESC LIMIT 1`, TelegramAlbumTaskKey(albumID), typ))
}

// BindExistingTelegramTopic performs the album and source uniqueness checks in
// the same transaction. Existing source history stays paused until date setup.
func (db *DB) BindExistingTelegramTopic(b *TopicBinding) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var fixed bool
	if err = tx.QueryRow(`SELECT is_home OR is_default FROM albums WHERE id=?`, b.AlbumID).Scan(&fixed); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if fixed {
		return errors.New("固定相册使用专用话题，不能绑定其他话题")
	}
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM album_topics WHERE album_id=? OR (account_id=? AND chat_id=? AND (topic_id=? OR (state='creating' AND create_name=?)))`, b.AlbumID, b.AccountID, b.ChatID, b.TopicID, b.Name).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("该相册或 Telegram 话题已经绑定，不能重复绑定")
	}
	if _, err = tx.Exec(`INSERT INTO album_topics(album_id,account_id,chat_id,topic_id,name,create_name,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'active',?,?)`, b.AlbumID, b.AccountID, b.ChatID, b.TopicID, b.Name, b.Name, now(), now()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE albums SET name=? WHERE id=?`, b.Name, b.AlbumID); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) ActivateTopicBinding(albumID int64, topicID int) error {
	if topicID <= 1 {
		return errors.New("必须使用独立的归档话题，不能绑定 General")
	}
	r, err := db.Exec(`UPDATE album_topics SET topic_id=?,state='active',last_error='',scan_date=CASE WHEN scan_date=0 THEN ? ELSE scan_date END,updated_at=? WHERE album_id=? AND state='creating'`, topicID, now(), now(), albumID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrAlbumFrozen
	}
	return nil
}
func (db *DB) SetTopicError(albumID int64, message string) error {
	_, err := db.Exec(`UPDATE album_topics SET last_error=?,updated_at=? WHERE album_id=?`, message, now(), albumID)
	return err
}
func (db *DB) SetTopicDeletePhase(albumID int64, phase string) error {
	_, err := db.Exec(`UPDATE album_topics SET delete_phase=?,updated_at=? WHERE album_id=? AND state='deleting'`, phase, now(), albumID)
	return err
}
func (db *DB) MarkTopicMissing(albumID int64) error {
	_, err := db.Exec(`UPDATE album_topics SET state='missing',last_error='Telegram 话题已不存在；已保留图片索引',updated_at=? WHERE album_id=? AND state='active'`, now(), albumID)
	return err
}
func (db *DB) FollowTelegramTopicName(albumID int64, name string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var busy int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=? AND file_id=? AND state IN ('pending','running')`, TaskTelegramRenameAlbum, TelegramAlbumTaskKey(albumID)).Scan(&busy); err != nil {
		return err
	}
	if busy > 0 {
		return nil
	}
	r, err := tx.Exec(`UPDATE albums SET name=? WHERE id=? AND is_home=0 AND is_default=0`, name, albumID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return nil
	}
	if _, err = tx.Exec(`UPDATE album_topics SET name=?,updated_at=? WHERE album_id=? AND state='active'`, name, now(), albumID); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) RenameLinkedAlbum(id int64, name string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var home, def bool
	var state string
	if err = tx.QueryRow(`SELECT a.is_home,a.is_default,COALESCE(t.state,'active') FROM albums a LEFT JOIN album_topics t ON t.album_id=a.id WHERE a.id=?`, id).Scan(&home, &def, &state); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if home || def {
		return errors.New("固定相册不能重命名")
	}
	if state != "active" {
		return ErrAlbumFrozen
	}
	if _, err = tx.Exec(`UPDATE albums SET name=? WHERE id=?`, name, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE album_topics SET name=?,updated_at=? WHERE album_id=?`, name, now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTelegramRenameAlbum, TelegramAlbumTaskKey(id), TaskPayloadJSON(map[string]any{"album_id": id, "name": name}), now(), now(), now()); err != nil {
		return err
	}
	return tx.Commit()
}

func TelegramAlbumTaskKey(id int64) string { return fmt.Sprintf("tg-album:%d", id) }

// QueueLinkedAlbumDelete freezes writers in the same transaction that creates
// the durable task. Readers and retries can still inspect the source binding.
func (db *DB) QueueLinkedAlbumDelete(albumID int64, payload string, emptyOwner ...string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if len(emptyOwner) > 0 {
		var owner string
		var count int
		if err = tx.QueryRow(`SELECT owner_id,(SELECT COUNT(*) FROM files WHERE album_id=albums.id) FROM albums WHERE id=?`, albumID).Scan(&owner, &count); err != nil || owner != emptyOwner[0] {
			return 0, ErrNotFound
		}
		if count != 0 {
			return 0, errors.New("请先移走或删除相册内的图片")
		}
	}
	var home, def bool
	var state string
	err = tx.QueryRow(`SELECT a.is_home,a.is_default,t.state FROM albums a JOIN album_topics t ON t.album_id=a.id WHERE a.id=?`, albumID).Scan(&home, &def, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if home || def {
		return 0, errors.New("固定相册不能删除")
	}
	if len(emptyOwner) > 0 && state != "active" && state != "deleting" {
		return 0, errors.New("相册归档正在初始化，请稍后重试删除")
	}
	var taskID int64
	err = tx.QueryRow(`SELECT id FROM tasks WHERE type=? AND file_id=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, TaskTelegramDeleteAlbum, TelegramAlbumTaskKey(albumID)).Scan(&taskID)
	if err == nil {
		return taskID, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if state == "deleted" {
		return 0, ErrNotFound
	}
	var incoming bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tg_moves WHERE album_id=?) OR EXISTS(SELECT 1 FROM tasks WHERE type=? AND state IN ('pending','running') AND CASE WHEN json_valid(payload) THEN json_extract(payload,'$.album_id') END=?)`, albumID, TaskTelegramMoveFile, albumID).Scan(&incoming); err != nil {
		return 0, err
	}
	if incoming {
		return 0, errors.New("相册仍有迁入或清理任务，请完成这些任务后再删除相册")
	}
	if err = ownerTransferAlbumGuard(tx, albumID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE album_topics SET state='deleting',last_error='',updated_at=? WHERE album_id=?`, now(), albumID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE tasks SET state='canceled',updated_at=? WHERE type=? AND file_id=? AND state='pending'`, now(), TaskTelegramScan, TelegramAlbumTaskKey(albumID)); err != nil {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTelegramDeleteAlbum, TelegramAlbumTaskKey(albumID), payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	taskID, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return taskID, tx.Commit()
}

func (db *DB) QueueLinkedAlbumScan(albumID int64, payload string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRow(`SELECT state FROM album_topics WHERE album_id=?`, albumID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	if state != "active" {
		return 0, ErrAlbumFrozen
	}
	var id int64
	var existing string
	err = tx.QueryRow(`SELECT id,payload FROM tasks WHERE type=? AND file_id=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, TaskTelegramScan, TelegramAlbumTaskKey(albumID)).Scan(&id, &existing)
	if err == nil {
		if !PassiveTask(&Task{Type: TaskTelegramScan, Payload: payload}) && PassiveTask(&Task{Type: TaskTelegramScan, Payload: existing}) {
			// Manual intent promotes an existing probe without resetting its
			// durable history cursor or replacing an unfinished date range.
			if _, err = tx.Exec(`UPDATE tasks SET payload=json_set(payload,'$.automatic',json('false')),updated_at=? WHERE id=?`, now(), id); err != nil {
				return 0, err
			}
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTelegramScan, TelegramAlbumTaskKey(albumID), payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`UPDATE album_topics SET scan_task_id=?,scan_offset=0,updated_at=? WHERE album_id=?`, id, now(), albumID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (db *DB) ResumeTelegramScan(albumID int64, automatic bool) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state string
	var id int64
	if err = tx.QueryRow(`SELECT t.id,t.state FROM album_topics b JOIN tasks t ON t.id=b.scan_task_id WHERE b.album_id=? AND b.state='active' AND t.type=?`, albumID, TaskTelegramScan).Scan(&id, &state); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	if state == "done" || state == "canceled" {
		return 0, ErrNotFound
	}
	if !automatic {
		if _, err = tx.Exec(`UPDATE tasks SET payload=json_set(payload,'$.automatic',json('false')),updated_at=? WHERE id=? AND json_valid(payload)`, now(), id); err != nil {
			return 0, err
		}
	}
	if state == "failed" {
		if _, err = tx.Exec(`UPDATE tasks SET state='pending',attempts=0,last_error=NULL,run_after=?,updated_at=?,lease_until=NULL WHERE id=?`, now(), now(), id); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func (db *DB) TelegramScanProgress(albumID, taskID int64, offset int, payload string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if taskID > 0 {
		query := `UPDATE tasks SET payload=?,updated_at=? WHERE id=?`
		args := []any{payload, now(), taskID}
		if PassiveTask(&Task{Type: TaskTelegramScan, Payload: payload}) {
			// A running probe may have been promoted by a manual request after
			// it loaded its payload. Never downgrade that intent at a checkpoint.
			query = `UPDATE tasks SET payload=CASE WHEN ` + passiveTaskSQL("tasks") + ` THEN ? ELSE json_set(?,'$.automatic',json('false')) END,updated_at=? WHERE id=?`
			args = []any{payload, payload, now(), taskID}
		}
		if _, err = tx.Exec(query, args...); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE album_topics SET scan_offset=?,updated_at=? WHERE album_id=?`, offset, now(), albumID); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) CompleteTelegramScan(albumID, date int64, messageID int) error {
	_, err := db.Exec(`UPDATE album_topics SET scan_date=MAX(scan_date,?),scan_message_id=CASE WHEN scan_date<? THEN ? WHEN scan_date=? THEN MAX(scan_message_id,?) ELSE scan_message_id END,scan_offset=0,last_error='',updated_at=? WHERE album_id=?`, date, date, messageID, date, messageID, now(), albumID)
	return err
}

func (db *DB) LinkedAlbumFileIDs(albumID int64) ([]string, error) {
	rows, err := db.Query(`SELECT id FROM files WHERE album_id=? AND hidden=0 ORDER BY created_at,id`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (db *DB) FinishLinkedAlbumDelete(albumID, destination int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM files WHERE album_id=? AND hidden=0`, albumID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("仍有图片尚未迁出，不能完成相册删除")
	}
	if _, err = tx.Exec(`UPDATE files SET album_id=? WHERE album_id=? AND hidden=1`, destination, albumID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE album_topics SET state='deleted',last_error='',updated_at=? WHERE album_id=? AND is_default=0`, now(), albumID); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM albums WHERE id=? AND is_default=0 AND is_home=0`, albumID); err != nil {
		return err
	}
	return tx.Commit()
}

const telegramMessageCols = `account_id,chat_id,message_id,topic_id,message_date,reference,name,mime,caption,is_service,state,file_id,last_error`

func scanTelegramMessage(row interface{ Scan(...any) error }) (*TelegramMessage, error) {
	m := &TelegramMessage{}
	err := row.Scan(&m.AccountID, &m.ChatID, &m.MessageID, &m.TopicID, &m.Date, &m.Reference, &m.Name, &m.MIME, &m.Caption, &m.IsService, &m.State, &m.FileID, &m.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}
func (db *DB) GetTelegramMessage(account, chat int64, messageID int) (*TelegramMessage, error) {
	return scanTelegramMessage(db.QueryRow(`SELECT `+telegramMessageCols+` FROM tg_messages WHERE account_id=? AND chat_id=? AND message_id=?`, account, chat, messageID))
}

const upsertTelegramMessageSQL = `INSERT INTO tg_messages(account_id,chat_id,message_id,topic_id,message_date,reference,name,mime,caption,is_service,state,file_id,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(account_id,chat_id,message_id) DO UPDATE SET topic_id=excluded.topic_id,message_date=MAX(tg_messages.message_date,excluded.message_date),reference=excluded.reference,name=excluded.name,mime=excluded.mime,caption=excluded.caption,is_service=excluded.is_service,
state=CASE WHEN tg_messages.state='retired' OR excluded.state='retired' THEN 'retired' WHEN excluded.state='managed' THEN 'managed' ELSE tg_messages.state END,
file_id=CASE WHEN tg_messages.state='retired' OR excluded.file_id='' THEN tg_messages.file_id ELSE excluded.file_id END,updated_at=excluded.updated_at`

func telegramMessageArgs(m *TelegramMessage) []any {
	if m.State == "" {
		m.State = "pending"
	}
	if m.Reference == "" {
		m.Reference = "{}"
	}
	return []any{m.AccountID, m.ChatID, m.MessageID, m.TopicID, m.Date, m.Reference, m.Name, m.MIME, m.Caption, m.IsService, m.State, m.FileID, m.LastError, now(), now()}
}
func (db *DB) UpsertTelegramMessage(m *TelegramMessage) error {
	_, err := db.Exec(upsertTelegramMessageSQL, telegramMessageArgs(m)...)
	return err
}
func (db *DB) SetTelegramMessageState(account, chat int64, messageID int, state, fileID, lastError string) error {
	_, err := db.Exec(`UPDATE tg_messages SET state=CASE WHEN state='retired' THEN 'retired' ELSE ? END,file_id=CASE WHEN ?='' THEN file_id ELSE ? END,last_error=?,updated_at=? WHERE account_id=? AND chat_id=? AND message_id=?`, state, fileID, fileID, lastError, now(), account, chat, messageID)
	return err
}
func (db *DB) TelegramMessagesForTopic(account, chat int64, topic int) ([]*TelegramMessage, error) {
	rows, err := db.Query(`SELECT `+telegramMessageCols+` FROM tg_messages WHERE account_id=? AND chat_id=? AND topic_id=? ORDER BY message_date,message_id`, account, chat, topic)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TelegramMessage
	for rows.Next() {
		m, e := scanTelegramMessage(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (db *DB) RetireTelegramFileMessages(fileID string) error {
	_, err := db.Exec(`UPDATE tg_messages SET state='retired',updated_at=? WHERE file_id=?`, now(), fileID)
	return err
}
func (db *DB) FileByTelegramSource(source string) (*File, error) {
	f, err := scanFile(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE telegram_source=?`, source))
	if !errors.Is(err, ErrNotFound) {
		return f, err
	}
	// Dedup sources point at a canonical file with a different telegram_source.
	f, err = scanFile(db.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=(SELECT file_id FROM telegram_source_parts WHERE source_key=? AND part_index=0 AND purged_at IS NULL)`, source))
	if !errors.Is(err, ErrNotFound) {
		return f, err
	}
	var count int
	if e := db.QueryRow(`SELECT COUNT(*) FROM telegram_source_parts WHERE source_key=?`, source).Scan(&count); e != nil {
		return nil, e
	}
	if count > 0 {
		return nil, ErrTelegramSourceRetired
	}
	return nil, err
}
func (db *DB) TelegramSourceParts(source string) ([]TelegramSourcePart, error) {
	rows, err := db.Query(`SELECT source_key,part_index,file_id,COALESCE(purged_at,0) FROM telegram_source_parts WHERE source_key=? ORDER BY part_index`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TelegramSourcePart
	for rows.Next() {
		var p TelegramSourcePart
		if err = rows.Scan(&p.SourceKey, &p.PartIndex, &p.FileID, &p.PurgedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (db *DB) TelegramSourcePartIDs(source string) ([]string, error) {
	parts, err := db.TelegramSourceParts(source)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.FileID)
	}
	return out, nil
}
func (db *DB) LatestTelegramSourceDate(account, chat int64, topic int) (int64, error) {
	var date int64
	err := db.QueryRow(`SELECT COALESCE(MAX(message_date),0) FROM tg_messages WHERE account_id=? AND chat_id=? AND topic_id=? AND state='done'`, account, chat, topic).Scan(&date)
	return date, err
}

func (db *DB) GetTelegramMove(fileID string) (*TelegramMove, error) {
	m := &TelegramMove{}
	err := db.QueryRow(`SELECT file_id,album_id,phase,refs,last_error,created_at,updated_at FROM tg_moves WHERE file_id=?`, fileID).Scan(&m.FileID, &m.AlbumID, &m.Phase, &m.References, &m.LastError, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}
func (db *DB) SaveTelegramMove(m *TelegramMove) error {
	if m.Phase == "" {
		m.Phase = "prepare"
	}
	if m.References == "" {
		m.References = "[]"
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now()
	}
	m.UpdatedAt = now()
	result, err := db.Exec(`INSERT INTO tg_moves(file_id,album_id,phase,refs,last_error,created_at,updated_at)
SELECT ?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))
AND NOT EXISTS(SELECT 1 FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?))
ON CONFLICT(file_id) DO UPDATE SET album_id=excluded.album_id,phase=excluded.phase,refs=excluded.refs,last_error=excluded.last_error,updated_at=excluded.updated_at`, m.FileID, m.AlbumID, m.Phase, m.References, m.LastError, m.CreatedAt, m.UpdatedAt, m.AlbumID, m.FileID, m.FileID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrAlbumFrozen
	}
	return nil
}

// EnqueueTelegramMove closes the race between target readiness and an album's
// deletion freeze. Direct source/target mutations are validated in SQLite.
func (db *DB) EnqueueTelegramMove(fileID string, albumID int64, operationIDs ...int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at)
SELECT ?,?,?, 'pending',?,?,? FROM files f WHERE f.id=? AND f.hidden=0 AND f.purge_state IS NULL
AND EXISTS(SELECT 1 FROM albums WHERE id=?)
AND NOT EXISTS(SELECT 1 FROM album_topics WHERE (album_id=? OR album_id=f.album_id) AND state IN ('deleting','deleted'))
AND NOT EXISTS(SELECT 1 FROM file_conversions WHERE phase!='done' AND (source_id=f.id OR target_id=f.id))`, TaskTelegramMoveFile, fileID, TaskPayloadJSON(map[string]any{"album_id": albumID}), now(), now(), now(), fileID, albumID, albumID)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return 0, ErrAlbumFrozen
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachTaskOperation(tx, id, TaskTelegramMoveFile, fileID, TaskPayloadJSON(map[string]any{"album_id": albumID}), operationIDs...); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
func (db *DB) DeleteTelegramMove(fileID string) error {
	_, err := db.Exec(`DELETE FROM tg_moves WHERE file_id=?`, fileID)
	return err
}
