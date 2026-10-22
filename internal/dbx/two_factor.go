package dbx

import (
	"database/sql"
	"errors"
)

const twoFactorSchema = `
CREATE TABLE account_two_factor(
 owner_id TEXT PRIMARY KEY,
 secret BLOB NOT NULL, nonce BLOB NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL DEFAULT 0,
 last_step INTEGER NOT NULL DEFAULT -1
);
CREATE TABLE two_factor_recovery(
 owner_id TEXT NOT NULL REFERENCES account_two_factor(owner_id) ON DELETE CASCADE,
 code_hash TEXT NOT NULL,
 PRIMARY KEY(owner_id,code_hash)
);
CREATE TRIGGER account_two_factor_delete AFTER DELETE ON accounts BEGIN
 DELETE FROM account_two_factor WHERE owner_id=OLD.id;
END;
`

type TwoFactor struct {
	Secret, Nonce       []byte
	Enabled             bool
	ExpiresAt, LastStep int64
}

func (db *DB) TwoFactor(owner string) (*TwoFactor, error) {
	f := &TwoFactor{}
	err := db.QueryRow(`SELECT secret,nonce,enabled,expires_at,last_step FROM account_two_factor WHERE owner_id=?`, owner).Scan(&f.Secret, &f.Nonce, &f.Enabled, &f.ExpiresAt, &f.LastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return f, err
}

func (db *DB) SetTwoFactorPending(owner string, secret, nonce []byte, expires int64) error {
	r, err := db.Exec(`INSERT INTO account_two_factor(owner_id,secret,nonce,expires_at) VALUES(?,?,?,?)
 ON CONFLICT(owner_id) DO UPDATE SET secret=excluded.secret,nonce=excluded.nonce,expires_at=excluded.expires_at,last_step=-1 WHERE enabled=0`, owner, secret, nonce, expires)
	return changedFactor(r, err)
}

func changedFactor(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("验证信息已失效或已使用，请重试")
	}
	return err
}

func (db *DB) ConsumeTwoFactorStep(owner string, nonce []byte, step int64) error {
	r, err := db.Exec(`UPDATE account_two_factor SET last_step=? WHERE owner_id=? AND enabled=1 AND nonce=? AND last_step<?`, step, owner, nonce, step)
	return changedFactor(r, err)
}

func (db *DB) ConsumeRecovery(owner, hash string) error {
	r, err := db.Exec(`DELETE FROM two_factor_recovery WHERE owner_id=? AND code_hash=? AND EXISTS(SELECT 1 FROM account_two_factor WHERE owner_id=? AND enabled=1)`, owner, hash, owner)
	return changedFactor(r, err)
}

func (db *DB) RecoveryCount(owner string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM two_factor_recovery WHERE owner_id=?`, owner).Scan(&n)
	return n, err
}

// FinishTwoFactor changes the factor and revokes other sessions in one commit.
// An enable is bound to the exact, still-live pending enrollment.
func (db *DB) FinishTwoFactor(owner, action, keep string, nonce []byte, step int64, hashes []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if action == "enable" {
		r, e := tx.Exec(`UPDATE account_two_factor SET enabled=1,expires_at=0,last_step=? WHERE owner_id=? AND nonce=? AND enabled=0 AND expires_at>?`, step, owner, nonce, now())
		if err = changedFactor(r, e); err != nil {
			return err
		}
	} else if action == "disable" {
		r, e := tx.Exec(`DELETE FROM account_two_factor WHERE owner_id=? AND enabled=1`, owner)
		if err = changedFactor(r, e); err != nil {
			return err
		}
	} else if action != "recovery" {
		return errors.New("不支持的操作")
	}
	if _, err = tx.Exec(`DELETE FROM two_factor_recovery WHERE owner_id=?`, owner); err != nil {
		return err
	}
	for _, hash := range hashes {
		if _, err = tx.Exec(`INSERT INTO two_factor_recovery(owner_id,code_hash) VALUES(?,?)`, owner, hash); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM sessions WHERE owner_id=? AND token_hash<>?`, owner, keep); err != nil {
		return err
	}
	return tx.Commit()
}
