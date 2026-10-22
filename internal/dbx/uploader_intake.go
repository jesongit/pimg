package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

type UploaderPart struct {
	Name                                                   string
	InstanceID, KeysetID, UploadID, ToolFileID, Mode, Kind string
	Index, Parts                                           int
	AccountID, ChatID, AlbumID                             int64
	TopicID, MessageID                                     int
	Metadata                                               string
	Reference                                              string
	WireSHA256, PayloadSHA256                              string
	WireSize, PayloadSize                                  int64
	Chunk                                                  Chunk
}

func (p UploaderPart) SourceKey() string { return p.InstanceID + ":" + p.KeysetID + ":" + p.ToolFileID }
func (p UploaderPart) Event(typ, reason, canonical string, album int64) IngestEvent {
	details, _ := json.Marshal(map[string]any{"source": "uploader", "instance_id": p.InstanceID, "keyset_id": p.KeysetID, "account_id": p.AccountID, "chat_id": p.ChatID, "topic_id": p.TopicID, "message_id": p.MessageID, "requested_album_id": p.AlbumID, "retained_album_id": album, "mode": p.Mode, "verification": "declared"})
	return IngestEvent{SourceKey: p.SourceKey(), Type: typ, Reason: reason, FileID: canonical, ToolFileID: p.ToolFileID, UploadID: p.UploadID, Body: details}
}

type UploaderReceipt struct{ State, CanonicalID, Metadata string }

