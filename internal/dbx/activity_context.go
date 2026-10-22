package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const activityContextSchema = `ALTER TABLE task_operations ADD COLUMN context TEXT NOT NULL DEFAULT '{}';
ALTER TABLE local_import_batches ADD COLUMN context TEXT NOT NULL DEFAULT '{}';`

type ActivityAlbum struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type ActivityContext struct {
	OwnerID     string          `json:"owner_id,omitempty"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source,omitempty"`
	FromAlbums  []ActivityAlbum `json:"from_albums,omitempty"`
	ToAlbum     *ActivityAlbum  `json:"to_album,omitempty"`
	Storages    []string        `json:"storages,omitempty"`
}

func snapshotTaskOutputs(tx *sql.Tx, taskID int64) error {
	var id int64
	var kind, fileID, raw string
	err := tx.QueryRow(`SELECT o.id,o.kind,COALESCE(t.file_id,''),o.context FROM task_operation_jobs j JOIN task_operations o ON o.id=j.operation_id JOIN tasks t ON t.id=j.task_id WHERE t.id=?`, taskID).Scan(&id, &kind, &fileID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if fileID == "" || kind != "publish" && kind != "archive" && kind != "upload" && kind != "sync" && kind != "telegram_receive" {
		return nil
	}
	var c ActivityContext
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return err
	}
	table, backend, ready := "media_replicas", "r.instance_id", "r.state='ready'"
	if kind == "publish" {
		table, backend, ready = "media_publications", "r.backend", "r.ready=1"
	}
	rows, err := tx.Query(`SELECT DISTINCT i.name FROM `+table+` r JOIN storage_instances i ON i.id=`+backend+` WHERE r.file_id=? AND `+ready, fileID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		found := false
		for _, old := range c.Storages {
			found = found || old == name
		}
		if !found {
			c.Storages = append(c.Storages, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_operations SET context=? WHERE id=?`, JSONValue(c), id)
	return err
}

type contextQuery interface{ QueryRow(string, ...any) *sql.Row }

func contextAlbum(q contextQuery, id int64) ActivityAlbum {
	a := ActivityAlbum{ID: id, Name: "未分类"}
	if id > 0 {
		if err := q.QueryRow(`SELECT name FROM albums WHERE id=?`, id).Scan(&a.Name); err != nil {
			a.Name = fmt.Sprintf("相册 #%d（已不可用）", id)
		}
	}
	return a
}

// Capture action-time names before a synchronous move/deletion can change them.
func snapshotOperationContext(tx *sql.Tx, id int64, fileID string) error {
	var raw, kind, scope string
	if err := tx.QueryRow(`SELECT context,kind,scope FROM task_operations WHERE id=?`, id).Scan(&raw, &kind, &scope); err != nil {
		return err
	}
	var c ActivityContext
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return err
	}
	if c.ToAlbum == nil && strings.HasPrefix(scope, "album:") {
		var album int64
		if _, err := fmt.Sscanf(scope, "album:%d", &album); err == nil {
			a := contextAlbum(tx, album)
			c.ToAlbum = &a
		}
	}
	if fileID != "" {
		var album int64
		err := tx.QueryRow(`SELECT COALESCE(album_id,0) FROM files WHERE id=?`, fileID).Scan(&album)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			a := contextAlbum(tx, album)
			if kind == "upload" && c.ToAlbum == nil {
				c.ToAlbum = &a
			}
			if kind != "upload" {
				found := false
				for _, old := range c.FromAlbums {
					found = found || old.ID == a.ID
				}
				if !found {
					c.FromAlbums = append(c.FromAlbums, a)
				}
			}
			if kind == "unpublish" {
				var name string
				if e := tx.QueryRow(`SELECT i.name FROM files f JOIN storage_instances i ON i.id=f.hot_backend WHERE f.id=?`, fileID).Scan(&name); e == nil {
					found := false
					for _, old := range c.Storages {
						found = found || old == name
					}
					if !found {
						c.Storages = append(c.Storages, name)
					}
				} else if !errors.Is(e, sql.ErrNoRows) {
					return e
				}
			}
		}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_operations SET context=? WHERE id=?`, string(body), id)
	return err
}

func SnapshotLocalImport(tx *sql.Tx, id, album int64, source string, owners ...string) error {
	a := contextAlbum(tx, album)
	owner := AdminOwner
	if len(owners) > 0 {
		owner = NormalizeOwner(owners[0])
	}
	body, err := json.Marshal(ActivityContext{OwnerID: owner, Source: source, ToAlbum: &a})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE local_import_batches SET context=? WHERE id=?`, string(body), id)
	return err
}

// Historical records have explicit unknown origins; never infer a move's old
// album from its file's current album after the operation has already finished.
func (db *DB) ActivityContext(t *ActivityTask) (ActivityContext, error) {
	var c ActivityContext
	var raw string
	var operation int64
	if t.Group != nil && t.Group.BatchID > 0 {
		var root string
		var album int64
		if err := db.QueryRow(`SELECT context,root,album_id FROM local_import_batches WHERE id=?`, t.Group.BatchID).Scan(&raw, &root, &album); err != nil {
			return c, err
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return c, err
		}
		if c.ToAlbum == nil {
			a := contextAlbum(db, album)
			c.ToAlbum = &a
			c.Source = root
		}
		if !t.LastError.Valid || t.LastError.String == "" {
			var message string
			err := db.QueryRow(`SELECT error FROM local_import_items WHERE batch_id=? AND state='failed' AND error!='' ORDER BY id LIMIT 1`, t.Group.BatchID).Scan(&message)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return c, err
			}
			if message != "" {
				t.LastError = sql.NullString{String: message, Valid: true}
			}
		}
	} else {
		var kind, scope string
		err := db.QueryRow(`SELECT o.id,o.context,o.kind,o.scope FROM task_operation_jobs j JOIN task_operations o ON o.id=j.operation_id WHERE j.task_id=?`, t.ID).Scan(&operation, &raw, &kind, &scope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return c, err
		}
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &c); err != nil {
				return c, err
			}
			if c.ToAlbum == nil && strings.HasPrefix(scope, "album:") {
				var album int64
				if _, err = fmt.Sscanf(scope, "album:%d", &album); err == nil {
					a := contextAlbum(db, album)
					c.ToAlbum = &a
				}
			}
		}
		if len(c.FromAlbums) == 0 && kind != "move_album" && t.AlbumName != "" {
			c.FromAlbums = []ActivityAlbum{{ID: t.AlbumID, Name: t.AlbumName}}
		}
	}
	// Actual persisted copies, not the provider selected for a future operation.
	kind := ""
	if t.Group != nil {
		kind = t.Group.Kind
	}
	table, backend := "media_replicas", "r.instance_id"
	if kind == "publish" || kind == "unpublish" || t.Type == "publish" || t.Type == "unpublish" {
		table, backend = "media_publications", "r.backend"
	}
	if t.Type == TaskCleanupTemp || t.Type == TaskTempMaterialize {
		table, backend = "temp_objects", "r.backend"
	}
	rows, err := db.Query(`SELECT DISTINCT i.name FROM `+table+` r JOIN storage_instances i ON i.id=`+backend+`
 WHERE r.file_id=? OR (?!=0 AND r.file_id IN(SELECT t.file_id FROM task_operation_jobs j JOIN tasks t ON t.id=j.task_id WHERE j.operation_id=?)) ORDER BY i.name`, t.FileID.String, operation, operation)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return c, err
		}
		found := false
		for _, old := range c.Storages {
			found = found || old == name
		}
		if !found {
			c.Storages = append(c.Storages, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	var payload struct {
		OwnerID string `json:"owner_id"`
		Name    string `json:"name"`
	}
	_ = json.Unmarshal([]byte(t.Payload), &payload)
	if t.Type == "account_space" || t.Type == "account_group_title" {
		name := payload.OwnerID
		if payload.OwnerID == AdminOwner {
			if user, _, e := db.GetAdmin(); e == nil {
				name = user
			}
		} else if payload.OwnerID != "" {
			_ = db.QueryRow(`SELECT username FROM accounts WHERE id=?`, payload.OwnerID).Scan(&name)
		}
		if name != "" {
			c.Description = "账号「" + name + "」"
			if payload.Name != "" {
				c.Description += " →「" + payload.Name + "」"
			}
		}
	}
	if t.Type == TaskConvert || t.Group != nil && t.Group.Kind == "convert" {
		var encrypted bool
		if e := db.QueryRow(`SELECT target_encrypted FROM file_conversions WHERE source_id=? OR target_id=? LIMIT 1`, t.FileID.String, t.FileID.String).Scan(&encrypted); e == nil {
			if encrypted {
				c.Description = "未加密 → 加密"
			} else {
				c.Description = "加密 → 未加密"
			}
		}
	}
	return c, err
}
