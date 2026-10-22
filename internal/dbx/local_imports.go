package dbx

import (
	"database/sql"
	"errors"
)

const TaskLocalImportScan = "local_import_scan"
const TaskLocalImport = "local_import"
const TaskLocalImportIdle = "local_import_idle"

// Old batches always belonged to the original administrator. New batches
// persist their owner in the existing immutable context, independent of albums.
func (db *DB) LocalImportOwner(id int64) (string, error) {
	var owner string
	err := db.QueryRow(`SELECT COALESCE(NULLIF(json_extract(context,'$.owner_id'),''),'admin:1') FROM local_import_batches WHERE id=?`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

const localImportSchema = `
CREATE TABLE upload_policies(file_id TEXT PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,auto_publish INTEGER NOT NULL);
CREATE TABLE input_groups(id TEXT PRIMARY KEY,owner_id TEXT NOT NULL,sha256 TEXT NOT NULL,size INTEGER NOT NULL,encrypted INTEGER NOT NULL);
CREATE INDEX input_group_fingerprint ON input_groups(owner_id,sha256,size,encrypted);
CREATE TABLE input_group_files(group_id TEXT NOT NULL REFERENCES input_groups(id),file_id TEXT NOT NULL,part_index INTEGER NOT NULL,PRIMARY KEY(group_id,part_index));
CREATE TABLE local_import_batches(id INTEGER PRIMARY KEY AUTOINCREMENT,root TEXT NOT NULL,paths TEXT NOT NULL,album_id INTEGER NOT NULL,encrypted INTEGER NOT NULL,recursive INTEGER NOT NULL,schedule TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'scanning',error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT(unixepoch()));
CREATE TABLE local_import_items(id INTEGER PRIMARY KEY AUTOINCREMENT,batch_id INTEGER NOT NULL REFERENCES local_import_batches(id),path TEXT NOT NULL,size INTEGER NOT NULL,version TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',file_ids TEXT NOT NULL DEFAULT '[]',error TEXT NOT NULL DEFAULT '',UNIQUE(batch_id,path));
CREATE INDEX local_import_work ON local_import_items(batch_id,state,id);
`

// Missing policies belong to pre-upgrade files and preserve explicit legacy
// direct-upload intent. New private uploads always persist auto_publish=0.
func (db *DB) UploadAutoPublish(id string) (bool, error) {
	var publish bool
	err := db.QueryRow(`SELECT auto_publish FROM upload_policies WHERE file_id=?`, id).Scan(&publish)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return publish, err
}

// InputDuplicate compares original bytes, before video remux/transcode/split.
// Every part must remain usable; an encrypted and plaintext copy are distinct.
func (db *DB) InputDuplicate(owner, hash string, size int64, encrypted bool) ([]*File, error) {
	var group string
	err := db.QueryRow(`SELECT g.id FROM input_groups g WHERE g.owner_id=? AND g.sha256=? AND g.size=? AND g.encrypted=?
 AND NOT EXISTS(SELECT 1 FROM input_group_files x LEFT JOIN files f ON f.id=x.file_id WHERE x.group_id=g.id AND (f.id IS NULL OR f.hidden=1 OR f.purge_state IS NOT NULL OR f.archive_state='failed' OR EXISTS(SELECT 1 FROM file_conversions c WHERE c.phase!='done' AND (c.source_id=f.id OR c.target_id=f.id)))) ORDER BY g.rowid LIMIT 1`, owner, hash, size, encrypted).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT file_id FROM input_group_files WHERE group_id=? ORDER BY part_index`, group)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []*File{}
	for _, id := range ids {
		f, e := db.GetVisibleFile(id)
		if e != nil {
			return nil, e
		}
		if f.ArchiveState != "done" {
			return nil, ErrImagePending
		}
		f.UploadDuplicate = true
		out = append(out, f)
	}
	return out, nil
}

func (db *DB) BindInputSource(source string, files []*File) error {
	if source == "" {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, f := range files {
		if _, err = tx.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, source, i, f.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func registerInputGroup(tx *sql.Tx, files []*File) error {
	f := files[0]
	if f.InputHash == "" {
		return nil
	}
	// The intake write lock serializes this check with all other insertions.
	if f.Kind == "video" {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM input_groups g WHERE owner_id=? AND sha256=? AND size=? AND encrypted=? AND NOT EXISTS(SELECT 1 FROM input_group_files x LEFT JOIN files f ON f.id=x.file_id WHERE x.group_id=g.id AND (f.id IS NULL OR f.hidden=1 OR f.purge_state IS NOT NULL OR f.archive_state='failed')))`, f.OwnerID, f.InputHash, f.InputSize, f.Storage != "direct").Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrImagePending
		}
	}
	if _, err := tx.Exec(`INSERT INTO input_groups(id,owner_id,sha256,size,encrypted) VALUES(?,?,?,?,?)`, f.ID, f.OwnerID, f.InputHash, f.InputSize, f.Storage != "direct"); err != nil {
		return err
	}
	for i, p := range files {
		if _, err := tx.Exec(`INSERT INTO input_group_files(group_id,file_id,part_index) VALUES(?,?,?)`, f.ID, p.ID, i); err != nil {
			return err
		}
	}
	return nil
}