func (db *DB) UploaderReceipt(instance, keyset, file string) (UploaderReceipt, error) {
	var r UploaderReceipt
	err := db.QueryRow(`SELECT state,canonical_id,metadata FROM uploader_files WHERE instance_id=? AND keyset_id=? AND tool_file_id=?`, instance, keyset, file).Scan(&r.State, &r.CanonicalID, &r.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

func (db *DB) StageUploaderPart(p UploaderPart, partIndex, partCount int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return err
	}
	var state, metadata, upload, mode string
	var album, account, chat int64
	var topic, pi, pc int
	err = tx.QueryRow(`SELECT state,metadata,upload_id,mode,album_id,account_id,chat_id,topic_id,part_index,part_count FROM uploader_files WHERE instance_id=? AND keyset_id=? AND tool_file_id=?`, p.InstanceID, p.KeysetID, p.ToolFileID).Scan(&state, &metadata, &upload, &mode, &album, &account, &chat, &topic, &pi, &pc)
	if err == nil {
		if state == "retired" {
			return ErrUploaderRetired
		}
		if upload != p.UploadID || mode != p.Mode || album != p.AlbumID || account != p.AccountID || chat != p.ChatID || topic != p.TopicID || (metadata != "" && p.Metadata != "" && metadata != p.Metadata) || (pc > 0 && partCount > 0 && (pi != partIndex || pc != partCount)) {
			return ErrUploaderConflict
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else {
		_, err = tx.Exec(`INSERT INTO uploader_files(instance_id,keyset_id,tool_file_id,upload_id,album_id,account_id,chat_id,topic_id,mode,metadata,part_index,part_count,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.InstanceID, p.KeysetID, p.ToolFileID, p.UploadID, p.AlbumID, p.AccountID, p.ChatID, p.TopicID, p.Mode, p.Metadata, partIndex, partCount, now(), now())
		if err != nil {
			return err
		}
	}
	if partCount > 0 {
		var conflict bool
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM uploader_files WHERE instance_id=? AND keyset_id=? AND upload_id=? AND tool_file_id!=? AND (mode!=? OR album_id!=? OR account_id!=? OR chat_id!=? OR topic_id!=? OR (part_count>0 AND (part_count!=? OR part_index=?))))`, p.InstanceID, p.KeysetID, p.UploadID, p.ToolFileID, p.Mode, p.AlbumID, p.AccountID, p.ChatID, p.TopicID, partCount, partIndex).Scan(&conflict)
		if err != nil {
			return err
		}
		if conflict {
			return ErrUploaderConflict
		}
	}
	var oldJSON string
	err = tx.QueryRow(`SELECT body FROM uploader_parts WHERE instance_id=? AND keyset_id=? AND tool_file_id=? AND kind=? AND idx=?`, p.InstanceID, p.KeysetID, p.ToolFileID, p.Kind, p.Index).Scan(&oldJSON)
	if err == nil {
		var old UploaderPart
		if json.Unmarshal([]byte(oldJSON), &old) != nil {
			return ErrUploaderConflict
		}
		if old.Parts != p.Parts || old.WireSHA256 != p.WireSHA256 || old.PayloadSHA256 != p.PayloadSHA256 || old.WireSize != p.WireSize || old.PayloadSize != p.PayloadSize {
			return ErrUploaderConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if state == "done" || state == "duplicate" {
			return ErrUploaderConflict
		}
		var countConflict bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM uploader_parts WHERE instance_id=? AND keyset_id=? AND tool_file_id=? AND kind=? AND parts!=?)`, p.InstanceID, p.KeysetID, p.ToolFileID, p.Kind, p.Parts).Scan(&countConflict); err != nil {
			return err
		}
		if countConflict {
			return ErrUploaderConflict
		}
		body, e := json.Marshal(p)
		if e != nil {
			return e
		}
		_, err = tx.Exec(`INSERT INTO uploader_parts VALUES(?,?,?,?,?,?,?,?,?,?)`, p.InstanceID, p.KeysetID, p.ToolFileID, p.Kind, p.Index, p.Parts, p.AccountID, p.ChatID, p.MessageID, string(body))
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if p.Metadata != "" {
		_, err = tx.Exec(`UPDATE uploader_files SET metadata=?,part_index=?,part_count=?,updated_at=? WHERE instance_id=? AND keyset_id=? AND tool_file_id=?`, p.Metadata, partIndex, partCount, now(), p.InstanceID, p.KeysetID, p.ToolFileID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) UploaderGroup(p UploaderPart) ([]UploaderPart, error) {
	rows, err := db.Query(`SELECT p.body,f.metadata FROM uploader_parts p JOIN uploader_files f ON f.instance_id=p.instance_id AND f.keyset_id=p.keyset_id AND f.tool_file_id=p.tool_file_id WHERE f.instance_id=? AND f.keyset_id=? AND f.upload_id=? ORDER BY f.part_index,p.kind,p.idx`, p.InstanceID, p.KeysetID, p.UploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UploaderPart{}
	for rows.Next() {
		var b, m string
		if err = rows.Scan(&b, &m); err != nil {
			return nil, err
		}
		var part UploaderPart
		if err = json.Unmarshal([]byte(b), &part); err != nil {
			return nil, err
		}
		part.Metadata = m
		out = append(out, part)
	}
	return out, rows.Err()
}

// Store validates and prepares every small preview before committing a group.
// This transaction rechecks retirement, binding, dedupe and final numbering.
func (db *DB) AdoptUploaderGroup(parts []UploaderPart, files []*File) (map[string]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return nil, err
	}
	result := map[string]string{}
	sort.Slice(files, func(i, j int) bool { return files[i].Naming.PartIndex < files[j].Naming.PartIndex })
	for _, f := range files {
		var p *UploaderPart
		for i := range parts {
			if parts[i].ToolFileID == f.ID {
				p = &parts[i]
				break
			}
		}
		if p == nil {
			return nil, ErrUploaderConflict
		}
		var state, canonical string
		var bound bool
		if err = tx.QueryRow(`SELECT state,canonical_id FROM uploader_files WHERE instance_id=? AND keyset_id=? AND tool_file_id=?`, p.InstanceID, p.KeysetID, p.ToolFileID).Scan(&state, &canonical); err != nil {
			return nil, err
		}
		if state == "retired" {
			return nil, ErrUploaderRetired
		}
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND account_id=? AND chat_id=? AND topic_id=? AND state='active')`, p.AlbumID, p.AccountID, p.ChatID, p.TopicID).Scan(&bound); err != nil {
			return nil, err
		}
		if !bound {
			return nil, ErrAlbumFrozen
		}
		if state == "done" || state == "duplicate" {
			result[f.ID] = canonical
			continue
		}
		candidate, e := imageCandidate(tx, f)
		if e != nil {
			return nil, e
		}
		eventType := "adopted"
		retainedAlbum := f.AlbumID.Int64
		if candidate != nil {
			canonical = candidate.ID
			retainedAlbum = candidate.AlbumID.Int64
			eventType = "duplicate"
		} else {
			f.Naming.Sequence = 0
			f.Name = ""
			if err = insertFile(tx, f); err != nil {
				return nil, err
			}
			canonical = f.ID
			for _, part := range parts {
				if part.ToolFileID != f.ID {
					continue
				}
				c := part.Chunk
				if _, err = tx.Exec(`INSERT INTO file_chunks(file_id,kind,idx,message_id,tg_file_id,object_key,size,sha256,uploaded_at) VALUES(?,?,?,?,?,?,?,?,?)`, f.ID, part.Kind, part.Index, part.MessageID, c.TgFileID, c.ObjectKey, part.PayloadSize, part.PayloadSHA256, now()); err != nil {
					return nil, err
				}
			}
			if _, err = tx.Exec(`UPDATE files SET has_thumb=?,has_bthumb=?,thumb_w=?,thumb_h=?,thumb_mime='image/jpeg',thumb_size=?,archived_at=? WHERE id=?`, f.HasThumb, f.HasBThumb, f.ThumbW, f.ThumbH, f.ThumbSize, now(), f.ID); err != nil {
				return nil, err
			}
		}
		storedState := "done"
		if candidate != nil {
			storedState = "duplicate"
		}
		if _, err = tx.Exec(`UPDATE uploader_files SET state=?,canonical_id=?,last_error='',updated_at=? WHERE instance_id=? AND keyset_id=? AND tool_file_id=?`, storedState, canonical, now(), p.InstanceID, p.KeysetID, p.ToolFileID); err != nil {
			return nil, err
		}
		for _, part := range parts {
			if part.ToolFileID == f.ID {
				if _, err = tx.Exec(`UPDATE tg_messages SET state='managed',file_id=?,last_error='',updated_at=? WHERE account_id=? AND chat_id=? AND message_id=? AND state!='retired'`, canonical, now(), part.AccountID, part.ChatID, part.MessageID); err != nil {
					return nil, err
				}
			}
		}
		event := p.Event(eventType, "", canonical, retainedAlbum)
		var detail map[string]any
		_ = json.Unmarshal(event.Body, &detail)
		detail["sha256"] = f.SHA256
		detail["size"] = f.Size
		event.Body, _ = json.Marshal(detail)
		if err = appendIngestEvent(tx, event); err != nil {
			return nil, err
		}
		result[f.ID] = canonical
	}
	return result, tx.Commit()
}

func localIntakeEvent(f *File, typ, canonical string, retainedAlbum int64) IngestEvent {
	body, _ := json.Marshal(map[string]any{"source": "pimg", "name": f.Name, "orig_name": f.OrigName, "requested_album_id": f.AlbumID.Int64, "retained_album_id": retainedAlbum, "mode": f.Storage, "sha256": f.SHA256, "size": f.Size, "verification": "verified_local_bytes", "telegram_source": f.TelegramSource, "operation_id": f.OperationID})
	return IngestEvent{SourceKey: "pimg:" + f.ID, Type: typ, FileID: canonical, Body: body}
}

func (db *DB) FailUploader(p UploaderPart, reason string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE uploader_files SET last_error=?,updated_at=? WHERE instance_id=? AND keyset_id=? AND tool_file_id=? AND state='pending'`, reason, now(), p.InstanceID, p.KeysetID, p.ToolFileID); err != nil {
		return err
	}
	if err = appendIngestEvent(tx, p.Event("failed", reason, "", 0)); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) UploaderPendingCount() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM uploader_files WHERE state='pending'`).Scan(&n)
	return n, err
}
