// Package dbx 负责 SQLite 打开、迁移与所有数据访问。
package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"pimg/internal/diagnostics"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// DB 包装 database/sql。
type DB struct {
	*sql.DB
	reader      *sql.DB
	fileOrders  fileOrderCache
	performance atomic.Pointer[diagnostics.Performance]
	versions    changeVersionReader
	readDSN     string
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", filepath.ToSlash(path))
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite 单写者：限制连接数避免 database is locked。
	d.SetMaxOpenConns(1)
	db := &DB{DB: d}
	if err := db.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	// WAL permits readers while the single writer commits. Large task/history
	// queries must not occupy the only connection used by uploads and workers.
	reader, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)", filepath.ToSlash(path)))
	if err != nil {
		d.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(2)
	reader.SetMaxIdleConns(2)
	if err = reader.Ping(); err != nil {
		reader.Close()
		d.Close()
		return nil, err
	}
	db.reader = reader
	db.readDSN = fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)", filepath.ToSlash(path))
	return db, nil
}

func (db *DB) Close() error {
	db.closeChangeVersion()
	if p := db.Performance(); p != nil {
		_ = p.Close()
	}
	var readErr error
	if db.reader != nil {
		readErr = db.reader.Close()
	}
	err := db.DB.Close()
	if err != nil {
		return err
	}
	return readErr
}

// Read helpers are deliberately explicit: transactions and writes stay on the
// original single-writer pool. No additional database or backup is created.
func (db *DB) ReadQuery(query string, args ...any) (ReadRows, error) {
	return db.ReadQueryContext(context.Background(), query, args...)
}
func (db *DB) ReadQueryContext(ctx context.Context, query string, args ...any) (ReadRows, error) {
	span := db.performanceSpan(ctx)
	pool := db.reader
	if pool == nil {
		pool = db.DB
	}
	rows, err := pool.QueryContext(ctx, query, args...)
	if err != nil {
		span.Finish(err)
		return nil, err
	}
	if span == nil {
		return rows, nil
	}
	return &measuredRows{Rows: rows, span: span}, nil
}
func (db *DB) ReadQueryRow(query string, args ...any) RowScanner {
	span := db.performanceSpan(context.Background())
	pool := db.reader
	if pool == nil {
		pool = db.DB
	}
	row := pool.QueryRow(query, args...)
	if span == nil {
		return row
	}
	return &measuredRow{row, span}
}

func (db *DB) ReadStats() sql.DBStats {
	if db.reader != nil {
		return db.reader.Stats()
	}
	return db.DB.Stats()
}

