package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
)

const storageSchema = `
CREATE TABLE storage_instances(id TEXT PRIMARY KEY,name TEXT NOT NULL,driver TEXT NOT NULL,config BLOB NOT NULL,enabled INTEGER NOT NULL DEFAULT 1);
CREATE TABLE storage_album_topics(instance_id TEXT NOT NULL REFERENCES storage_instances(id),album_id INTEGER NOT NULL,topic_id INTEGER NOT NULL,name TEXT NOT NULL,PRIMARY KEY(instance_id,album_id));
CREATE TABLE archive_objects(
 file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,instance_id TEXT NOT NULL REFERENCES storage_instances(id),
 kind TEXT NOT NULL,idx INTEGER NOT NULL,reference TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,verified INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(file_id,instance_id,kind,idx));
CREATE INDEX archive_objects_instance ON archive_objects(instance_id,file_id);
CREATE TABLE archive_manifests(file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,instance_id TEXT NOT NULL REFERENCES storage_instances(id),reference TEXT NOT NULL,revision TEXT NOT NULL,PRIMARY KEY(file_id,instance_id));
CREATE TABLE archive_manifest_jobs(file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,instance_id TEXT NOT NULL REFERENCES storage_instances(id),revision TEXT NOT NULL,body BLOB NOT NULL,reference TEXT NOT NULL DEFAULT '',PRIMARY KEY(file_id,instance_id,revision));
CREATE TABLE archive_garbage(file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,instance_id TEXT NOT NULL REFERENCES storage_instances(id),reference TEXT NOT NULL,PRIMARY KEY(file_id,instance_id,reference));
CREATE TABLE import_rules(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,instance_id TEXT NOT NULL REFERENCES storage_instances(id),source TEXT NOT NULL,album_id INTEGER NOT NULL REFERENCES albums(id),recursive INTEGER NOT NULL DEFAULT 0,interval_minutes INTEGER NOT NULL DEFAULT 0,enabled INTEGER NOT NULL DEFAULT 1,cursor TEXT NOT NULL DEFAULT '',next_scan INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '');
CREATE TABLE import_receipts(rule_id INTEGER NOT NULL REFERENCES import_rules(id),locator TEXT NOT NULL,version TEXT NOT NULL,entry TEXT NOT NULL DEFAULT '{}',file_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL DEFAULT 'pending',error TEXT NOT NULL DEFAULT '',PRIMARY KEY(rule_id,locator,version));
CREATE INDEX import_receipt_file ON import_receipts(file_id);
`

const storageLocationSchema = `
ALTER TABLE archive_objects ADD COLUMN album_id INTEGER NOT NULL DEFAULT 0;
CREATE TRIGGER import_file_deleted AFTER DELETE ON files BEGIN
 UPDATE import_receipts SET state='deleted',error='Pimg 记录已删除，保留来源不再自动导入' WHERE file_id=OLD.id;
END;
`

const tempCleanupFairnessSchema = `
ALTER TABLE temp_objects ADD COLUMN cleanup_attempted_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX temp_cleanup_order ON temp_objects(cleanup_attempted_at,expires_at,id) WHERE deleted_at IS NULL;
`

type StorageInstance struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Driver  string `json:"driver"`
	Config  []byte `json:"-"`
	Enabled bool   `json:"enabled"`
}

