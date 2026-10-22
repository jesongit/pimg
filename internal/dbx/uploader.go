package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const uploaderSchema = `
ALTER TABLE api_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'admin';
ALTER TABLE api_tokens ADD COLUMN owner_id TEXT NOT NULL DEFAULT 'admin:1';
ALTER TABLE api_tokens ADD COLUMN topic_ids TEXT NOT NULL DEFAULT '[]';
CREATE TABLE intake_mutex(id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL DEFAULT 0);
INSERT INTO intake_mutex(id) VALUES(1);
CREATE TABLE uploader_keys(
 instance_id TEXT NOT NULL, upload_id TEXT NOT NULL, file_id TEXT NOT NULL,
 keyset_id TEXT NOT NULL, owner_id TEXT NOT NULL, chat_id INTEGER NOT NULL, topic_id INTEGER NOT NULL,
 crypto_version INTEGER NOT NULL, wrapped BLOB NOT NULL, nonce BLOB NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(instance_id,file_id)
);
CREATE TABLE uploader_files(
 instance_id TEXT NOT NULL, keyset_id TEXT NOT NULL, tool_file_id TEXT NOT NULL,
 upload_id TEXT NOT NULL, album_id INTEGER NOT NULL, account_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, topic_id INTEGER NOT NULL,
 mode TEXT NOT NULL, metadata TEXT NOT NULL DEFAULT '', part_index INTEGER NOT NULL DEFAULT 0, part_count INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'pending', canonical_id TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(instance_id,keyset_id,tool_file_id)
);
CREATE INDEX uploader_group ON uploader_files(instance_id,keyset_id,upload_id);
CREATE INDEX uploader_canonical ON uploader_files(canonical_id);
CREATE TABLE uploader_parts(
 instance_id TEXT NOT NULL, keyset_id TEXT NOT NULL, tool_file_id TEXT NOT NULL,
 kind TEXT NOT NULL, idx INTEGER NOT NULL, parts INTEGER NOT NULL,
 account_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
 body TEXT NOT NULL,
 PRIMARY KEY(instance_id,keyset_id,tool_file_id,kind,idx),
 UNIQUE(account_id,chat_id,message_id)
);
CREATE TABLE ingest_events(
 id INTEGER PRIMARY KEY AUTOINCREMENT, source_key TEXT NOT NULL, event_type TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT '', file_id TEXT NOT NULL DEFAULT '', tool_file_id TEXT NOT NULL DEFAULT '',
 upload_id TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE INDEX ingest_event_file ON ingest_events(file_id,id DESC);
CREATE INDEX ingest_event_tool ON ingest_events(tool_file_id,id DESC);
CREATE INDEX ingest_event_upload ON ingest_events(upload_id,id DESC);
CREATE TRIGGER uploader_retire_hidden AFTER UPDATE OF hidden ON files WHEN NEW.hidden=1 AND OLD.hidden=0 BEGIN
 INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at)
 SELECT instance_id||':'||keyset_id||':'||tool_file_id,'retired_ignored','转换后旧身份作废',NEW.id,tool_file_id,upload_id,
 json_object('instance_id',instance_id,'keyset_id',keyset_id,'account_id',account_id,'chat_id',chat_id,'topic_id',topic_id,'requested_album_id',album_id,'retained_album_id',NEW.album_id,'mode',mode,'sha256',json_extract(metadata,'$.sha256'),'size',json_extract(metadata,'$.size'),'verification','declared'),unixepoch()
 FROM uploader_files WHERE canonical_id=NEW.id AND state!='retired';
 UPDATE uploader_files SET state='retired',updated_at=unixepoch() WHERE canonical_id=NEW.id;
 UPDATE tg_messages SET state='retired',updated_at=unixepoch() WHERE file_id=NEW.id;
END;
CREATE TRIGGER uploader_retire_deleted BEFORE DELETE ON files BEGIN
 INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at)
 SELECT instance_id||':'||keyset_id||':'||tool_file_id,'retired_ignored','文件已删除，旧身份作废',OLD.id,tool_file_id,upload_id,
 json_object('instance_id',instance_id,'keyset_id',keyset_id,'account_id',account_id,'chat_id',chat_id,'topic_id',topic_id,'requested_album_id',album_id,'retained_album_id',OLD.album_id,'mode',mode,'sha256',json_extract(metadata,'$.sha256'),'size',json_extract(metadata,'$.size'),'verification','declared'),unixepoch()
 FROM uploader_files WHERE canonical_id=OLD.id AND state!='retired';
 UPDATE uploader_files SET state='retired',updated_at=unixepoch() WHERE canonical_id=OLD.id;
 UPDATE tg_messages SET state='retired',updated_at=unixepoch() WHERE file_id=OLD.id;
END;
`

var ErrUploaderConflict = errors.New("上传身份或参数冲突")
var ErrUploaderForbidden = errors.New("无此密钥申请的访问权限")
var ErrUploaderRetired = errors.New("上传身份已作废")
var ErrImagePending = errors.New("相同图片正在归档，请稍后重试")

// A write before reading serializes all intake transactions, including distinct
// DB handles. Historical duplicates stay intact; no destructive unique-index migration.
func lockIntake(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE intake_mutex SET revision=revision WHERE id=1`)
	return err
}

func (db *DB) EnsureSetting(key, candidate string) (string, error) {
	if _, err := db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO NOTHING`, key, candidate); err != nil {
		return "", err
	}
	return db.GetSetting(key)
}

type UploaderKey struct {
	InstanceID, KeysetID, UploadID, FileID, OwnerID string
	ChatID                                          int64
	TopicID, CryptoVersion                          int
	Wrapped, Nonce                                  []byte
}