var migrations = []string{
	// v1
	`
CREATE TABLE settings(
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE admin(
  id INTEGER PRIMARY KEY CHECK(id=1),
  username TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE sessions(
  token_hash TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ua TEXT, ip TEXT
);
CREATE TABLE api_tokens(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at INTEGER
);
CREATE TABLE albums(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE files(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  orig_name TEXT NOT NULL,
  ext TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('image','video','other')),
  width INTEGER, height INTEGER, duration REAL,
  key_wrapped BLOB NOT NULL,
  key_nonce BLOB NOT NULL,
  crypto_ver INTEGER NOT NULL DEFAULT 1,
  album_id INTEGER REFERENCES albums(id) ON DELETE SET NULL,
  archive_state TEXT NOT NULL DEFAULT 'pending' CHECK(archive_state IN ('pending','running','done','failed')),
  archive_error TEXT,
  archive_attempts INTEGER NOT NULL DEFAULT 0,
  archived_at INTEGER,
  has_thumb INTEGER NOT NULL DEFAULT 0,
  thumb_w INTEGER, thumb_h INTEGER, thumb_mime TEXT, thumb_size INTEGER,
  has_bthumb INTEGER NOT NULL DEFAULT 0,
  published INTEGER NOT NULL DEFAULT 0,
  published_at INTEGER,
  published_path TEXT,
  publish_error TEXT,
  show_home INTEGER NOT NULL DEFAULT 0,
  purge_state TEXT CHECK(purge_state IS NULL OR purge_state IN ('pending','running','done','failed')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_files_created ON files(created_at DESC);
CREATE INDEX idx_files_album ON files(album_id);
CREATE INDEX idx_files_home ON files(published, show_home, published_at DESC);
CREATE TABLE file_chunks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('data','thumb','bthumb')),
  idx INTEGER NOT NULL,
  message_id INTEGER,
  tg_file_id TEXT,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  uploaded_at INTEGER,
  UNIQUE(file_id, kind, idx)
);
CREATE INDEX idx_chunks_file ON file_chunks(file_id, kind, idx);
CREATE TABLE temp_objects(
  id TEXT PRIMARY KEY,
  file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('original','thumb')),
  object_key TEXT NOT NULL,
  size INTEGER,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX idx_temp_expiry ON temp_objects(expires_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_temp_file ON temp_objects(file_id, kind) WHERE deleted_at IS NULL;
CREATE TABLE tasks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  file_id TEXT,
  payload TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','running','done','failed','canceled')),
  attempts INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 8,
  last_error TEXT,
  result TEXT,
  run_after INTEGER NOT NULL,
  lease_until INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  done_at INTEGER
);
CREATE INDEX idx_tasks_ready ON tasks(state, run_after);
CREATE INDEX idx_tasks_file ON tasks(file_id);
`,
	// v2: 首页相册（直发通道）与文件存储位置
	`
ALTER TABLE files ADD COLUMN storage TEXT NOT NULL DEFAULT 'tg';
ALTER TABLE albums ADD COLUMN is_home INTEGER NOT NULL DEFAULT 0;
`,
	// v3: legacy archives remain Telegram/R2; new writes explicitly record providers.
	`
ALTER TABLE files ADD COLUMN cold_backend TEXT NOT NULL DEFAULT 'telegram';
ALTER TABLE files ADD COLUMN hot_backend TEXT NOT NULL DEFAULT 'r2';
ALTER TABLE file_chunks ADD COLUMN object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE temp_objects ADD COLUMN backend TEXT NOT NULL DEFAULT 'r2';
`,
	// v4: cleanup intent may precede a successful temporary-object upload.
	// Historical rows stay unconfirmed until an existing preview verifies them.
	`
ALTER TABLE temp_objects ADD COLUMN ready_at INTEGER;
`,
	// v5: Verify a hidden replacement, atomically switch visibility, then clean
	// the retired source through a separate resumable task.
	`
ALTER TABLE files ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;
CREATE TABLE file_conversions(
  source_id TEXT PRIMARY KEY,
  target_id TEXT NOT NULL UNIQUE,
  target_encrypted INTEGER NOT NULL,
  phase TEXT NOT NULL DEFAULT 'prepare' CHECK(phase IN ('prepare','archive','cleanup','done')),
  task_id INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  cleanup_pending INTEGER NOT NULL DEFAULT 0,
  cleanup_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`,
	// v6: User-session Telegram topics, durable discovery and verified moves.
	`
ALTER TABLE albums ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_albums_default ON albums(is_default) WHERE is_default=1;
ALTER TABLE files ADD COLUMN telegram_source TEXT;
CREATE UNIQUE INDEX idx_files_telegram_source ON files(telegram_source) WHERE telegram_source IS NOT NULL;
CREATE TABLE album_topics(
 album_id INTEGER PRIMARY KEY,
 account_id INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 topic_id INTEGER NOT NULL DEFAULT 0,
 name TEXT NOT NULL,
 create_name TEXT NOT NULL,
 is_default INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'creating',
 delete_phase TEXT NOT NULL DEFAULT '',
 last_error TEXT NOT NULL DEFAULT '',
 scan_date INTEGER NOT NULL DEFAULT 0,
 scan_message_id INTEGER NOT NULL DEFAULT 0,
 scan_offset INTEGER NOT NULL DEFAULT 0,
 scan_task_id INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_album_topics_source ON album_topics(account_id,chat_id,topic_id) WHERE topic_id>0;
CREATE TABLE tg_messages(
 account_id INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 message_id INTEGER NOT NULL,
 topic_id INTEGER NOT NULL,
 message_date INTEGER NOT NULL DEFAULT 0,
 reference TEXT NOT NULL DEFAULT '{}',
 name TEXT NOT NULL DEFAULT '',
 mime TEXT NOT NULL DEFAULT '',
 caption TEXT NOT NULL DEFAULT '',
 is_service INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'pending',
 file_id TEXT NOT NULL DEFAULT '',
 last_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(account_id,chat_id,message_id)
);
CREATE INDEX idx_tg_messages_topic ON tg_messages(account_id,chat_id,topic_id,state);
CREATE INDEX idx_tg_messages_file ON tg_messages(file_id);
CREATE TABLE tg_moves(
 file_id TEXT PRIMARY KEY,
 album_id INTEGER NOT NULL,
 phase TEXT NOT NULL DEFAULT 'prepare',
 refs TEXT NOT NULL DEFAULT '[]',
 last_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
`,
	// v7: one discovered Telegram message may produce independent video files.
	// Keep the receipt after purge, without a file FK that could erase it.
	`
CREATE TABLE telegram_source_parts(
 source_key TEXT NOT NULL,
 part_index INTEGER NOT NULL,
 file_id TEXT NOT NULL,
 purged_at INTEGER,
 PRIMARY KEY(source_key,part_index)
);
CREATE INDEX idx_telegram_source_parts_file ON telegram_source_parts(file_id);
INSERT INTO telegram_source_parts(source_key,part_index,file_id)
SELECT telegram_source,0,id FROM files WHERE telegram_source IS NOT NULL;
`,
	// v8: secondary cold repositories and a durable, low-priority replication queue.
	`
CREATE TABLE cold_repositories(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 driver TEXT NOT NULL,
 config TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
 created_at INTEGER NOT NULL,
 deleted_at INTEGER
);
CREATE UNIQUE INDEX idx_cold_repository_config ON cold_repositories(driver,config) WHERE deleted_at IS NULL;
CREATE TABLE cold_sync_revisions(
 file_id TEXT PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL DEFAULT 1
);
INSERT INTO cold_sync_revisions(file_id) SELECT id FROM files;
CREATE TABLE cold_sync_files(
 repository_id INTEGER NOT NULL REFERENCES cold_repositories(id),
 file_id TEXT NOT NULL,
 wanted_revision INTEGER NOT NULL,
 done_revision INTEGER NOT NULL DEFAULT 0,
 objects TEXT NOT NULL DEFAULT '[]',
 synced_at INTEGER NOT NULL DEFAULT 0,
 task_id INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(repository_id,file_id)
);
CREATE TABLE cold_sync_albums(
 repository_id INTEGER NOT NULL REFERENCES cold_repositories(id),
 album_id INTEGER NOT NULL,
 name TEXT NOT NULL,
 reference TEXT NOT NULL,
 PRIMARY KEY(repository_id,album_id)
);
CREATE TRIGGER cold_sync_file_insert AFTER INSERT ON files BEGIN
 INSERT INTO cold_sync_revisions(file_id) VALUES(NEW.id);
END;
CREATE TRIGGER cold_sync_file_update AFTER UPDATE OF name,orig_name,ext,mime,size,sha256,kind,width,height,duration,key_wrapped,key_nonce,crypto_ver,album_id,archive_state,has_thumb,has_bthumb,hidden,purge_state,cold_backend ON files BEGIN
 UPDATE cold_sync_revisions SET revision=revision+1 WHERE file_id=NEW.id;
END;
CREATE TRIGGER cold_sync_chunk_insert AFTER INSERT ON file_chunks BEGIN
 UPDATE cold_sync_revisions SET revision=revision+1 WHERE file_id=NEW.file_id;
END;
CREATE TRIGGER cold_sync_chunk_update AFTER UPDATE ON file_chunks BEGIN
 UPDATE cold_sync_revisions SET revision=revision+1 WHERE file_id=NEW.file_id;
END;
CREATE TRIGGER cold_sync_chunk_delete AFTER DELETE ON file_chunks BEGIN
 UPDATE cold_sync_revisions SET revision=revision+1 WHERE file_id=OLD.file_id;
END;
CREATE TRIGGER cold_sync_album_rename AFTER UPDATE OF name ON albums BEGIN
 UPDATE cold_sync_revisions SET revision=revision+1 WHERE file_id IN (SELECT id FROM files WHERE album_id=NEW.id);
END;
ALTER TABLE tasks ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;
CREATE TRIGGER cold_sync_reject_purging_reference BEFORE INSERT ON tasks
 WHEN NEW.type NOT IN ('purge_file','unpublish','cleanup_converted') AND EXISTS(SELECT 1 FROM files WHERE id=NEW.file_id AND purge_state IS NOT NULL)
 BEGIN SELECT RAISE(ABORT,'文件正在删除，不能创建引用任务'); END;
`,
	`
ALTER TABLE files ADD COLUMN naming_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE file_name_counters(
 date TEXT PRIMARY KEY,
 last_sequence INTEGER NOT NULL CHECK(last_sequence BETWEEN 1 AND 9999)
);
`,
	uploaderSchema,
	storageSchema,
	storageLocationSchema,
	tempCleanupFairnessSchema,
	activitySchema,
	accountStorageSchema,
	userManagementSchema,
	localImportSchema,
	accountDeleteSchema,
	twoFactorSchema,
	mediaReplicaSchema,
	mediaTimeSchema,
	taskOperationSchema,
	explicitTaskOperationSchema,
	logVisibilitySchema,
	systemScheduleSchema,
	fileOrderSchema,
	activityContextSchema,
	manualMediaTimeSchema,
	exhibitionSchema,
	fileExhibitionSchema,
}

