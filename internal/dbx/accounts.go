package dbx

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

const AdminOwner = "admin:1"

const accountStorageSchema = `
CREATE TABLE accounts(id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE,password_hash TEXT NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
ALTER TABLE sessions ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'admin:1';
ALTER TABLE albums ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'admin:1';
ALTER TABLE files ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'admin:1';
DROP INDEX idx_albums_default;
CREATE UNIQUE INDEX idx_albums_default ON albums(owner_id) WHERE is_default=1;
CREATE INDEX files_owner_created ON files(owner_id,created_at);
CREATE INDEX albums_owner ON albums(owner_id);
ALTER TABLE file_name_counters RENAME TO legacy_file_name_counters;
CREATE TABLE file_name_counters(owner_id TEXT NOT NULL DEFAULT 'admin:1',date TEXT NOT NULL,last_sequence INTEGER NOT NULL CHECK(last_sequence BETWEEN 1 AND 9999),PRIMARY KEY(owner_id,date));
INSERT INTO file_name_counters(owner_id,date,last_sequence) SELECT 'admin:1',date,last_sequence FROM legacy_file_name_counters;
DROP TABLE legacy_file_name_counters;

CREATE TRIGGER file_owner_insert BEFORE INSERT ON files WHEN NEW.album_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM albums WHERE id=NEW.album_id AND owner_id=NEW.owner_id) BEGIN SELECT RAISE(ABORT,'相册不属于文件账号'); END;
CREATE TRIGGER file_owner_update BEFORE UPDATE OF owner_id,album_id ON files WHEN NEW.owner_id!=OLD.owner_id OR (NEW.album_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM albums WHERE id=NEW.album_id AND owner_id=NEW.owner_id)) BEGIN SELECT RAISE(ABORT,'不允许跨账号移动文件'); END;
CREATE TRIGGER album_owner_update BEFORE UPDATE OF owner_id ON albums WHEN OLD.owner_id!=NEW.owner_id BEGIN SELECT RAISE(ABORT,'不允许改变相册账号'); END;
CREATE TABLE account_telegram(owner_id TEXT PRIMARY KEY,account_id INTEGER NOT NULL DEFAULT 0,chat_id INTEGER NOT NULL DEFAULT 0,create_token TEXT NOT NULL UNIQUE,state TEXT NOT NULL DEFAULT 'pending',last_error TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,retry_after INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX account_telegram_chat ON account_telegram(chat_id) WHERE chat_id>0;
`

type ownerContextKey struct{}

func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerContextKey{}, owner)
}
func ContextOwner(ctx context.Context) string {
	v, _ := ctx.Value(ownerContextKey{}).(string)
	return NormalizeOwner(v)
}
func NormalizeOwner(owner string) string {
	if owner == "" {
		return AdminOwner
	}
	return owner
}

// Directory names are reversible, collision-free and valid on Windows too.
func OwnerDirectory(owner string) string {
	return "account-" + hex.EncodeToString([]byte(NormalizeOwner(owner)))
}

type Account struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Disabled     bool   `json:"disabled"`
}