func (db *DB) StorageInstances() ([]StorageInstance, error) {
	rows, err := db.Query(`SELECT id,name,driver,config,enabled FROM storage_instances ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageInstance{}
	for rows.Next() {
		var i StorageInstance
		if err = rows.Scan(&i.ID, &i.Name, &i.Driver, &i.Config, &i.Enabled); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (db *DB) SaveStorageInstance(i StorageInstance) error {
	_, err := db.Exec(`INSERT INTO storage_instances(id,name,driver,config,enabled) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,config=excluded.config,enabled=excluded.enabled`, i.ID, i.Name, i.Driver, i.Config, i.Enabled)
	return err
}

type ArchiveObject struct {
	AlbumID    int64  `json:"album_id"`
	FileID     string `json:"file_id"`
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"`
	Idx        int    `json:"idx"`
	Reference  string `json:"reference"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Verified   bool   `json:"verified"`
}

func (db *DB) ArchiveObjects(fileID, instance string) ([]ArchiveObject, error) {
	rows, err := db.Query(`SELECT file_id,instance_id,kind,idx,reference,size,sha256,verified,album_id FROM archive_objects WHERE file_id=? AND (?='' OR instance_id=?) ORDER BY instance_id,kind,idx`, fileID, instance, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArchiveObject{}
	for rows.Next() {
		var o ArchiveObject
		if err = rows.Scan(&o.FileID, &o.InstanceID, &o.Kind, &o.Idx, &o.Reference, &o.Size, &o.SHA256, &o.Verified, &o.AlbumID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (db *DB) SaveArchiveObject(o ArchiveObject) error {
	_, err := db.Exec(`INSERT INTO archive_objects(file_id,instance_id,kind,idx,reference,size,sha256,verified,album_id) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(file_id,instance_id,kind,idx) DO UPDATE SET reference=excluded.reference,size=excluded.size,sha256=excluded.sha256,verified=excluded.verified,album_id=excluded.album_id`, o.FileID, o.InstanceID, o.Kind, o.Idx, o.Reference, o.Size, o.SHA256, o.Verified, o.AlbumID)
	return err
}

const TaskArchiveSync = "archive_sync"
const TaskSourceScan = "source_scan"
const TaskSourceCleanup = "source_cleanup"
const TaskStorageProbe = "storage_probe"

func (db *DB) EnqueueUniqueTask(typ, file, payload string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if typ == TaskArchiveSync {
		var deleting bool
		if err = tx.QueryRow(`SELECT purge_state IS NOT NULL OR hidden=1 FROM files WHERE id=?`, file).Scan(&deleting); err != nil {
			return 0, err
		}
		if deleting {
			return 0, ErrDeleting
		}
	}
	var id int64
	err = tx.QueryRow(`SELECT id FROM tasks WHERE type=? AND COALESCE(file_id,'')=? AND payload=? AND (state IN ('pending','running','failed') OR (type='archive_sync' AND state='canceled')) ORDER BY id DESC LIMIT 1`, typ, file, payload).Scan(&id)
	if err == nil {
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,max_attempts,run_after,created_at,updated_at) VALUES(?,?,?,'pending',1000000,?,?,?)`, typ, file, payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachTaskOperation(tx, id, typ, file, payload); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

type ImportRule struct {
	Encrypted       bool   `json:"encrypted"`
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	InstanceID      string `json:"instance_id"`
	Source          string `json:"source"`
	AlbumID         int64  `json:"album_id"`
	Recursive       bool   `json:"recursive"`
	IntervalMinutes int    `json:"interval_minutes"`
	Enabled         bool   `json:"enabled"`
	Cursor          string `json:"cursor"`
	NextScan        int64  `json:"next_scan"`
	LastError       string `json:"last_error"`
}

func (db *DB) ImportRules() ([]ImportRule, error) {
	rows, err := db.Query(`SELECT id,name,instance_id,source,album_id,recursive,interval_minutes,enabled,cursor,next_scan,last_error,encrypted FROM import_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportRule{}
	for rows.Next() {
		var r ImportRule
		if err = rows.Scan(&r.ID, &r.Name, &r.InstanceID, &r.Source, &r.AlbumID, &r.Recursive, &r.IntervalMinutes, &r.Enabled, &r.Cursor, &r.NextScan, &r.LastError, &r.Encrypted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (db *DB) ImportRule(id int64) (ImportRule, error) {
	rs, e := db.ImportRules()
	for _, r := range rs {
		if r.ID == id {
			return r, e
		}
	}
	if e != nil {
		return ImportRule{}, e
	}
	return ImportRule{}, ErrNotFound
}
func (db *DB) SaveImportRule(r ImportRule) (int64, error) {
	if r.ID > 0 {
		res, e := db.Exec(`UPDATE import_rules SET name=?,instance_id=?,source=?,album_id=?,recursive=?,interval_minutes=?,enabled=?,encrypted=?,cursor='',next_scan=0 WHERE id=?`, r.Name, r.InstanceID, r.Source, r.AlbumID, r.Recursive, r.IntervalMinutes, r.Enabled, r.Encrypted, r.ID)
		if e != nil {
			return 0, e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return 0, ErrNotFound
		}
		return r.ID, nil
	}
	res, e := db.Exec(`INSERT INTO import_rules(name,instance_id,source,album_id,recursive,interval_minutes,enabled,encrypted) VALUES(?,?,?,?,?,?,?,?)`, r.Name, r.InstanceID, r.Source, r.AlbumID, r.Recursive, r.IntervalMinutes, r.Enabled, r.Encrypted)
	if e != nil {
		return 0, e
	}
	return res.LastInsertId()
}
func (db *DB) QueueSourceScan(id int64) (int64, error) {
	return db.EnqueueUniqueTask(TaskSourceScan, "", TaskPayloadJSON(map[string]any{"rule_id": id}))
}
func (db *DB) StorageStats(fileID string) (map[string]any, error) {
	objects, err := db.ArchiveObjects(fileID, "")
	if err != nil {
		return nil, err
	}
	copies := map[string]int{}
	for _, o := range objects {
		if o.Verified {
			copies[o.InstanceID]++
		}
	}
	return map[string]any{"verified_objects": copies}, nil
}
func JSONValue(v any) string { b, _ := json.Marshal(v); return string(b) }
