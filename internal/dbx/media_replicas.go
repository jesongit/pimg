package dbx

import (
	"encoding/json"
	"strings"
)

const mediaReplicaSchema = `
ALTER TABLE files ADD COLUMN media_version INTEGER NOT NULL DEFAULT 0;
-- Existing automatic rules always requested encrypted intake before this column.
-- New rules explicitly insert the user's intent, including false.
ALTER TABLE import_rules ADD COLUMN encrypted INTEGER NOT NULL DEFAULT 1;
CREATE TABLE media_replicas(
 file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
 instance_id TEXT NOT NULL REFERENCES storage_instances(id),
 policy TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(file_id,instance_id));
CREATE TABLE media_parts(
 file_id TEXT NOT NULL, instance_id TEXT NOT NULL, idx INTEGER NOT NULL, metadata TEXT NOT NULL,
 PRIMARY KEY(file_id,instance_id,idx),
 FOREIGN KEY(file_id,instance_id) REFERENCES media_replicas(file_id,instance_id) ON DELETE CASCADE);
CREATE TABLE storage_health(instance_id TEXT PRIMARY KEY,available INTEGER,latency_ms REAL NOT NULL DEFAULT 0,bytes_per_second REAL NOT NULL DEFAULT 0,checked_at INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '');
CREATE TABLE media_publications(file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,backend TEXT NOT NULL,object_key TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,mime TEXT NOT NULL,encrypted INTEGER NOT NULL,payload_size INTEGER NOT NULL,payload_sha256 TEXT NOT NULL,ready INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(file_id,backend,object_key));
CREATE TABLE media_temporary(object_id TEXT PRIMARY KEY REFERENCES temp_objects(id) ON DELETE CASCADE,mime TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,encrypted INTEGER NOT NULL,payload_size INTEGER NOT NULL,payload_sha256 TEXT NOT NULL);
`

// MediaReplica records the immutable write policy of one representation.
// Configuration changes never reinterpret an object that is already stored.
type MediaReplica struct {
	FileID     string      `json:"file_id"`
	InstanceID string      `json:"instance_id"`
	Encrypted  bool        `json:"encrypted"`
	Split      bool        `json:"split"`
	PartMB     int         `json:"part_mb"`
	State      string      `json:"state"`
	Error      string      `json:"error,omitempty"`
	Parts      []MediaPart `json:"parts"`
}

type MediaPart struct {
	Index         int     `json:"index"`
	Count         int     `json:"count"`
	Start         float64 `json:"start"`
	Duration      float64 `json:"duration"`
	Ext           string  `json:"ext"`
	MIME          string  `json:"mime"`
	Size          int64   `json:"size"`
	SHA256        string  `json:"sha256"`
	PayloadSize   int64   `json:"payload_size"`
	StoredSize    int64   `json:"stored_size"`
	LocalModNS    int64   `json:"local_mod_ns,omitempty"`
	PayloadSHA256 string  `json:"payload_sha256"`
	Reference     string  `json:"reference"`
	Verified      bool    `json:"verified"`
	Original      bool    `json:"original"`
}

func (db *DB) MediaReplicas(id string) ([]MediaReplica, error) {
	all, err := db.FileMediaReplicas([]string{id})
	return all[id], err
}

// Fetch each page's immutable replica policies and part metadata once. Reads
// use the WAL reader pool rather than competing for the single writer.
func (db *DB) FileMediaReplicas(ids []string) (map[string][]MediaReplica, error) {
	out := make(map[string][]MediaReplica, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	rows, err := db.ReadQuery(`SELECT file_id,instance_id,policy,state,error FROM media_replicas WHERE file_id IN (`+marks+`) ORDER BY file_id,instance_id`, args...)
	if err != nil {
		return nil, err
	}
	type location struct {
		file  string
		index int
	}
	positions := map[string]location{}
	for rows.Next() {
		var r MediaReplica
		var policy string
		if err = rows.Scan(&r.FileID, &r.InstanceID, &policy, &r.State, &r.Error); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(policy), &r); err != nil {
			rows.Close()
			return nil, err
		}
		positions[r.FileID+"\x00"+r.InstanceID] = location{r.FileID, len(out[r.FileID])}
		out[r.FileID] = append(out[r.FileID], r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.ReadQuery(`SELECT file_id,instance_id,metadata FROM media_parts WHERE file_id IN (`+marks+`) ORDER BY file_id,instance_id,idx`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fileID, instanceID, data string
		var p MediaPart
		if err = rows.Scan(&fileID, &instanceID, &data); err == nil {
			err = json.Unmarshal([]byte(data), &p)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		if pos, ok := positions[fileID+"\x00"+instanceID]; ok {
			out[pos.file][pos.index].Parts = append(out[pos.file][pos.index].Parts, p)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (db *DB) EnsureMediaReplica(r MediaReplica) error {
	policy := map[string]any{"encrypted": r.Encrypted, "split": r.Split, "part_mb": r.PartMB}
	_, err := db.Exec(`INSERT INTO media_replicas(file_id,instance_id,policy) VALUES(?,?,?) ON CONFLICT DO NOTHING`, r.FileID, r.InstanceID, JSONValue(policy))
	return err
}
func (db *DB) SaveMediaPart(id, instance string, p MediaPart) error {
	_, err := db.Exec(`INSERT INTO media_parts(file_id,instance_id,idx,metadata) VALUES(?,?,?,?) ON CONFLICT(file_id,instance_id,idx) DO UPDATE SET metadata=excluded.metadata`, id, instance, p.Index, JSONValue(p))
	return err
}
func (db *DB) SetMediaReplicaState(id, instance, state, message string) error {
	_, err := db.Exec(`UPDATE media_replicas SET state=?,error=? WHERE file_id=? AND instance_id=?`, state, message, id, instance)
	return err
}
