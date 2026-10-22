package dbx

import (
	"database/sql"
	"errors"
)

// QueueTelegramMessage commits the inbox and its work together. The task key is
// per message so an arrival cannot be lost while another inbox task is finishing.
func (db *DB) QueueTelegramMessage(albumID int64, m *TelegramMessage) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var enabled bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND account_id=? AND chat_id=? AND topic_id=? AND state='active' AND (scan_date>0 OR scan_task_id>0))`, albumID, m.AccountID, m.ChatID, m.TopicID).Scan(&enabled); err != nil || !enabled {
		return 0, err
	}
	var state string
	err = tx.QueryRow(`SELECT state FROM tg_messages WHERE account_id=? AND chat_id=? AND message_id=?`, m.AccountID, m.ChatID, m.MessageID).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if state == "managed" || state == "retired" || state == "done" || state == "ignored" {
		return 0, nil
	}
	if _, err = tx.Exec(upsertTelegramMessageSQL, telegramMessageArgs(m)...); err != nil {
		return 0, err
	}
	key := TelegramAlbumTaskKey(albumID)
	var id int64
	err = tx.QueryRow(`SELECT id FROM tasks WHERE type=? AND file_id=? AND json_extract(payload,'$.message_id')=? AND state IN ('pending','running') ORDER BY id DESC LIMIT 1`, TaskTelegramReceive, key, m.MessageID).Scan(&id)
	if err == nil {
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	payload := TaskPayloadJSON(map[string]any{"album_id": albumID, "account_id": m.AccountID, "chat_id": m.ChatID, "message_id": m.MessageID, "automatic": true})
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskTelegramReceive, key, payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachTelegramReceiveOperation(tx, id, albumID, m); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