func (db *DB) CreateAccount(id, username, hash string) error {
	if !strings.HasPrefix(id, "user:") || len(id) < 8 || !ValidUsername(username) || hash == "" {
		return errors.New("账号参数无效")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin WHERE username=?`, username).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("账号名已存在")
	}
	_, err := db.Exec(`INSERT INTO accounts(id,username,password_hash,created_at) VALUES(?,?,?,?)`, id, username, hash, now())
	return err
}
func (db *DB) AccountByUsername(username string) (*Account, error) {
	a := &Account{}
	err := db.QueryRow(`SELECT id,username,password_hash,disabled FROM accounts WHERE username=?`, username).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}
func (db *DB) AccountActive(owner string) bool {
	if owner == AdminOwner {
		return true
	}
	var disabled bool
	return db.QueryRow(`SELECT disabled FROM accounts WHERE id=?`, owner).Scan(&disabled) == nil && !disabled
}
func (db *DB) AccountIDs() ([]string, error) {
	rows, err := db.Query(`SELECT id FROM accounts WHERE disabled=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{AdminOwner}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (db *DB) EnsureOwnerDefaultAlbum(owner string) (int64, error) {
	owner = NormalizeOwner(owner)
	_, err := db.Exec(`INSERT INTO albums(name,created_at,is_default,owner_id) VALUES('未分类',?,1,?) ON CONFLICT DO NOTHING`, now(), owner)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRow(`SELECT id FROM albums WHERE owner_id=? AND is_default=1`, owner).Scan(&id)
	return id, err
}
func (db *DB) InsertOwnerAlbum(owner, name string) (int64, error) {
	r, err := db.Exec(`INSERT INTO albums(name,created_at,owner_id) VALUES(?,?,?)`, name, now(), NormalizeOwner(owner))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}
func (db *DB) OwnedFile(owner, id string) (*File, error) {
	f, err := db.GetFile(id)
	if err == nil && f.OwnerID != NormalizeOwner(owner) {
		return nil, ErrNotFound
	}
	return f, err
}
func (db *DB) OwnedAlbum(owner string, id int64) (*Album, error) {
	a, err := db.GetAlbum(id)
	if err == nil && a.OwnerID != NormalizeOwner(owner) {
		return nil, ErrNotFound
	}
	return a, err
}
func (db *DB) InsertOwnerSession(owner, hash string, expires int64, ua, ip string) error {
	_, err := db.Exec(`INSERT INTO sessions(token_hash,created_at,expires_at,ua,ip,owner_id) VALUES(?,?,?,?,?,?)`, hash, now(), expires, ua, ip, NormalizeOwner(owner))
	return err
}

// assignFileOwner runs inside intake's transaction, before deduplication.
func assignFileOwner(q fileInserter, f *File) error {
	if f == nil {
		return errors.New("上传记录为空")
	}
	if f.AlbumID.Valid {
		var owner string
		if err := q.QueryRow(`SELECT owner_id FROM albums WHERE id=?`, f.AlbumID.Int64).Scan(&owner); err != nil {
			return ErrNotFound
		}
		if f.OwnerID != "" && f.OwnerID != owner {
			return errors.New("相册不属于上传账号")
		}
		f.OwnerID = owner
	}
	f.OwnerID = NormalizeOwner(f.OwnerID)
	return nil
}

type TelegramSpace struct {
	OwnerID         string `json:"owner_id"`
	AccountID       int64  `json:"account_id"`
	ChatID          int64  `json:"chat_id"`
	Token           string `json:"-"`
	State           string `json:"state"`
	LastError       string `json:"last_error"`
	Attempts        int    `json:"attempts"`
	RetryAfter      int64  `json:"retry_after"`
	ChatTitle       string `json:"chat_title"`
	DesiredTitle    string `json:"desired_title"`
	TitleError      string `json:"title_error"`
	TitleRetryAfter int64  `json:"title_retry_after"`
}

func (db *DB) TelegramSpace(owner string) (*TelegramSpace, error) {
	b := &TelegramSpace{}
	err := db.QueryRow(`SELECT owner_id,account_id,chat_id,create_token,state,last_error,attempts,retry_after,chat_title,desired_title,title_error,title_retry_after FROM account_telegram WHERE owner_id=?`, NormalizeOwner(owner)).Scan(&b.OwnerID, &b.AccountID, &b.ChatID, &b.Token, &b.State, &b.LastError, &b.Attempts, &b.RetryAfter, &b.ChatTitle, &b.DesiredTitle, &b.TitleError, &b.TitleRetryAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
func (db *DB) TelegramChatAllowed(account, chat int64) bool {
	var n int
	return db.QueryRow(`SELECT COUNT(*) FROM account_telegram WHERE account_id=? AND chat_id=? AND state='ready'`, account, chat).Scan(&n) == nil && n == 1
}
