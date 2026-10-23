package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"pimg/internal/dbx"
	"strconv"
	"strings"
	"time"
)

func (s *Service) SaveImportRule(r dbx.ImportRule) (int64, error) {
	if strings.TrimSpace(r.Name) == "" || r.AlbumID <= 0 {
		return 0, errors.New("请填写规则名称和目标相册")
	}
	if r.IntervalMinutes < 0 || r.IntervalMinutes > 43200 || (r.IntervalMinutes > 0 && r.IntervalMinutes < 5) {
		return 0, errors.New("自动扫描间隔应为 5–43200 分钟，0 表示仅手动扫描")
	}
	i, e := s.instance(r.InstanceID)
	if e != nil {
		return 0, e
	}
	if i.source == nil || !i.row.Enabled {
		return 0, errors.New("该实例不支持导入或未启用")
	}
	var album bool
	if e = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM albums WHERE id=?)`, r.AlbumID).Scan(&album); e != nil {
		return 0, e
	}
	if !album {
		return 0, errors.New("目标相册不存在")
	}
	r.Source = strings.Trim(strings.TrimSpace(r.Source), "/")
	if i.row.Driver == "telegram" {
		n, e := strconv.Atoi(r.Source)
		if e != nil || n <= 0 {
			return 0, errors.New("TG 来源需要填写正数话题 ID")
		}
		for _, v := range s.StorageInstances() {
			if v.Driver == "telegram" && v.Config.TopicID == n {
				return 0, errors.New("导入源不能是 Pimg 归档输出话题")
			}
		}
	} else {
		for _, part := range strings.Split(r.Source, "/") {
			if part == ".." || strings.Contains(part, "\\") {
				return 0, errors.New("源目录或前缀必须位于实例根目录内")
			}
		}
		for _, owned := range []string{"archives", "display", "albums", "permanent", "temp", "tmp", "pimg", "thumbs"} {
			if pathOverlap(r.Source, owned) && r.Source != "" {
				return 0, errors.New("导入源不能包含 Pimg 输出目录")
			}
		}
	}
	if r.ID > 0 {
		old, e := s.DB.ImportRule(r.ID)
		if e != nil {
			return 0, e
		}
		if old.InstanceID != r.InstanceID || old.Source != r.Source {
			return 0, errors.New("修改来源请创建新规则，旧规则的整理记录需保留")
		}
	}
	return s.DB.SaveImportRule(r)
}

// A page is acknowledged only after every source has a durable receipt. Intake
// uses a stable source key in the files transaction to close the crash window
// between a successful upload and the corresponding receipt update.
func (s *Service) RunSourceScan(ctx context.Context, ruleID int64) (retErr error) {
	r, e := s.DB.ImportRule(ruleID)
	if e != nil {
		return e
	}
	if !r.Enabled {
		return nil
	}
	album, e := s.DB.GetAlbum(r.AlbumID)
	if e != nil {
		return e
	}
	if !s.DB.AccountActive(album.OwnerID) {
		return errors.New("来源导入所属账号已停用")
	}
	ctx = dbx.WithOwner(ctx, album.OwnerID)
	i, e := s.instance(r.InstanceID)
	if e != nil {
		return e
	}
	if i.source == nil {
		return errors.New("存储来源不可用")
	}
	defer func() {
		msg := ""
		if retErr != nil {
			msg = retErr.Error()
		}
		_, _ = s.DB.Exec(`UPDATE import_rules SET last_error=? WHERE id=?`, msg, r.ID)
	}()
	// Retry unfinished receipts first; pagination must not strand a failed item.
	rows, e := s.DB.Query(`SELECT entry FROM import_receipts WHERE rule_id=? AND state='pending' ORDER BY locator LIMIT 50`, r.ID)
	if e != nil {
		return e
	}
	pending := []SourceEntry{}
	var errs []error
	for rows.Next() {
		var raw string
		var v SourceEntry
		if e = rows.Scan(&raw); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			rows.Close()
			return e
		}
		pending = append(pending, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range pending {
		if e = s.ingestSource(ctx, r, i.source, v); e != nil {
			errs = append(errs, e)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	page, e := i.source.List(ctx, r.Source, r.Cursor, r.Recursive)
	if e != nil {
		return e
	}
	for _, v := range page.Entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		state := "pending"
		if !sourceMedia(v) {
			state = "ignored"
		}
		if _, e = s.DB.Exec(`INSERT INTO import_receipts(rule_id,locator,version,entry,state) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, r.ID, v.ID, v.Version, dbx.JSONValue(v), state); e != nil {
			return e
		}
		if state == "pending" {
			if e = s.ingestSource(ctx, r, i.source, v); e != nil {
				errs = append(errs, e)
			}
		}
	}
	next := time.Now().Add(time.Duration(r.IntervalMinutes) * time.Minute).Unix()
	if page.Cursor != "" {
		next = 0
	}
	_, e = s.DB.Exec(`UPDATE import_rules SET cursor=?,next_scan=? WHERE id=?`, page.Cursor, next, r.ID)
	if e != nil {
		return e
	}
	// Each task processes a bounded page, then yields to the foreground queue.
	if page.Cursor != "" {
		_, e = s.DB.EnqueueTask(dbx.TaskSourceScan, "", dbx.TaskPayloadJSON(map[string]any{"rule_id": r.ID}))
	}
	return errors.Join(append(errs, e)...)
}
func (s *Service) ingestSource(ctx context.Context, r dbx.ImportRule, source SourceDriver, v SourceEntry) (retErr error) {
	var enabled bool
	if e := s.DB.QueryRow(`SELECT enabled FROM import_rules WHERE id=?`, r.ID).Scan(&enabled); e != nil {
		return e
	}
	if !enabled {
		return nil
	}
	var state, file string
	if e := s.DB.QueryRow(`SELECT state,file_id FROM import_receipts WHERE rule_id=? AND locator=? AND version=?`, r.ID, v.ID, v.Version).Scan(&state, &file); e != nil {
		return e
	}
	if state != "pending" {
		return nil
	}
	defer func() {
		msg := ""
		if retErr != nil {
			msg = retErr.Error()
		}
		_, _ = s.DB.Exec(`UPDATE import_receipts SET error=? WHERE rule_id=? AND locator=? AND version=? AND state='pending'`, msg, r.ID, v.ID, v.Version)
	}()
	key := fmt.Sprintf("source:%s:%s:%s", r.InstanceID, v.ID, v.Version)
	existing, e := s.DB.FileByTelegramSource(key)
	if errors.Is(e, dbx.ErrNotFound) {
		rc, err := source.Open(ctx, v)
		if err != nil {
			if errors.Is(err, ErrSourceChanged) {
				_, e = s.DB.Exec(`UPDATE import_receipts SET state='ignored',error=? WHERE rule_id=? AND locator=? AND version=?`, err.Error(), r.ID, v.ID, v.Version)
				return e
			}
			return err
		}
		defer rc.Close()
		ctx = context.WithValue(ctx, telegramSourceKey{}, key)
		// Source scans already run in the idle lane. Do not register foreground
		// activity for this nested intake or it would cancel its own task.
		ctx = context.WithValue(ctx, idleIntakeContext{}, true)
		ctx = WithPrivateUpload(ctx)
		if album, err := s.DB.GetAlbum(r.AlbumID); err == nil {
			ctx = dbx.WithOwner(ctx, album.OwnerID)
		}
		existing, e = s.ReceiveUpload(ctx, s.syncCopyReader(ctx, rc, r.InstanceID), v.Name, strings.TrimPrefix(strings.ToLower(pathExtension(v.Name)), "."), sql.NullInt64{Int64: r.AlbumID, Valid: true}, nil, 0, !r.Encrypted)
	}
	if e != nil {
		return e
	}
	_, e = s.DB.Exec(`UPDATE import_receipts SET file_id=?,state='archiving',error='' WHERE rule_id=? AND locator=? AND version=?`, existing.ID, r.ID, v.ID, v.Version)
	if e != nil {
		return e
	}
	if existing.ArchiveState == "done" {
		_, e = s.DB.EnqueueUniqueTask(dbx.TaskSourceCleanup, existing.ID, "{}")
	}
	return e
}
func pathExtension(name string) string {
	index := strings.LastIndexByte(name, '.')
	if index < 0 {
		return ""
	}
	return name[index:]
}

