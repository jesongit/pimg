package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"pimg/internal/filenames"
	"strings"
	"time"
)

var ErrNotFound = errors.New("记录不存在")
var ErrDeleting = errors.New("文件正在删除或仍有副本待清理，请在任务页重试删除")
var ErrConverting = errors.New("文件正在转换或等待清理，请先完成或重试转换")
var ErrPermanentEncrypted = errors.New("加密归档只能生成临时外链；请先转为未加密归档再永久发布")

const fileCols = `id,name,orig_name,ext,mime,size,sha256,kind,COALESCE(width,0),COALESCE(height,0),COALESCE(duration,0),
key_wrapped,key_nonce,crypto_ver,album_id,archive_state,archive_error,archive_attempts,archived_at,
has_thumb,COALESCE(thumb_w,0),COALESCE(thumb_h,0),thumb_mime,COALESCE(thumb_size,0),has_bthumb,published,published_at,published_path,publish_error,
show_home,purge_state,storage,cold_backend,hot_backend,hidden,created_at,updated_at,COALESCE(telegram_source,''),naming_json,owner_id,media_version,home_visibility,` + mediaTimeSQL

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	f := &File{}
	var albumID sql.NullInt64
	var namingJSON string
	err := row.Scan(&f.ID, &f.Name, &f.OrigName, &f.Ext, &f.Mime, &f.Size, &f.SHA256, &f.Kind,
		&f.Width, &f.Height, &f.Duration, &f.KeyWrapped, &f.KeyNonce, &f.CryptoVer, &albumID,
		&f.ArchiveState, &f.ArchiveError, &f.ArchiveAttempts, &f.ArchivedAt,
		&f.HasThumb, &f.ThumbW, &f.ThumbH, &f.ThumbMime, &f.ThumbSize, &f.HasBThumb,
		&f.Published, &f.PublishedAt, &f.PublishedPath, &f.PublishError,
		&f.ShowHome, &f.PurgeState, &f.Storage, &f.ColdBackend, &f.HotBackend, &f.Hidden, &f.CreatedAt, &f.UpdatedAt, &f.TelegramSource, &namingJSON, &f.OwnerID, &f.MediaVersion, &f.HomeVisibility, &f.MediaAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.AlbumID = albumID
	if err := json.Unmarshal([]byte(namingJSON), &f.Naming); err != nil {
		return nil, fmt.Errorf("文件命名元数据损坏: %w", err)
	}
	return f, nil
}

// GetFile 按 ID 取文件。
func (db *DB) GetFile(id string) (*File, error) {
	return scanFile(db.ReadQueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, id))
}

func (db *DB) GetVisibleFile(id string) (*File, error) {
	f, err := db.GetFile(id)
	if err != nil {
		return nil, err
	}
	if f.Hidden {
		return nil, ErrNotFound
	}
	return f, nil
}

func (db *DB) MutationFile(id string) (*File, error) {
	f, err := db.GetVisibleFile(id)
	if err != nil {
		return nil, err
	}
	active, err := db.ConversionActive(id)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, ErrConverting
	}
	return f, nil
}

// FileFilter 列表筛选。
type FileFilter struct {
	OwnerID    string
	Query      string
	Kind       string
	State      string // archive|published|show_home|failed
	Encryption string // File setting: encrypted|plaintext; independent of replica policies.
	AlbumID    int64
	HomeID     int64 // 固定游客相册 ID（state=show_home 时使用）
	Page       int
	PerPage    int
	Sort       string
	Seed       string // Stable full-scope random permutation.
	Before     int64  // Exclusive upper media-time bound for time navigation.
	After      int64  // Inclusive lower media-time bound for time navigation.
	BeforeSet  bool   // Explicit zero is a valid Unix-epoch boundary.
	AfterSet   bool
}