func (db *DB) migrate() error {
	cur, err := ReadSchemaVersion(db.DB)
	if err != nil {
		return err
	}
	if cur == CurrentSchemaVersion() {
		return nil
	}
	// All pending steps form one transaction. A failure at a later version must
	// not leave an earlier step committed against the operator's old data.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version(v INTEGER NOT NULL)`); err != nil {
		return err
	}
	for i, m := range migrations {
		v := i + 1
		if v <= cur {
			continue
		}
		if _, err := tx.Exec(m); err != nil {
			return fmt.Errorf("迁移 v%d: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES(?)`, v); err != nil {
			return err
		}
	}
	var check string
	if err = tx.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("升级数据库校验失败: %s", check)
	}
	return tx.Commit()
}

func CurrentSchemaVersion() int { return len(migrations) }

// ReadSchemaVersion performs no writes and rejects a database newer than this binary.
func ReadSchemaVersion(db *sql.DB) (int, error) {
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		var tables int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return 0, err
		}
		if tables != 0 {
			return 0, fmt.Errorf("数据库缺少版本标记，拒绝自动初始化已有数据")
		}
		return 0, nil
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&cur); err != nil {
		return 0, err
	}
	if cur < 0 || cur > CurrentSchemaVersion() {
		return cur, fmt.Errorf("数据库版本 v%d 不受当前程序支持（最高 v%d），请使用对应的新版本；回退请恢复升级前备份", cur, CurrentSchemaVersion())
	}
	return cur, nil
}

func now() int64 { return time.Now().Unix() }
