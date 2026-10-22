package dbx

import "errors"

// Display is an album-level grant for thumbnails only. Publication and original
// storage references are deliberately independent of this metadata migration.
const exhibitionSchema = `
ALTER TABLE albums ADD COLUMN displayed INTEGER NOT NULL DEFAULT 0 CHECK(displayed IN (0,1));
INSERT INTO albums(name,created_at,is_default,owner_id)
 SELECT '未分类',unixepoch(),1,owner FROM (
 SELECT owner_id owner FROM albums WHERE is_home=1
 UNION SELECT owner_id FROM files WHERE album_id IN (SELECT id FROM albums WHERE is_home=1)) legacy
 WHERE NOT EXISTS(SELECT 1 FROM albums a WHERE a.owner_id=legacy.owner AND a.is_default=1);
UPDATE files SET album_id=(SELECT id FROM albums a WHERE a.owner_id=files.owner_id AND a.is_default=1),updated_at=unixepoch()
 WHERE album_id IN (SELECT id FROM albums WHERE is_home=1);
UPDATE import_rules SET album_id=(SELECT id FROM albums a WHERE a.owner_id=(SELECT owner_id FROM albums WHERE id=import_rules.album_id) AND a.is_default=1)
 WHERE album_id IN (SELECT id FROM albums WHERE is_home=1);
UPDATE local_import_batches SET album_id=(SELECT id FROM albums a WHERE a.owner_id=(SELECT owner_id FROM albums WHERE id=local_import_batches.album_id) AND a.is_default=1)
 WHERE album_id IN (SELECT id FROM albums WHERE is_home=1);
UPDATE tg_moves SET album_id=COALESCE((SELECT a.id FROM albums a JOIN files f ON f.owner_id=a.owner_id WHERE f.id=tg_moves.file_id AND a.is_default=1),
 (SELECT a.id FROM albums a WHERE a.owner_id=(SELECT owner_id FROM albums WHERE id=tg_moves.album_id) AND a.is_default=1))
 WHERE album_id IN (SELECT id FROM albums WHERE is_home=1);
UPDATE tasks SET payload=json_set(payload,'$.album_id',(SELECT a.id FROM albums a WHERE a.owner_id=(SELECT owner_id FROM albums WHERE id=json_extract(tasks.payload,'$.album_id')) AND a.is_default=1))
 WHERE type IN ('local_import_scan','local_import','local_import_idle','tg_scan','tg_receive','tg_move_file')
 AND state IN ('pending','running') AND json_valid(payload) AND json_extract(payload,'$.album_id') IN (SELECT id FROM albums WHERE is_home=1);
DELETE FROM album_topics WHERE album_id IN (SELECT id FROM albums WHERE is_home=1);
DELETE FROM albums WHERE is_home=1;
CREATE INDEX albums_displayed ON albums(id,owner_id) WHERE displayed=1;
`

// Legacy show_home is intentionally not imported: old automatic publication
// flags never authorize new public thumbnails.
const fileExhibitionSchema = `
ALTER TABLE files ADD COLUMN home_visibility TEXT NOT NULL DEFAULT 'inherit' CHECK(home_visibility IN ('inherit','public','private'));
CREATE TRIGGER gallery_file_visibility_update AFTER UPDATE OF home_visibility ON files
 WHEN OLD.home_visibility IS NOT NEW.home_visibility BEGIN
 INSERT INTO gallery_revisions(owner_id) VALUES(NEW.owner_id) ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
END;
CREATE TRIGGER gallery_album_visibility_update AFTER UPDATE OF displayed ON albums
 WHEN OLD.displayed IS NOT NEW.displayed BEGIN
 INSERT INTO gallery_revisions(owner_id) VALUES(NEW.owner_id) ON CONFLICT(owner_id) DO UPDATE SET revision=revision+1;
END;
`

// An explicit private choice overrides the album. Every public read checks the
// current grant and account state, including after preview IO.
const exhibitedFileWhere = `hidden=0 AND purge_state IS NULL AND archive_state='done'
 AND (home_visibility='public' OR (home_visibility='inherit' AND EXISTS(SELECT 1 FROM albums a WHERE a.owner_id=files.owner_id AND a.displayed=1
 AND (a.id=files.album_id OR (files.album_id IS NULL AND a.is_default=1)))))
 AND (owner_id='admin:1' OR EXISTS(SELECT 1 FROM accounts u WHERE u.id=files.owner_id AND u.disabled=0 AND u.deleting=0))`

// Commit the visibility policy and optional batch receipt atomically. Album,
// original bytes, publication and storage references remain untouched.
func (db *DB) SetFileHomeVisibility(id string, visible bool, operationID int64) error {
	kind, policy := "hide_home", "private"
	if visible {
		kind, policy = "show_home", "public"
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE task_operations SET updated_at=updated_at WHERE 0`); err != nil {
		return err
	}
	var f *File
	if operationID != 0 {
		f, err = operationMutationFile(tx, id, operationID, kind, "")
	} else {
		f, err = scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=? AND hidden=0`, id))
		if err == nil && f.PurgeState.String != "" {
			return ErrDeleting
		}
		var converting bool
		if err == nil {
			err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_conversions WHERE phase!='done' AND (source_id=? OR target_id=?))`, id, id).Scan(&converting)
		}
		if converting {
			return ErrConverting
		}
	}
	if err != nil {
		return err
	}
	if visible && (f.ArchiveState != "done" || (f.Kind != "image" && f.Kind != "video")) {
		return errors.New("仅可公开归档完成的图片或视频缩略图")
	}
	if operationID != 0 {
		if err = recordTaskOperationItem(tx, operationID, id, id, f.OrigName, "done", ""); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE files SET home_visibility=?,updated_at=? WHERE id=?`, policy, now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) SetAlbumDisplayed(id int64, displayed bool) error {
	res, err := db.Exec(`UPDATE albums SET displayed=? WHERE id=?`, displayed, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) FileExhibited(id string) bool {
	var ok bool
	return db.ReadQueryRow(`SELECT EXISTS(SELECT 1 FROM files WHERE id=? AND `+exhibitedFileWhere+`)`, id).Scan(&ok) == nil && ok
}

func (db *DB) ListExhibitedFiles(page, perPage int) ([]*File, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 24
	}
	var total int
	if err := db.ReadQueryRow(`SELECT COUNT(*) FROM files WHERE ` + exhibitedFileWhere).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.ReadQuery(`SELECT `+fileCols+` FROM files WHERE `+exhibitedFileWhere+` ORDER BY `+mediaTimeSQL+` DESC,id DESC LIMIT ? OFFSET ?`, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, f)
	}
	return out, total, rows.Err()
}
