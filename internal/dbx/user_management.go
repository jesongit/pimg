package dbx

import (
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

const userManagementSchema = `
ALTER TABLE account_telegram ADD COLUMN chat_title TEXT NOT NULL DEFAULT '';
ALTER TABLE account_telegram ADD COLUMN desired_title TEXT NOT NULL DEFAULT '';
ALTER TABLE account_telegram ADD COLUMN title_error TEXT NOT NULL DEFAULT '';
ALTER TABLE account_telegram ADD COLUMN title_retry_after INTEGER NOT NULL DEFAULT 0;
`

type ManagedAccount struct {
	ID          string         `json:"id"`
	Username    string         `json:"username"`
	Role        string         `json:"role"`
	Disabled    bool           `json:"disabled"`
	Deleting    bool           `json:"deleting"`
	DeleteError string         `json:"delete_error,omitempty"`
	CreatedAt   int64          `json:"created_at"`
	Files       int64          `json:"files"`
	Albums      int64          `json:"albums"`
	Bytes       int64          `json:"bytes"`
	Telegram    *TelegramSpace `json:"telegram,omitempty"`
}

func (db *DB) ManagedAccounts() ([]ManagedAccount, error) {
	rows, err := db.Query(`SELECT 'admin:1',username,'admin',0,created_at,0 FROM admin UNION ALL SELECT id,username,'user',disabled,created_at,deleting FROM accounts ORDER BY 3,5,1`)
	if err != nil {
		return nil, err
	}
	items := []ManagedAccount{}
	for rows.Next() {
		var a ManagedAccount
		if err = rows.Scan(&a.ID, &a.Username, &a.Role, &a.Disabled, &a.CreatedAt, &a.Deleting); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range items {
		a := &items[i]
		if a.Deleting {
			_ = db.QueryRow(`SELECT CASE WHEN state='canceled' THEN '删除已暂停，可以重试' ELSE COALESCE(last_error,'') END FROM tasks WHERE type=? AND file_id=? ORDER BY id DESC LIMIT 1`, TaskAccountDelete, "account-delete:"+a.ID).Scan(&a.DeleteError)
		}
		if err = db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(size),0) FROM files WHERE owner_id=? AND hidden=0 AND purge_state IS NULL`, a.ID).Scan(&a.Files, &a.Bytes); err != nil {
			return nil, err
		}
		if err = db.QueryRow(`SELECT COUNT(*) FROM albums WHERE owner_id=?`, a.ID).Scan(&a.Albums); err != nil {
			return nil, err
		}
		if b, e := db.TelegramSpace(a.ID); e == nil {
			a.Telegram = b
		} else if !errors.Is(e, ErrNotFound) {
			return nil, e
		}
	}
	return items, nil
}

func ValidUsername(username string) bool {
	return username != "" && username == strings.TrimSpace(username) && utf8.RuneCountInString(username) <= 64 && !strings.ContainsAny(username, "\r\n\x00")
}

// Mutations keep the immutable owner ID, files, albums, and archive bindings.
func (db *DB) UpdateManagedAccount(owner string, username *string, disabled *bool, passwordHash string) error {
	if owner == AdminOwner {
		return errors.New("管理员账号请在账号设置中修改")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	var deleting bool
	if err = tx.QueryRow(`SELECT username,deleting FROM accounts WHERE id=?`, owner).Scan(&current, &deleting); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if deleting {
		return errors.New("用户正在删除，不能修改或重新启用")
	}
	if username != nil {
		if !ValidUsername(*username) {
			return errors.New("用户名须为 1–64 个字符，不能包含换行")
		}
		var n int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM admin WHERE username=?`, *username).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("用户名已存在")
		}
		if _, err = tx.Exec(`UPDATE accounts SET username=? WHERE id=?`, *username, owner); err != nil {
			return errors.New("用户名已存在或无法保存")
		}
	}
	if disabled != nil {
		if _, err = tx.Exec(`UPDATE accounts SET disabled=? WHERE id=?`, *disabled, owner); err != nil {
			return err
		}
	}
	if passwordHash != "" {
		if _, err = tx.Exec(`UPDATE accounts SET password_hash=? WHERE id=?`, passwordHash, owner); err != nil {
			return err
		}
	}
	if disabled != nil && *disabled || passwordHash != "" {
		if _, err = tx.Exec(`DELETE FROM sessions WHERE owner_id=?`, owner); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE api_tokens SET revoked_at=? WHERE owner_id=? AND revoked_at IS NULL`, now(), owner); err != nil {
			return err
		}
	}
	return tx.Commit()
}