func (db *DB) ReserveUploaderKey(k UploaderKey) (*UploaderKey, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return nil, err
	}
	var retired bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM uploader_files WHERE instance_id=? AND tool_file_id=? AND state='retired')`, k.InstanceID, k.FileID).Scan(&retired); err != nil {
		return nil, err
	}
	if retired {
		return nil, ErrUploaderRetired
	}
	previous := k
	err = tx.QueryRow(`SELECT keyset_id,upload_id,owner_id,chat_id,topic_id,crypto_version,wrapped,nonce FROM uploader_keys WHERE instance_id=? AND file_id=?`, k.InstanceID, k.FileID).Scan(&previous.KeysetID, &previous.UploadID, &previous.OwnerID, &previous.ChatID, &previous.TopicID, &previous.CryptoVersion, &previous.Wrapped, &previous.Nonce)
	if err == nil {
		if previous.OwnerID != k.OwnerID {
			return nil, ErrUploaderForbidden
		}
		if previous.KeysetID != k.KeysetID || previous.UploadID != k.UploadID || previous.ChatID != k.ChatID || previous.TopicID != k.TopicID || previous.CryptoVersion != k.CryptoVersion {
			return nil, ErrUploaderConflict
		}
		return &previous, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM files WHERE id=? UNION ALL SELECT 1 FROM uploader_files WHERE instance_id=? AND tool_file_id=?)`, k.FileID, k.InstanceID, k.FileID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrUploaderConflict
	}
	var recent int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM uploader_keys WHERE owner_id=? AND created_at>?`, k.OwnerID, now()-60).Scan(&recent); err != nil {
		return nil, err
	}
	if recent >= 120 {
		return nil, ErrUploaderRateLimited
	}
	_, err = tx.Exec(`INSERT INTO uploader_keys VALUES(?,?,?,?,?,?,?,?,?,?,?)`, k.InstanceID, k.UploadID, k.FileID, k.KeysetID, k.OwnerID, k.ChatID, k.TopicID, k.CryptoVersion, k.Wrapped, k.Nonce, now())
	if err != nil {
		return nil, err
	}
	return &k, tx.Commit()
}

var ErrUploaderRateLimited = errors.New("文件密钥申请过于频繁")

// Event bodies contain public media identities and declared/verified status,
// never wrapped/plain keys, authorization headers or Telegram access hashes.
type IngestEvent struct {
	ID         int64           `json:"id"`
	SourceKey  string          `json:"source_key"`
	Type       string          `json:"type"`
	Reason     string          `json:"reason"`
	FileID     string          `json:"file_id"`
	ToolFileID string          `json:"tool_file_id"`
	UploadID   string          `json:"upload_id"`
	Body       json.RawMessage `json:"details"`
	CreatedAt  int64           `json:"created_at"`
}

func appendIngestEvent(tx *sql.Tx, e IngestEvent) error {
	var typ, reason string
	err := tx.QueryRow(`SELECT event_type,reason FROM ingest_events WHERE source_key=? ORDER BY id DESC LIMIT 1`, e.SourceKey).Scan(&typ, &reason)
	if err == nil && typ == e.Type && reason == e.Reason {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(e.Body) == 0 {
		e.Body = json.RawMessage(`{}`)
	}
	if err := enrichIntakeEvent(tx, &e); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at) VALUES(?,?,?,?,?,?,?,?)`, e.SourceKey, e.Type, e.Reason, e.FileID, e.ToolFileID, e.UploadID, string(e.Body), now())
	return err
}
func (db *DB) RecordIngestEvent(e IngestEvent) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return err
	}
	if err = appendIngestEvent(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) ListIngestEvents(fileID, toolID, uploadID string, before int64) ([]IngestEvent, error) {
	where := []string{"id < ?"}
	if before <= 0 {
		before = 1 << 62
	}
	args := []any{before}
	for _, v := range []struct{ column, value string }{{"file_id", fileID}, {"tool_file_id", toolID}, {"upload_id", uploadID}} {
		if v.value != "" {
			where = append(where, v.column+"=?")
			args = append(args, v.value)
		}
	}
	rows, err := db.Query(`SELECT id,source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at FROM ingest_events WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngestEvent{}
	for rows.Next() {
		var e IngestEvent
		var body string
		if err = rows.Scan(&e.ID, &e.SourceKey, &e.Type, &e.Reason, &e.FileID, &e.ToolFileID, &e.UploadID, &body, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Body = json.RawMessage(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

func imageCandidate(tx *sql.Tx, f *File) (*File, error) {
	if f.Kind != "image" {
		return nil, nil
	}
	if err := assignFileOwner(tx, f); err != nil {
		return nil, err
	}
	mode := f.Storage
	if mode == "" {
		mode = "tg"
	}
	candidate, err := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE owner_id=? AND kind='image' AND sha256=? AND (storage='direct')=? AND hidden=0 AND purge_state IS NULL AND COALESCE(archive_error,'')='' AND archive_state IN ('pending','running','done') AND NOT EXISTS(SELECT 1 FROM file_conversions c WHERE c.phase!='done' AND (c.source_id=files.id OR c.target_id=files.id)) ORDER BY created_at,id LIMIT 1`, f.OwnerID, f.SHA256, mode == "direct"))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if candidate.Size != f.Size {
		return nil, fmt.Errorf("图片指纹相同但大小不一致: %w", ErrUploaderConflict)
	}
	if candidate.ArchiveState != "done" {
		return nil, ErrImagePending
	}
	return candidate, nil
}