type idleIntakeContext struct{}

func (s *Service) RunSourceCleanup(ctx context.Context, id string) error {
	unlock, e := s.lockFile(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	f, e := s.mutableFile(id)
	if e != nil {
		return e
	}
	if f.ArchiveState != "done" {
		return errors.New("归档未完成，保留源文件")
	}
	var verified int
	query := `SELECT COUNT(*) FROM archive_objects WHERE file_id=? AND verified=1 AND kind='data'`
	if f.MediaVersion == 2 {
		query = `SELECT COUNT(*) FROM media_replicas WHERE file_id=? AND state='ready'`
	}
	if e = s.DB.QueryRow(query, id).Scan(&verified); e != nil {
		return e
	}
	if verified == 0 {
		return errors.New("还没有经过校验的托管归档，保留源文件")
	}
	rows, e := s.DB.Query(`SELECT rule_id,locator,version,entry FROM import_receipts WHERE file_id=? AND state='archiving'`, id)
	if e != nil {
		return e
	}
	type receipt struct {
		rule                    int64
		locator, version, entry string
	}
	receipts := []receipt{}
	for rows.Next() {
		var v receipt
		if e = rows.Scan(&v.rule, &v.locator, &v.version, &v.entry); e != nil {
			rows.Close()
			return e
		}
		receipts = append(receipts, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	var errs []error
	archiveVerified := false
	for _, v := range receipts {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		rule, e := s.DB.ImportRule(v.rule)
		if e != nil {
			return e
		}
		if !rule.Enabled {
			continue
		}
		i, e := s.instance(rule.InstanceID)
		if e != nil {
			return e
		}
		if !i.row.Enabled {
			continue
		}
		if !archiveVerified {
			if e = s.verifyIdleArchive(ctx, f); e != nil {
				return fmt.Errorf("归档不可恢复，保留源文件: %w", e)
			}
			archiveVerified = true
		}
		var entry SourceEntry
		if e = json.Unmarshal([]byte(v.entry), &entry); e != nil {
			return e
		}
		entry.SHA256 = f.SHA256
		if manual, ok := i.source.(ManualSourceCleanup); ok {
			if _, e = s.DB.Exec(`UPDATE import_receipts SET state='cleanup_pending',error=? WHERE rule_id=? AND locator=? AND version=?`, manual.ManualCleanupReason(), v.rule, v.locator, v.version); e != nil {
				return e
			}
			continue
		}
		if e = i.source.Delete(ctx, entry); e != nil {
			_, _ = s.DB.Exec(`UPDATE import_receipts SET error=? WHERE rule_id=? AND locator=? AND version=?`, e.Error(), v.rule, v.locator, v.version)
			errs = append(errs, e)
			continue
		}
		if _, e = s.DB.Exec(`UPDATE import_receipts SET state='cleaned',error='' WHERE rule_id=? AND locator=? AND version=?`, v.rule, v.locator, v.version); e != nil {
			return e
		}
	}
	return errors.Join(errs...)
}

func (s *Service) verifyIdleArchive(ctx context.Context, f *dbx.File) error {
	_, err := s.verifyArchivedOriginal(ctx, f, true)
	return err
}
