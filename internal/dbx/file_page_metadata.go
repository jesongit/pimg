package dbx

import "strings"

type FileArchiveMetadata struct {
	Copies map[string]int
	Chunks map[string][]*Chunk
}

func (db *DB) FileArchiveMetadata(ids []string) (map[string]*FileArchiveMetadata, error) {
	out := make(map[string]*FileArchiveMetadata, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
		out[id] = &FileArchiveMetadata{Copies: map[string]int{}, Chunks: map[string][]*Chunk{}}
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	rows, err := db.ReadQuery(`SELECT file_id,instance_id,COUNT(*) FROM archive_objects WHERE file_id IN (`+marks+`) AND kind='data' AND verified=1 GROUP BY file_id,instance_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, instance string
		var n int
		if err = rows.Scan(&id, &instance, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out[id].Copies[instance] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.ReadQuery(`SELECT id,file_id,kind,idx,message_id,tg_file_id,object_key,size,sha256,uploaded_at FROM file_chunks WHERE file_id IN (`+marks+`) ORDER BY file_id,kind,idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c := &Chunk{}
		if err = rows.Scan(&c.ID, &c.FileID, &c.Kind, &c.Idx, &c.MessageID, &c.TgFileID, &c.ObjectKey, &c.Size, &c.SHA256, &c.UploadedAt); err != nil {
			return nil, err
		}
		out[c.FileID].Chunks[c.Kind] = append(out[c.FileID].Chunks[c.Kind], c)
	}
	return out, rows.Err()
}