func fileFilterSQL(f FileFilter) (string, []any) {
	where := []string{"hidden=0"}
	args := []any{}
	if f.OwnerID != "" {
		where = append(where, "owner_id=?")
		args = append(args, f.OwnerID)
	}
	if f.Query != "" {
		where = append(where, "(name LIKE ? OR orig_name LIKE ? OR id LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q, q)
	}
	if f.Kind != "" {
		where = append(where, "kind=?")
		args = append(args, f.Kind)
	}
	switch f.Encryption {
	case "encrypted":
		where = append(where, "storage!='direct'")
	case "plaintext":
		where = append(where, "storage='direct'")
	}
	switch f.State {
	case "published":
		where = append(where, "published=1")
	case "unpublished":
		where = append(where, "published=0 AND purge_state IS NULL")
	case "show_home":
		where = append(where, exhibitedFileWhere)
	case "failed":
		where = append(where, "(archive_state='failed' OR purge_state='failed' OR (publish_error IS NOT NULL AND publish_error!=''))")
	case "archived":
		where = append(where, "archive_state='done'")
	}
	if f.AlbumID > 0 {
		where = append(where, "(album_id=? OR (album_id IS NULL AND EXISTS(SELECT 1 FROM albums a WHERE a.id=? AND a.is_default=1 AND a.owner_id=files.owner_id)))")
		args = append(args, f.AlbumID, f.AlbumID)
	} else if f.AlbumID == -1 {
		// Keep old ungrouped links usable, including records now in the default album.
		where = append(where, "(album_id IS NULL OR album_id IN (SELECT id FROM albums WHERE is_default=1))")
	}
	if f.BeforeSet || f.Before != 0 {
		where = append(where, "("+mediaTimeSQL+")<?")
		args = append(args, f.Before)
	}
	if f.AfterSet || f.After != 0 {
		where = append(where, "("+mediaTimeSQL+")>=?")
		args = append(args, f.After)
	}
	return strings.Join(where, " AND "), args
}

// ListFiles 分页列出文件（管理员视角）。
func (db *DB) ListFiles(f FileFilter) ([]*File, int, error) {
	if f.Sort == "random" {
		return db.listRandomFiles(f)
	}
	wsql, args := fileFilterSQL(f)
	var total int
	if err := db.ReadQueryRow(`SELECT COUNT(*) FROM files WHERE `+wsql, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.PerPage <= 0 {
		f.PerPage = 40
	}
	if f.PerPage > 200 {
		f.PerPage = 200
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	order := fileOrderSQL(f.Sort)
	q := `SELECT ` + fileCols + ` FROM files WHERE ` + wsql + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, f.PerPage, (f.Page-1)*f.PerPage)
	rows, err := db.ReadQuery(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		fl, err := scanFile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, fl)
	}
	return out, total, rows.Err()
}

// FileMonth describes all matching records, independently of the loaded pages.
type FileMonth struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

func (db *DB) FileTimeline(f FileFilter, offsetMinutes int) ([]FileMonth, error) {
	return db.FileTimelineGroups(f, offsetMinutes, "month")
}

// FileTimelineGroups returns date buckets for the complete matching scope.
// Keys and half-open bounds use the same fixed browser offset for every date.
func (db *DB) FileTimelineGroups(f FileFilter, offsetMinutes int, group string) ([]FileMonth, error) {
	if offsetMinutes < -840 || offsetMinutes > 840 {
		return nil, errors.New("时区偏移无效")
	}
	var expression, layout string
	var dayStep, monthStep, yearStep int
	switch group {
	case "year":
		expression, layout, yearStep = "strftime('%Y',("+mediaTimeSQL+"),'unixepoch',?)", "2006", 1
	case "", "month":
		expression, layout, monthStep = "strftime('%Y-%m',("+mediaTimeSQL+"),'unixepoch',?)", "2006-01", 1
	case "day":
		expression, layout, dayStep = "strftime('%Y-%m-%d',("+mediaTimeSQL+"),'unixepoch',?)", "2006-01-02", 1
	case "week":
		// Moving back six days before seeking Monday keeps Monday itself in
		// the current week, including weeks crossing month/year boundaries.
		expression, layout, dayStep = "date(("+mediaTimeSQL+"),'unixepoch',?,'-6 days','weekday 1')", "2006-01-02", 7
	default:
		return nil, errors.New("日期分区无效")
	}
	// The same scope as ListFiles, but date seeking must not shrink the index.
	f.Before, f.After = 0, 0
	f.BeforeSet, f.AfterSet = false, false
	where, args := fileFilterSQL(f)
	args = append([]any{fmt.Sprintf("%+d minutes", offsetMinutes)}, args...)
	rows, err := db.ReadQuery(`SELECT `+expression+`,COUNT(*) FROM files WHERE `+where+` GROUP BY 1 ORDER BY 1 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	months := make([]FileMonth, 0)
	zone := time.FixedZone("browser", offsetMinutes*60)
	for rows.Next() {
		var month FileMonth
		if err := rows.Scan(&month.Key, &month.Count); err != nil {
			return nil, err
		}
		start, err := time.ParseInLocation(layout, month.Key, zone)
		if err != nil {
			return nil, err
		}
		month.Start, month.End = start.Unix(), start.AddDate(yearStep, monthStep, dayStep).Unix()
		months = append(months, month)
	}
	return months, rows.Err()
}

// HomeItem 首页公开条目。
type HomeItem struct {
	File
	ThumbURL string
}

// ListHome 首页瀑布流：游客相册中已发布的文件。
func (db *DB) ListHome(albumID int64, page, perPage int) ([]*File, int, error) {
	if perPage <= 0 || perPage > 100 {
		perPage = 24
	}
	if page <= 0 {
		page = 1
	}
	var total int
	if err := db.ReadQueryRow(`SELECT COUNT(*) FROM files WHERE hidden=0 AND (storage='direct' OR media_version=2) AND published=1 AND album_id=? AND purge_state IS NULL`, albumID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.ReadQuery(`SELECT `+fileCols+` FROM files WHERE hidden=0 AND (storage='direct' OR media_version=2) AND published=1 AND album_id=? AND purge_state IS NULL
		ORDER BY published_at DESC,id DESC LIMIT ? OFFSET ?`, albumID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// InsertFile 新建文件记录。
func (db *DB) InsertFile(f *File) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = insertFile(tx, f); err != nil {
		return err
	}
	if err = appendIngestEvent(tx, localIntakeEvent(f, "accepted", f.ID, f.AlbumID.Int64)); err != nil {
		return err
	}
	if f.TelegramSource != "" {
		if _, err = tx.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES(?,0,?)`, f.TelegramSource, f.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type fileInserter interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func insertFile(q fileInserter, f *File) error {
	if f == nil {
		return errors.New("上传记录为空")
	}
	if err := assignFileOwner(q, f); err != nil {
		return err
	}
	f.CreatedAt = now()
	f.UpdatedAt = f.CreatedAt
	f.MediaAt = f.CreatedAt
	if f.MediaTimeSource() == "manual" {
		f.MediaAt = *f.Naming.ManualTime
	} else if f.MediaTimeSource() != "ingested" {
		f.MediaAt = f.Naming.OriginalTime
	}
	storage := f.Storage
	if storage == "" {
		storage = "tg"
	}
	f.Storage = storage
	if f.Naming.Date != "" {
		oldStem := filenames.Stem(f.Kind, storage != "direct", f.Naming)
		automatic := f.Naming.Sequence == 0 || f.Name == "" || f.Name == oldStem
		if !f.Naming.ValidDate() || f.Naming.Sequence < 0 || f.Naming.Sequence > 9999 {
			return errors.New("文件命名日期或序号无效")
		}
		// Counter changes share the insertion transaction: concurrent uploads,
		// imports and whole video batches cannot reserve the same sequence.
		requested := f.Naming.Sequence
		if requested > 0 {
			var last int
			err := q.QueryRow(`SELECT last_sequence FROM file_name_counters WHERE owner_id=? AND date=?`, f.OwnerID, f.Naming.Date).Scan(&last)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && last >= requested {
				requested = 0
			}
		}
		if requested == 0 {
			err := q.QueryRow(`INSERT INTO file_name_counters(owner_id,date,last_sequence) VALUES(?,?,1)
ON CONFLICT(owner_id,date) DO UPDATE SET last_sequence=last_sequence+1 WHERE last_sequence<9999 RETURNING last_sequence`, f.OwnerID, f.Naming.Date).Scan(&f.Naming.Sequence)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("该日期的四位文件序号已用尽（最多 9999 个）")
			}
			if err != nil {
				return err
			}
		} else {
			if _, err := q.Exec(`INSERT INTO file_name_counters(owner_id,date,last_sequence) VALUES(?,?,?) ON CONFLICT(owner_id,date) DO UPDATE SET last_sequence=MAX(last_sequence,excluded.last_sequence)`, f.OwnerID, f.Naming.Date, requested); err != nil {
				return err
			}
		}
		if automatic {
			f.Name = filenames.Stem(f.Kind, storage != "direct", f.Naming)
		}
	}
	namingJSON, err := json.Marshal(f.Naming)
	if err != nil {
		return err
	}
	if f.ColdBackend == "" {
		f.ColdBackend = "telegram"
	}
	result, err := q.Exec(`INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,width,height,duration,
		key_wrapped,key_nonce,crypto_ver,album_id,archive_state,storage,cold_backend,hot_backend,created_at,updated_at,telegram_source,naming_json,owner_id)
		SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULLIF(?,''),?,?
		WHERE NOT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`,
		f.ID, f.Name, f.OrigName, f.Ext, f.Mime, f.Size, f.SHA256, f.Kind, f.Width, f.Height, f.Duration,
		f.KeyWrapped, f.KeyNonce, f.CryptoVer, f.AlbumID, f.ArchiveState, storage, f.ColdBackend, f.HotBackend, f.CreatedAt, f.UpdatedAt, f.TelegramSource, string(namingJSON), f.OwnerID, f.AlbumID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrAlbumFrozen
	}
	if _, err = q.Exec(`INSERT INTO upload_policies(file_id,auto_publish) VALUES(?,?)`, f.ID, storage == "direct" && !f.NoAutoPublish && !f.Hidden); err != nil {
		return err
	}
	return nil
}

// InsertUploadBatch commits every independent part and its archive task as one
// unit. A source receipt can therefore never expose only the first half of a batch.
func (db *DB) InsertUploadBatch(files []*File) error {
	if len(files) == 0 {
		return errors.New("上传批次不能为空")
	}
	if files[0] == nil {
		return errors.New("上传记录为空")
	}
	source := files[0].TelegramSource
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockIntake(tx); err != nil {
		return err
	}
	for _, f := range files {
		if err = assignFileOwner(tx, f); err != nil {
			return err
		}
	}
	if len(files) == 1 && files[0].OperationID > 0 && files[0].OperationItemKey != "" {
		f := files[0]
		var input, owner string
		var expires int64
		if err = tx.QueryRow(`SELECT input_state,owner_id,expires_at FROM task_operations WHERE id=?`, f.OperationID).Scan(&input, &owner, &expires); err != nil {
			return err
		}
		if input != "open" || expires <= now() {
			return ErrTaskOperationClosed
		}
		if owner != AdminOwner && owner != f.OwnerID {
			return ErrNotFound
		}
		var previous string
		e := tx.QueryRow(`SELECT file_id FROM task_operation_items WHERE operation_id=? AND item_key=? AND file_id!=''`, f.OperationID, f.OperationItemKey).Scan(&previous)
		if e == nil {
			prior, e := scanFile(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, previous))
			if e != nil {
				return ErrTaskOperationConflict
			}
			if prior.SHA256 != f.SHA256 || prior.Size != f.Size || (prior.Storage == "direct") != (f.Storage == "direct") || prior.PurgeState.String != "" {
				return ErrTaskOperationConflict
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			prior.UploadDuplicate = true
			files[0] = prior
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	if len(files) == 1 && files[0].Kind == "image" {
		candidate, e := imageCandidate(tx, files[0])
		if e != nil {
			return e
		}
		if candidate != nil {
			if source != "" {
				if _, err = tx.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES(?,0,?)`, source, candidate.ID); err != nil {
					return err
				}
			}
			if err = appendIngestEvent(tx, localIntakeEvent(files[0], "duplicate", candidate.ID, candidate.AlbumID.Int64)); err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			candidate.UploadDuplicate = true
			files[0] = candidate
			return nil
		}
	}
	if err = registerInputGroup(tx, files); err != nil {
		return err
	}
	for index, f := range files {
		if f == nil {
			return errors.New("上传记录为空")
		}
		if index > 0 && f.TelegramSource != "" {
			return errors.New("批次来源只应记录在第一个文件上")
		}
		if err = insertFile(tx, f); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE files SET media_version=? WHERE id=?`, f.MediaVersion, f.ID); err != nil {
			return err
		}
		for _, policy := range f.MediaPolicies {
			if _, err = tx.Exec(`INSERT INTO media_replicas(file_id,instance_id,policy) VALUES(?,?,?)`, f.ID, policy.InstanceID, JSONValue(map[string]any{"encrypted": policy.Encrypted, "split": policy.Split, "part_mb": policy.PartMB})); err != nil {
				return err
			}
		}
		if err = appendIngestEvent(tx, localIntakeEvent(f, "accepted", f.ID, f.AlbumID.Int64)); err != nil {
			return err
		}
		typ := TaskArchive
		if f.Storage == "direct" {
			typ = TaskDirectArchive
		}
		res, e := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,'{}','pending',?,?,?)`, typ, f.ID, now(), now(), now())
		if e != nil {
			return e
		}
		id, e := res.LastInsertId()
		if e != nil {
			return e
		}
		if f.OperationID != 0 {
			err = AttachTaskOperationItem(tx, id, typ, f.ID, "{}", f.OperationID, f.OperationItemKey)
		} else if f.ParentTaskID != 0 {
			err = attachTaskFollowup(tx, id, f.ParentTaskID, "telegram_receive")
		} else {
			err = attachTaskOperation(tx, id, typ, f.ID, "{}")
		}
		if err != nil {
			return err
		}
		if source != "" {
			if _, err = tx.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES(?,?,?)`, source, index, f.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// touch 更新 updated_at。
func (db *DB) touch(id string) {
	db.Exec(`UPDATE files SET updated_at=? WHERE id=?`, now(), id)
}

// UpdateFileMeta 更新可编辑元数据。
func (db *DB) UpdateFileMeta(id, name string, albumID sql.NullInt64, showHome *bool) error {
	sets := []string{"updated_at=?"}
	args := []any{now()}
	if name != "" {
		sets = append(sets, "name=?")
		args = append(args, name)
	}
	if albumID.Valid {
		sets = append(sets, "album_id=?")
		if albumID.Int64 > 0 {
			args = append(args, albumID)
		} else {
			args = append(args, nil) // 移出相册 = NULL（0 会违反外键）
		}
	}
	if showHome != nil {
		sets = append(sets, "show_home=?")
		args = append(args, *showHome)
	}
	args = append(args, id)
	albumGuard := ""
	if albumID.Valid && albumID.Int64 > 0 {
		albumGuard = ` AND NOT EXISTS(SELECT 1 FROM album_topics WHERE album_id=? AND state IN ('deleting','deleted'))`
		args = append(args, albumID.Int64)
	}
	result, err := db.Exec(`UPDATE files SET `+strings.Join(sets, ",")+` WHERE id=? AND hidden=0
AND NOT EXISTS(SELECT 1 FROM file_conversions c WHERE c.source_id=files.id AND c.phase!='done')`+albumGuard, args...)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		if albumID.Valid {
			if err := db.AlbumWritable(albumID.Int64); err != nil {
				return err
			}
		}
		if _, err := db.MutationFile(id); err != nil {
			return err
		}
		return ErrConverting
	}
	return nil
}

// SetArchiveState 更新归档状态。
func (db *DB) SetArchiveState(id, state string, errMsg string) error {
	var archivedAt any
	if state == "done" {
		archivedAt = now()
	}
	_, err := db.Exec(`UPDATE files SET archive_state=?, archive_error=?, archived_at=COALESCE(?, archived_at),
		archive_attempts=archive_attempts+1, updated_at=? WHERE id=?`,
		state, errMsg, archivedAt, now(), id)
	return err
}

// SetPublished 标记发布结果。
func (db *DB) SetPublished(id string, published bool, path, errMsg string) error {
	if published {
		res, err := db.Exec(`UPDATE files SET published=1, published_at=?, published_path=?, publish_error=NULL, updated_at=? WHERE id=? AND purge_state IS NULL`,
			now(), path, now(), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.GetFile(id); err != nil {
				return err
			}
			return ErrDeleting
		}
		return nil
	}
	_, err := db.Exec(`UPDATE files SET published=0, published_at=NULL, published_path=NULL, publish_error=?, updated_at=? WHERE id=?`,
		errMsg, now(), id)
	return err
}

// SetPublishError records an incomplete revoke without pretending the objects
// have already been removed.
func (db *DB) SetPublishError(id, message string) error {
	_, err := db.Exec(`UPDATE files SET publish_error=?, updated_at=? WHERE id=?`, message, now(), id)
	return err
}

// SetShowHome 单独控制首页展示。
func (db *DB) SetShowHome(id string, v bool) error {
	_, err := db.Exec(`UPDATE files SET show_home=?, updated_at=? WHERE id=?`, v, now(), id)
	return err
}

// SetThumbInfo 归档时写入缩略图信息。
func (db *DB) SetThumbInfo(id string, hasThumb bool, w, h int, mime string, size int64, hasB bool) error {
	var m any
	if mime != "" {
		m = mime
	}
	_, err := db.Exec(`UPDATE files SET has_thumb=?, thumb_w=?, thumb_h=?, thumb_mime=?, thumb_size=?, has_bthumb=?, updated_at=? WHERE id=?`,
		hasThumb, w, h, m, size, hasB, now(), id)
	return err
}

// SetPurgeState 更新彻底删除流程状态。
func (db *DB) SetPurgeState(id string, state string) error {
	var v any
	if state != "" {
		v = state
	}
	_, err := db.Exec(`UPDATE files SET purge_state=?, updated_at=? WHERE id=?`, v, now(), id)
	return err
}

// DeleteFile 删除记录（连带 chunks/temp_objects 由外键级联）。
func (db *DB) DeleteFile(id string) error {
	return db.DeleteFileAfterTask(id, 0)
}

func (db *DB) DeleteFileAfterTask(id string, taskID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var referenced bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE file_id=? AND id!=? AND state IN ('pending','running','failed'))`, id, taskID).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return ErrTaskReferenced
	}
	// A crash can leave the source inbox row pending even though InsertFile
	// committed. Retire by source identity as well as acknowledged file_id.
	if _, err = tx.Exec(`UPDATE tg_messages SET state='retired',updated_at=? WHERE file_id=? OR EXISTS(
SELECT 1 FROM files f WHERE f.id=? AND f.telegram_source=CAST(tg_messages.account_id AS TEXT)||':'||CAST(tg_messages.chat_id AS TEXT)||':'||CAST(tg_messages.message_id AS TEXT)) OR EXISTS(
SELECT 1 FROM telegram_source_parts p WHERE p.file_id=? AND p.source_key=CAST(tg_messages.account_id AS TEXT)||':'||CAST(tg_messages.chat_id AS TEXT)||':'||CAST(tg_messages.message_id AS TEXT))`, now(), id, id, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE telegram_source_parts SET purged_at=? WHERE file_id=?`, now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM files WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Stats 汇总统计。
type Stats struct {
	Total, Archived, Failed, Published, ShowHome int
	TotalSize                                    int64
	TempLive, TasksFailed, TasksPending          int
}

// GetStats 汇总。
func (db *DB) GetStats() (*Stats, error) {
	s := &Stats{}
	err := db.ReadQueryRow(`SELECT COUNT(*), COALESCE(SUM(archive_state='done'),0), COALESCE(SUM(archive_state='failed'),0),
		COALESCE(SUM(published),0), 0, COALESCE(SUM(size),0) FROM files WHERE hidden=0`).
		Scan(&s.Total, &s.Archived, &s.Failed, &s.Published, &s.ShowHome, &s.TotalSize)
	if err != nil {
		return nil, err
	}
	db.ReadQueryRow(`SELECT COUNT(*) FROM files WHERE ` + exhibitedFileWhere).Scan(&s.ShowHome)
	db.ReadQueryRow(`SELECT COUNT(*) FROM temp_objects WHERE deleted_at IS NULL AND expires_at>?`, now()).Scan(&s.TempLive)
	db.ReadQueryRow(`SELECT COUNT(*) FROM tasks WHERE state='failed'`).Scan(&s.TasksFailed)
	db.ReadQueryRow(`SELECT COUNT(*) FROM tasks WHERE state='pending'`).Scan(&s.TasksPending)
	return s, nil
}

// ---- chunks ----

// UpsertChunk 记录已上传分片。
func (db *DB) UpsertChunk(c *Chunk) error {
	_, err := db.Exec(`INSERT INTO file_chunks(file_id,kind,idx,message_id,tg_file_id,object_key,size,sha256,uploaded_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(file_id,kind,idx) DO UPDATE SET message_id=excluded.message_id,
		tg_file_id=excluded.tg_file_id, object_key=excluded.object_key, size=excluded.size, sha256=excluded.sha256, uploaded_at=excluded.uploaded_at`,
		c.FileID, c.Kind, c.Idx, c.MessageID, c.TgFileID, c.ObjectKey, c.Size, c.SHA256, now())
	return err
}

// ListChunks 某文件某类全部分片（按序）。
func (db *DB) ListChunks(fileID, kind string) ([]*Chunk, error) {
	rows, err := db.ReadQuery(`SELECT id,file_id,kind,idx,message_id,tg_file_id,object_key,size,sha256,uploaded_at
		FROM file_chunks WHERE file_id=? AND kind=? ORDER BY idx`, fileID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chunk
	for rows.Next() {
		c := &Chunk{}
		if err := rows.Scan(&c.ID, &c.FileID, &c.Kind, &c.Idx, &c.MessageID, &c.TgFileID, &c.ObjectKey, &c.Size, &c.SHA256, &c.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteFileChunks 删除某文件全部分片记录。
func (db *DB) DeleteFileChunks(fileID string) error {
	_, err := db.Exec(`DELETE FROM file_chunks WHERE file_id=?`, fileID)
	return err
}

func (db *DB) DeleteChunk(id int64) error {
	_, err := db.Exec(`DELETE FROM file_chunks WHERE id=?`, id)
	return err
}

// ---- temp objects ----

// InsertTempObject 记录临时对象。
func (db *DB) InsertTempObject(t *TempObject) error {
	t.CreatedAt = now()
	if t.Backend == "" {
		t.Backend = "r2"
	}
	_, err := db.Exec(`INSERT INTO temp_objects(id,file_id,kind,object_key,backend,size,created_at,expires_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, t.ID, t.FileID, t.Kind, t.ObjectKey, t.Backend, t.Size, t.CreatedAt, t.ExpiresAt)
	return err
}

// LiveTempObject 查找可复用的未过期临时对象。
func (db *DB) LiveTempObject(fileID, kind string, nowSec int64) (*TempObject, error) {
	row := db.ReadQueryRow(`SELECT id,file_id,kind,object_key,backend,size,created_at,expires_at,deleted_at,ready_at
		FROM temp_objects WHERE file_id=? AND kind=? AND deleted_at IS NULL AND expires_at>? 
		ORDER BY created_at DESC LIMIT 1`, fileID, kind, nowSec)
	t := &TempObject{}
	err := row.Scan(&t.ID, &t.FileID, &t.Kind, &t.ObjectKey, &t.Backend, &t.Size, &t.CreatedAt, &t.ExpiresAt, &t.DeletedAt, &t.ReadyAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return t, nil
}

// LiveTempObjects returns recorded temporary copies for a page of files in one
// query. Expired and deleted copies are excluded even before cleanup runs.
func (db *DB) LiveTempObjects(fileIDs []string, nowSec int64) (map[string][]*TempObject, error) {
	out := make(map[string][]*TempObject)
	if len(fileIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(fileIDs)+1)
	args = append(args, nowSec)
	for _, id := range fileIDs {
		args = append(args, id)
	}
	rows, err := db.ReadQuery(`SELECT id,file_id,kind,backend,expires_at,ready_at,object_key
		FROM temp_objects WHERE deleted_at IS NULL AND expires_at>?
		AND file_id IN (`+strings.TrimRight(strings.Repeat("?,", len(fileIDs)), ",")+`)
		ORDER BY file_id,kind,expires_at DESC,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t := &TempObject{}
		if err := rows.Scan(&t.ID, &t.FileID, &t.Kind, &t.Backend, &t.ExpiresAt, &t.ReadyAt, &t.ObjectKey); err != nil {
			return nil, err
		}
		out[t.FileID] = append(out[t.FileID], t)
	}
	return out, rows.Err()
}

// ReadyTempObjects returns every confirmed live copy, including copies retained
// on a previously selected provider. It never chooses the current hot provider.
func (db *DB) ReadyTempObjects(fileID string, nowSec int64) ([]*TempObject, error) {
	rows, err := db.ReadQuery(`SELECT id,file_id,kind,object_key,backend,size,created_at,expires_at,deleted_at,ready_at
		FROM temp_objects WHERE file_id=? AND deleted_at IS NULL AND expires_at>? AND ready_at IS NOT NULL
		ORDER BY kind,backend,expires_at DESC,id`, fileID, nowSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TempObject
	for rows.Next() {
		obj := &TempObject{}
		if err := rows.Scan(&obj.ID, &obj.FileID, &obj.Kind, &obj.ObjectKey, &obj.Backend, &obj.Size, &obj.CreatedAt, &obj.ExpiresAt, &obj.DeletedAt, &obj.ReadyAt); err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

// ExpiredTempObjects 到期待清理对象。
func (db *DB) ExpiredTempObjects(nowSec int64, limit int) ([]*TempObject, error) {
	rows, err := db.ReadQuery(`SELECT id,file_id,kind,object_key,backend,size,created_at,expires_at,deleted_at,ready_at
		FROM temp_objects WHERE deleted_at IS NULL AND expires_at<=? ORDER BY cleanup_attempted_at,expires_at,id LIMIT ?`, nowSec, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TempObject
	for rows.Next() {
		t := &TempObject{}
		if err := rows.Scan(&t.ID, &t.FileID, &t.Kind, &t.ObjectKey, &t.Backend, &t.Size, &t.CreatedAt, &t.ExpiresAt, &t.DeletedAt, &t.ReadyAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FileTempObjects returns every remaining object of this file, including live
// objects, without truncating against objects belonging to other files.
func (db *DB) FileTempObjects(fileID string) ([]*TempObject, error) {
	rows, err := db.ReadQuery(`SELECT id,file_id,kind,object_key,backend,expires_at FROM temp_objects WHERE file_id=? AND deleted_at IS NULL`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TempObject
	for rows.Next() {
		o := &TempObject{}
		if err := rows.Scan(&o.ID, &o.FileID, &o.Kind, &o.ObjectKey, &o.Backend, &o.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkTempReady confirms a cleanup record only after upload or verification.
func (db *DB) MarkTempReady(id string) error {
	_, err := db.Exec(`UPDATE temp_objects SET ready_at=? WHERE id=? AND deleted_at IS NULL AND ready_at IS NULL`, now(), id)
	return err
}

// ClearTempReady withdraws confirmation when normal preview verification fails.
// Keep the cleanup record because an unavailable object may still need deletion.
func (db *DB) ClearTempReady(id string) error {
	_, err := db.Exec(`UPDATE temp_objects SET ready_at=NULL WHERE id=? AND ready_at IS NOT NULL`, id)
	return err
}

// MarkTempDeleted 标记已删除。
func (db *DB) MarkTempDeleted(id string) error {
	_, err := db.Exec(`UPDATE temp_objects SET deleted_at=? WHERE id=?`, now(), id)
	return err
}

// TouchTempObject 更新最后访问（可选）。
func (db *DB) TouchTempObject(id string) { db.touch(id) }

func fmtID(id string) string { return fmt.Sprintf("file[%s]", id) }
