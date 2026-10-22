package store

import (
	"encoding/json"
	"pimg/internal/storage"
	"strconv"
	"strings"
)

// Legacy object locators omitted global TG identity. Freeze that identity from
// the original pinned settings before any additional account group is created.
func (s *Service) backfillTelegramIdentity() error {
	raw, err := s.DB.GetSetting("telegram.account_id")
	if err != nil {
		return err
	}
	account, _ := strconv.ParseInt(raw, 10, 64)
	chat := s.TGChatID()
	if account <= 0 || chat <= 0 {
		return nil
	}
	complete := func(key string) (string, error) {
		if !strings.HasPrefix(key, telegramReferencePrefix) {
			return key, nil
		}
		object, e := decodeTelegramObject(key)
		if e != nil {
			return "", e
		}
		if object.AccountID == 0 {
			object.AccountID = account
		}
		if object.ChatID == 0 {
			object.ChatID = chat
		}
		return encodeTelegramObject(object), nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,object_key FROM file_chunks WHERE object_key LIKE 'tguser:%'`)
	if err != nil {
		return err
	}
	type chunk struct {
		id  int64
		key string
	}
	var chunks []chunk
	for rows.Next() {
		var c chunk
		if err = rows.Scan(&c.id, &c.key); err != nil {
			rows.Close()
			return err
		}
		chunks = append(chunks, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range chunks {
		next, e := complete(c.key)
		if e != nil {
			return e
		}
		if next != c.key {
			if _, err = tx.Exec(`UPDATE file_chunks SET object_key=? WHERE id=?`, next, c.id); err != nil {
				return err
			}
		}
	}
	for _, table := range []string{"archive_objects", "archive_garbage"} {
		rows, e := tx.Query(`SELECT rowid,reference FROM ` + table)
		if e != nil {
			return e
		}
		type entry struct {
			id        int64
			reference string
		}
		var entries []entry
		for rows.Next() {
			var value entry
			if err = rows.Scan(&value.id, &value.reference); err != nil {
				rows.Close()
				return err
			}
			entries = append(entries, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, value := range entries {
			var ref storage.Reference
			if err = json.Unmarshal([]byte(value.reference), &ref); err != nil {
				return err
			}
			key, e := complete(ref.Key)
			if e != nil {
				return e
			}
			if key != ref.Key {
				ref.Key = key
				if _, err = tx.Exec(`UPDATE `+table+` SET reference=? WHERE rowid=?`, referenceJSON(ref), value.id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
