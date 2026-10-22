package dbx

import "encoding/json"

func (db *DB) RecordFileVerification(f *File, result error) error {
	typ, reason, status := "verified", "", "verified"
	if result != nil {
		typ, reason, status = "failed", "原件恢复或完整性校验失败", "failed"
	}
	body, _ := json.Marshal(map[string]any{"source": "pimg_verification", "sha256": f.SHA256, "size": f.Size, "verification": status, "retained_album_id": f.AlbumID.Int64, "mode": f.Storage})
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return err
	}
	event := IngestEvent{SourceKey: "verify:" + f.ID, Type: typ, Reason: reason, FileID: f.ID, Body: body}
	var instance, keyset string
	e := tx.QueryRow(`SELECT instance_id,keyset_id,tool_file_id,upload_id FROM uploader_files WHERE canonical_id=? AND state='done' LIMIT 1`, f.ID).Scan(&instance, &keyset, &event.ToolFileID, &event.UploadID)
	if e == nil {
		var details map[string]any
		_ = json.Unmarshal(body, &details)
		details["instance_id"] = instance
		details["keyset_id"] = keyset
		event.Body, _ = json.Marshal(details)
	}
	if err = appendIngestEvent(tx, event); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE files SET archive_error=? WHERE id=?`, reason, f.ID); err != nil {
		return err
	}
	return tx.Commit()
}
