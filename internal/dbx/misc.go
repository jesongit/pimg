package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// ---- settings ----

// GetSetting 读设置（不存在返回空串）。
func (db *DB) GetSetting(k string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetSetting 写设置。
func (db *DB) SetSetting(k, v string) error {
	_, err := db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// AllSettings 全部设置。
func (db *DB) AllSettings() (map[string]string, error) {
	rows, err := db.Query(`SELECT k,v FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// ---- admin ----

// GetAdmin 取管理员。
func (db *DB) GetAdmin() (string, string, error) {
	var u, h string
	err := db.QueryRow(`SELECT username, password_hash FROM admin WHERE id=1`).Scan(&u, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return u, h, err
}

// EnsureAdmin 首次启动时创建管理员（bcrypt 已在外部计算）。
func (db *DB) EnsureAdmin(username, passwordHash string) error {
	return db.EnsureAdminWithSetup(username, passwordHash, false)
}

// The setup flag belongs only to a newly inserted default account. Upgrades
// must never reset credentials or put an existing administrator into setup.
func (db *DB) EnsureAdminWithSetup(username, passwordHash string, setup bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO admin(id,username,password_hash,created_at,updated_at)
		VALUES(1,?,?,?,?) ON CONFLICT(id) DO NOTHING`, username, passwordHash, now(), now())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 && setup {
		if _, err = tx.Exec(`INSERT INTO settings(k,v) VALUES('auth.admin_setup_required','1') ON CONFLICT(k) DO UPDATE SET v='1'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) AdminSetupRequired() (bool, error) {
	v, err := db.GetSetting("auth.admin_setup_required")
	return v == "1", err
}

// UpdateAdminCredentials 更新用户名/密码哈希。
func (db *DB) UpdateAdminCredentials(username, passwordHash string) error {
	_, current, err := db.GetAdmin()
	if err != nil {
		return err
	}
	return db.ChangeAdminCredentials(username, current, passwordHash, "")
}

// Check the password snapshot again in the transaction so concurrent password
// changes cannot reuse stale credentials. Username changes keep ownership intact.
func (db *DB) ChangeAdminCredentials(username, expectedHash, passwordHash, keepSession string) error {
	if !ValidUsername(username) {
		return errors.New("用户名须为 1–64 个字符")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE username=?`, username).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("用户名已存在")
	}
	result, err := tx.Exec(`UPDATE admin SET username=?,password_hash=?,updated_at=? WHERE id=1 AND password_hash=?`, username, passwordHash, now(), expectedHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("账号凭证已改变，请重新登录")
	}
	if passwordHash != expectedHash {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE owner_id=? AND token_hash<>?`, AdminOwner, keepSession); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM api_tokens WHERE owner_id=?`, AdminOwner); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM settings WHERE k='auth.admin_setup_required'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- sessions ----

// InsertSession 新建会话。
func (db *DB) InsertSession(tokenHash string, expiresAt int64, ua, ip string) error {
	_, err := db.Exec(`INSERT INTO sessions(token_hash,created_at,expires_at,ua,ip) VALUES(?,?,?,?,?)`,
		tokenHash, now(), expiresAt, ua, ip)
	return err
}

// GetSession 取会话。
func (db *DB) GetSession(tokenHash string) (*Session, error) {
	s := &Session{}
	err := db.QueryRow(`SELECT token_hash,created_at,expires_at,ua,ip,owner_id FROM sessions WHERE token_hash=?`, tokenHash).
		Scan(&s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.UA, &s.IP, &s.OwnerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s, nil
}

// DeleteSession 注销。
func (db *DB) DeleteSession(tokenHash string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// CleanSessions 清理过期会话。
func (db *DB) CleanSessions() {
	db.Exec(`DELETE FROM sessions WHERE expires_at<?`, now())
}

// ---- api tokens ----

// InsertToken 新建 API Token。
func (db *DB) InsertToken(t *APIToken) error {
	if t.Scope == "" {
		t.Scope = "admin"
	}
	if t.OwnerID == "" {
		t.OwnerID = "admin:1"
	}
	topics, err := json.Marshal(t.TopicIDs)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,prefix,created_at,scope,owner_id,topic_ids) VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.TokenHash, t.Prefix, now(), t.Scope, t.OwnerID, string(topics))
	return err
}

// GetTokenByHash 按哈希查有效 token。
func (db *DB) GetTokenByHash(hash string) (*APIToken, error) {
	t := &APIToken{}
	var topics string
	err := db.QueryRow(`SELECT id,name,token_hash,prefix,created_at,last_used_at,revoked_at,scope,owner_id,topic_ids
		FROM api_tokens WHERE token_hash=? AND revoked_at IS NULL`, hash).
		Scan(&t.ID, &t.Name, &t.TokenHash, &t.Prefix, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.Scope, &t.OwnerID, &topics)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(topics), &t.TopicIDs); err != nil {
		return nil, err
	}
	return t, nil
}

// ListTokens 列出全部 token。
func (db *DB) ListTokens() ([]*APIToken, error) {
	rows, err := db.Query(`SELECT id,name,token_hash,prefix,created_at,last_used_at,revoked_at,scope,owner_id,topic_ids
		FROM api_tokens WHERE revoked_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t := &APIToken{}
		var topics string
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &t.Prefix, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.Scope, &t.OwnerID, &topics); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(topics), &t.TopicIDs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken 吊销。
func (db *DB) RevokeToken(id string) error {
	_, err := db.Exec(`UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now(), id)
	return err
}

// TouchToken 记录最后使用。
func (db *DB) TouchToken(hash string) {
	db.Exec(`UPDATE api_tokens SET last_used_at=? WHERE token_hash=?`, now(), hash)
}

// ---- albums ----

// EnsureHomeAlbum 确保固定「游客相册」存在（is_home=1，不可删除；成员=首页展示）。
func (db *DB) EnsureHomeAlbum() (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM albums WHERE is_home=1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO albums(name,created_at,is_home) VALUES('游客相册',?,1)`, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetHomeAlbum 取首页相册（不存在返回 ErrNotFound）。
func (db *DB) GetHomeAlbum() (*Album, error) {
	a := &Album{}
	err := db.QueryRow(`SELECT id,name,created_at FROM albums WHERE is_home=1`).Scan(&a.ID, &a.Name, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.IsHome = true
	return a, nil
}

// ListAlbums 相册列表（首页相册置顶）。
func (db *DB) ListAlbums() ([]*Album, error) {
	return db.ListOwnerAlbums("")
}

func (db *DB) ListOwnerAlbums(owner string) ([]*Album, error) {
	query := `SELECT a.id, a.name, a.created_at, a.is_home, a.is_default, a.owner_id, a.displayed,
		(SELECT COUNT(*) FROM files f WHERE f.hidden=0 AND f.owner_id=a.owner_id AND (f.album_id=a.id OR (a.is_default=1 AND f.album_id IS NULL))) FROM albums a`
	var args []any
	if owner != "" {
		query += ` WHERE a.owner_id=?`
		args = append(args, owner)
	}
	rows, err := db.Query(query+` ORDER BY a.is_home DESC, a.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Album
	for rows.Next() {
		a := &Album{}
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt, &a.IsHome, &a.IsDefault, &a.OwnerID, &a.Displayed, &a.Count); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAlbum 新建相册。
func (db *DB) InsertAlbum(name string) (int64, error) {
	res, err := db.Exec(`INSERT INTO albums(name,created_at) VALUES(?,?)`, name, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RenameAlbum 重命名（固定游客相册不可重命名）。
func (db *DB) RenameAlbum(id int64, name string) error {
	var isHome, isDefault bool
	if err := db.QueryRow(`SELECT is_home,is_default FROM albums WHERE id=?`, id).Scan(&isHome, &isDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if isHome || isDefault {
		return errors.New("固定相册不能重命名")
	}
	res, err := db.Exec(`UPDATE albums SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAlbum 删除相册（文件 album_id 置空）；固定首页相册不可删除。
func (db *DB) DeleteAlbum(id int64) error {
	var isHome, isDefault bool
	if err := db.QueryRow(`SELECT is_home,is_default FROM albums WHERE id=?`, id).Scan(&isHome, &isDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if isHome || isDefault {
		return errors.New("固定相册不能删除")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := ownerTransferAlbumGuard(tx, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE files SET album_id=NULL WHERE album_id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM albums WHERE id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
