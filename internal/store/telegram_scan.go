package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/mediax"
	"pimg/internal/tguser"
)

type TelegramScanPayload struct {
	AlbumID      int64  `json:"album_id"`
	AccountID    int64  `json:"account_id"`
	ChatID       int64  `json:"chat_id"`
	TopicID      int    `json:"topic_id"`
	Mode         string `json:"mode"`
	Since        int64  `json:"since,omitempty"`
	Until        int64  `json:"until,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	TaskID       int64  `json:"task_id,omitempty"`
	MaxDate      int64  `json:"max_date,omitempty"`
	MaxMessageID int    `json:"max_message_id,omitempty"`
	Finished     bool   `json:"finished,omitempty"`
	Automatic    bool   `json:"automatic,omitempty"`
}

func (s *Service) QueueTelegramScan(ctx context.Context, albumID int64, mode string, since, until int64) (int64, error) {
	return s.queueTelegramScan(ctx, albumID, mode, since, until, false)
}

// QueueAutomaticTelegramScan reconciles a bound topic without preempting idle
// replication merely to discover that nothing has changed.
func (s *Service) QueueAutomaticTelegramScan(ctx context.Context, albumID int64) (int64, error) {
	return s.queueTelegramScan(ctx, albumID, "resume", 0, 0, true)
}

func (s *Service) queueTelegramScan(ctx context.Context, albumID int64, mode string, since, until int64, automatic bool) (int64, error) {
	mode = strings.TrimSpace(mode)
	resume := mode == "resume"
	if mode == "" || mode == "resume" {
		mode = "latest"
	}
	if mode != "latest" && mode != "date" && mode != "all" {
		return 0, errors.New("扫描方式应为 latest、date 或 all")
	}
	if since < 0 || until < 0 {
		return 0, errors.New("扫描日期无效")
	}
	if mode == "date" && since == 0 {
		return 0, errors.New("请选择扫描起始日期")
	}
	b, err := s.EnsureTelegramAlbum(ctx, albumID)
	if err != nil {
		return 0, err
	}
	if resume {
		if id, e := s.DB.ResumeTelegramScan(b.AlbumID, automatic); e == nil {
			return id, nil
		} else if !errors.Is(e, dbx.ErrNotFound) {
			return 0, e
		}
	}
	if mode == "latest" {
		since = b.ScanDate
		if since == 0 {
			since, err = s.DB.LatestTelegramSourceDate(b.AccountID, b.ChatID, b.TopicID)
			if err != nil {
				return 0, err
			}
		}
		if since > 0 {
			since--
		} // Inclusive overlap; source identity, not time, deduplicates.
	}
	if mode == "all" {
		since = 0
	}
	if until == 0 {
		until = time.Now().Unix()
	}
	if since > until {
		return 0, errors.New("扫描起始日期不能晚于结束日期")
	}
	p := TelegramScanPayload{AlbumID: b.AlbumID, AccountID: b.AccountID, ChatID: b.ChatID, TopicID: b.TopicID, Mode: mode, Since: since, Until: until, Automatic: automatic}
	id, err := s.DB.QueueLinkedAlbumScan(b.AlbumID, dbx.TaskPayloadJSON(p))
	if err != nil {
		return 0, err
	}
	// An existing task retains its original date bounds and durable page cursor.
	t, err := s.DB.GetTask(id)
	if err != nil {
		return 0, err
	}
	var existing TelegramScanPayload
	if json.Unmarshal([]byte(t.Payload), &existing) == nil && existing.TaskID == 0 {
		existing.TaskID = id
		if err = s.DB.TelegramScanProgress(b.AlbumID, id, existing.Offset, dbx.TaskPayloadJSON(existing)); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Service) RunTelegramScan(ctx context.Context, payload string) (retErr error) {
	if err := s.TGReady(); err != nil {
		return err
	}
	var p TelegramScanPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return fmt.Errorf("扫描任务参数无效：%w", err)
	}
	if p.AlbumID <= 0 {
		return errors.New("扫描任务缺少相册")
	}
	if p.Automatic {
		ctx = context.WithValue(ctx, automaticTelegramScanKey{}, true)
	}
	if p.TaskID > 0 {
		ctx = WithIntakeParent(ctx, p.TaskID)
	}
	unlock, err := s.lockFile(ctx, fmt.Sprintf("tg-operation:%d", p.AlbumID))
	if err != nil {
		return err
	}
	defer unlock()
	b, err := s.DB.GetTopicBinding(p.AlbumID)
	if err != nil {
		return err
	}
	if b.State != "active" {
		return dbx.ErrAlbumFrozen
	}
	if (p.AccountID != 0 && p.AccountID != b.AccountID) || (p.ChatID != 0 && p.ChatID != b.ChatID) || (p.TopicID != 0 && p.TopicID != b.TopicID) || b.AccountID != s.UserTG.Status().AccountID {
		return errors.New("扫描任务来源与当前绑定不一致")
	}
	p.AccountID, p.ChatID, p.TopicID = b.AccountID, b.ChatID, b.TopicID
	if p.TaskID == 0 {
		p.TaskID = b.ScanTaskID
	}
	defer func() {
		if retErr != nil {
			_ = s.DB.SetTopicError(b.AlbumID, retErr.Error())
		}
	}()
	_, err = s.scanTelegramTopic(ctx, *b, p, b.AlbumID)
	return err
}

type automaticTelegramScanKey struct{}

type telegramScanStats struct {
	MaxID       int
	Files       []string
	SourceParts []dbx.TelegramSourcePart
}

func (s *Service) scanTelegramTopic(ctx context.Context, b TopicBinding, p TelegramScanPayload, destination int64) (telegramScanStats, error) {
	stats := telegramScanStats{}
	var failures []string
	// Discovered failures survive moving the history cursor past newer pages.
	stored, err := s.DB.TelegramMessagesForTopic(b.AccountID, b.ChatID, b.TopicID)
	if err != nil {
		return stats, err
	}
	for _, m := range stored {
		if m.State != "pending" && m.State != "failed" {
			continue
		}
		var ref tguser.Reference
		if err = json.Unmarshal([]byte(m.Reference), &ref); err != nil {
			failures = append(failures, fmt.Sprintf("消息 %d 定位无效", m.MessageID))
			continue
		}
		msg := tguser.Message{Reference: ref, Date: m.Date, Name: m.Name, MIME: m.MIME, Caption: m.Caption, IsService: m.IsService}
		if _, _, err = s.receiveTelegramMessage(ctx, msg, destination); err != nil {
			failures = append(failures, fmt.Sprintf("消息 %d：%s", m.MessageID, err))
		}
	}
	for !p.Finished {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		var since, until time.Time
		if p.Since > 0 {
			since = time.Unix(p.Since, 0)
		}
		if p.Until > 0 {
			until = time.Unix(p.Until+1, 0)
		}
		page, e := s.UserTG.History(ctx, b.ChatID, b.TopicID, since, until, p.Offset, 100)
		if e != nil {
			return stats, e
		}
		for _, msg := range page.Messages {
			msg.Reference = normalizeTelegramReference(msg.Reference, b)
			if msg.Reference.AccountID != b.AccountID || msg.Reference.ChatID != b.ChatID || msg.Reference.TopicID != b.TopicID {
				return stats, errors.New("Telegram 历史返回了其他来源的消息，已停止扫描")
			}
			if msg.Date < p.Since || (p.Until > 0 && msg.Date > p.Until) {
				continue
			}
			if msg.Reference.MessageID <= 0 {
				continue
			}
			if msg.Date > p.MaxDate || (msg.Date == p.MaxDate && msg.Reference.MessageID > p.MaxMessageID) {
				p.MaxDate, p.MaxMessageID = msg.Date, msg.Reference.MessageID
			}
			fileID, unknown, e := s.receiveTelegramMessage(ctx, msg, destination)
			parts, partsErr := s.DB.TelegramSourceParts(fmt.Sprintf("%d:%d:%d", b.AccountID, b.ChatID, msg.Reference.MessageID))
			if partsErr != nil {
				return stats, partsErr
			}
			if len(parts) > 0 {
				stats.SourceParts = append(stats.SourceParts, parts...)
			} else if fileID != "" {
				stats.Files = append(stats.Files, fileID)
			}
			// Only archive-related messages affect the deletion sweep boundary.
			// Unrelated text, attachments and service events may be removed with
			// their topic and must not keep a deletion task pending.
			if !unknown && (fileID != "" || len(parts) > 0) && msg.Reference.MessageID > stats.MaxID {
				stats.MaxID = msg.Reference.MessageID
			}
			if e != nil {
				failures = append(failures, fmt.Sprintf("消息 %d：%s", msg.Reference.MessageID, e))
			}
		}
		previous := p.Offset
		p.Offset = page.NextOffset
		p.Finished = page.Done
		if !p.Finished && (p.Offset <= 0 || p.Offset == previous) {
			return stats, errors.New("Telegram 历史分页没有前进，扫描已暂停")
		}
		if p.TaskID > 0 {
			if e = s.DB.TelegramScanProgress(b.AlbumID, p.TaskID, p.Offset, dbx.TaskPayloadJSON(p)); e != nil {
				return stats, e
			}
		}
	}
	if p.TaskID > 0 {
		covered := p.MaxDate
		if p.Automatic && len(failures) == 0 && p.Until > covered && p.Until <= time.Now().Unix() {
			covered = p.Until
		}
		if err = s.DB.CompleteTelegramScan(b.AlbumID, covered, p.MaxMessageID); err != nil {
			return stats, err
		}
	}
	if len(failures) > 0 {
		if len(failures) > 8 {
			failures = append(failures[:8], "其余失败消息已保留，重试时继续处理")
		}
		return stats, errors.New(strings.Join(failures, "；"))
	}
	return stats, nil
}

func normalizeTelegramReference(ref tguser.Reference, b TopicBinding) tguser.Reference {
	if ref.AccountID == 0 {
		ref.AccountID = b.AccountID
	}
	if ref.ChatID == 0 {
		ref.ChatID = b.ChatID
	}
	if ref.TopicID == 0 {
		ref.TopicID = b.TopicID
	}
	return ref
}

// ReceiveTelegramMessage is also available to the update listener. History is
// still periodically reconciled so a reconnect cannot lose update-only items.
func (s *Service) ReceiveTelegramMessage(ctx context.Context, msg tguser.Message, albumID int64) (*dbx.File, error) {
	if err := s.TGReady(); err != nil {
		return nil, err
	}
	b, err := s.DB.GetTopicBinding(albumID)
	if err != nil {
		return nil, err
	}
	if b.State != "active" {
		return nil, dbx.ErrAlbumFrozen
	}
	msg.Reference = normalizeTelegramReference(msg.Reference, *b)
	if msg.Reference.AccountID != b.AccountID || msg.Reference.ChatID != b.ChatID || msg.Reference.TopicID != b.TopicID {
		return nil, errors.New("消息不属于绑定的话题")
	}
	id, _, err := s.receiveTelegramMessage(ctx, msg, albumID)
	if err != nil || id == "" {
		return nil, err
	}
	return s.resolveTelegramSourceFile(id)
}

// A source receipt may point at the first ID of a chain of conversions. Only
// completed cutovers can be followed; staging or a pending purge is not ready.
func (s *Service) resolveTelegramSourceFile(id string) (*dbx.File, error) {
	seen := map[string]bool{}
	for hops := 0; hops < 64; hops++ {
		if seen[id] {
			return nil, errors.New("转换回执形成循环，已保留来源")
		}
		seen[id] = true
		f, err := s.DB.GetFile(id)
		if err != nil {
			return nil, err
		}
		if f.PurgeState.String != "" {
			return nil, dbx.ErrDeleting
		}
		if !f.Hidden {
			if active, e := s.DB.ConversionActive(id); e != nil {
				return nil, e
			} else if active {
				return nil, dbx.ErrConverting
			}
			return f, nil
		}
		c, err := s.DB.GetConversion(id)
		if err != nil {
			return nil, fmt.Errorf("来源文件 %s 的转换回执不可用，已保留来源", id)
		}
		if c.Phase != "done" {
			return nil, dbx.ErrConverting
		}
		id = c.TargetID
	}
	return nil, errors.New("转换回执链过长，已保留来源")
}

func (s *Service) receiveTelegramMessage(ctx context.Context, msg tguser.Message, albumID int64) (string, bool, error) {
	r := msg.Reference
	if r.AccountID == 0 {
		r.AccountID = s.UserTG.Status().AccountID
	}
	if r.ChatID == 0 {
		r.ChatID = s.TGChatID()
	}
	msg.Reference = r
	unlock, err := s.lockFile(ctx, fmt.Sprintf("tg-message:%d:%d:%d", r.AccountID, r.ChatID, r.MessageID))
	if err != nil {
		return "", false, err
	}
	defer unlock()
	old, err := s.DB.GetTelegramMessage(r.AccountID, r.ChatID, r.MessageID)
	if err != nil && !errors.Is(err, dbx.ErrNotFound) {
		return "", false, err
	}
	if old != nil {
		switch old.State {
		case "retired":
			return "", false, nil
		case "managed":
			return old.FileID, false, nil
		}
		if old.State == "done" {
			var previous tguser.Reference
			if json.Unmarshal([]byte(old.Reference), &previous) == nil && previous.DocumentID != 0 && r.DocumentID != 0 && previous.DocumentID != r.DocumentID {
				return old.FileID, true, errors.New("已导入消息的媒体被替换，已保留来源，请核对后重试")
			}
			return old.FileID, false, nil
		}
	}
	refJSON, err := json.Marshal(r)
	if err != nil {
		return "", false, err
	}
	m := &dbx.TelegramMessage{AccountID: r.AccountID, ChatID: r.ChatID, TopicID: r.TopicID, MessageID: r.MessageID, Date: msg.Date, Reference: string(refJSON), Name: msg.Name, MIME: msg.MIME, Caption: msg.Caption, IsService: msg.IsService, State: "pending"}
	if err = s.DB.UpsertTelegramMessage(m); err != nil {
		return "", false, err
	}
	if msg.IsService {
		err = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "ignored", "", "")
		return "", false, err
	}
	if uploaderName.MatchString(msg.Name) {
		return s.receiveUploaderMessage(ctx, msg, albumID)
	}
	var caption struct {
		App string `json:"app"`
	}
	_ = json.Unmarshal([]byte(msg.Caption), &caption)
	if caption.App == "pimg" || caption.App == "pimg-part2" {
		err = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "ignored", "", "未登记的 Pimg 归档数据，已保留待核对")
		return "", true, err
	}
	if !isTelegramMedia(msg) {
		err = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "ignored", "", "非图片或视频内容，已保留")
		return "", true, err
	}
	source := fmt.Sprintf("%d:%d:%d", r.AccountID, r.ChatID, r.MessageID)
	if f, e := s.DB.FileByTelegramSource(source); e == nil {
		err = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "done", f.ID, "")
		return f.ID, false, err
	} else if !errors.Is(e, dbx.ErrNotFound) {
		return "", false, e
	}
	name := msg.Name
	if name == "" {
		name = fmt.Sprintf("telegram-%d%s", r.MessageID, telegramExtension(msg.MIME))
	} else if filepath.Ext(name) == "" {
		name += telegramExtension(msg.MIME)
	}
	if automatic, _ := ctx.Value(automaticTelegramScanKey{}).(bool); automatic {
		// Only unknown media reaches this point. Deduplicated archives, text,
		// service messages and ignored carriers keep the probe passive.
		finish := s.BeginForeground()
		defer finish()
	}
	var f *dbx.File
	if transport, ok := s.UserTG.(tguser.StreamTransport); ok {
		reader, e := transport.OpenStream(ctx, r)
		if e != nil {
			err = e
		} else if reader == nil {
			err = errors.New("Telegram 未返回文件流")
		} else {
			defer reader.Close()
			f, err = s.ReceiveTelegramStream(ctx, reader, name, msg.MIME, albumID, source)
		}
	} else {
		var data []byte
		data, err = s.UserTG.Download(ctx, r)
		if err == nil {
			f, err = s.ReceiveTelegramBytes(ctx, data, name, msg.MIME, albumID, source)
		}
	}
	if err != nil {
		_ = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "failed", "", err.Error())
		return "", false, err
	}
	if f == nil {
		return "", false, errors.New("Telegram 导入未返回图片记录")
	}
	if err = s.DB.SetTelegramMessageState(r.AccountID, r.ChatID, r.MessageID, "done", f.ID, ""); err != nil {
		return f.ID, false, err
	}
	return f.ID, false, nil
}

func isTelegramMedia(m tguser.Message) bool {
	info := mediax.Detect(strings.ToLower(filepath.Ext(m.Name)), strings.ToLower(m.MIME))
	if info.Kind == "image" || info.Kind == "video" {
		return true
	}
	// Existing accepted image extensions are retained alongside mediax's map.
	switch strings.ToLower(filepath.Ext(m.Name)) {
	case ".heic", ".heif", ".tif", ".tiff":
		return true
	}
	return false
}
func telegramExtension(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch mime {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/avif":
		return ".avif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/x-matroska", "video/matroska":
		return ".mkv"
	case "video/x-m4v":
		return ".m4v"
	case "video/x-msvideo":
		return ".avi"
	case "video/mpeg":
		return ".mpeg"
	case "video/ogg":
		return ".ogv"
	case "video/mp2t":
		return ".ts"
	case "video/3gpp":
		return ".3gp"
	default:
		if strings.HasPrefix(mime, "video/") {
			return ".mp4"
		}
		return ".jpg"
	}
}

var telegramSyncWakes sync.Map // *Service -> chan struct{}; removed on shutdown.
func (s *Service) WakeTelegramSync() {
	if c, ok := telegramSyncWakes.Load(s); ok {
		select {
		case c.(chan struct{}) <- struct{}{}:
		default:
		}
	}
}

// StartTelegramSync only wakes at startup, authorization/reconnection, a server
// gap notification, or an explicit binding change. There is no periodic scan.
func (s *Service) StartTelegramSync(ctx context.Context) {
	wake := make(chan struct{}, 1)
	if _, loaded := telegramSyncWakes.LoadOrStore(s, wake); loaded {
		return
	}
	defer telegramSyncWakes.Delete(s)
	goal := time.Now().Unix()
	scheduled := map[int64]bool{}
	backoff := 5 * time.Second
	for {
		err := s.reconcileTelegramTopics(ctx, goal, scheduled)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			goal = time.Now().Unix()
			scheduled = map[int64]bool{}
			backoff = 5 * time.Second
			continue
		}
		s.Log.Warn("TG 增量补漏等待重试", "err", err, "after", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
			goal = time.Now().Unix()
			scheduled = map[int64]bool{}
			backoff = 5 * time.Second
		case <-timer.C:
			if backoff < time.Minute {
				backoff *= 2
				if backoff > time.Minute {
					backoff = time.Minute
				}
			}
		}
	}
}

func (s *Service) reconcileTelegramTopics(ctx context.Context, goal int64, scheduled map[int64]bool) error {
	if s.TGReady() != nil {
		return nil
	} // OnReady wakes us after authorization.
	bindings, err := s.DB.ListTopicBindings()
	if err != nil {
		return err
	}
	byChat := map[int64]map[int]tguser.Topic{}
	var pending []error
	for _, b := range bindings {
		if scheduled[b.AlbumID] {
			continue
		}
		if b.AccountID != s.UserTG.Status().AccountID || (b.ChatID != s.TGChatID() && !s.DB.TelegramChatAllowed(b.AccountID, b.ChatID)) {
			continue
		}
		byID, ok := byChat[b.ChatID]
		if !ok {
			topics, e := s.UserTG.Topics(ctx, b.ChatID)
			if e != nil {
				pending = append(pending, e)
				continue
			}
			byID = map[int]tguser.Topic{}
			for _, topic := range topics {
				byID[topic.ID] = topic
			}
			byChat[b.ChatID] = byID
		}
		if b.State == "creating" {
			if _, err = s.EnsureTelegramAlbum(ctx, b.AlbumID); err != nil {
				pending = append(pending, err)
				continue
			}
			b, err = s.DB.GetTopicBinding(b.AlbumID)
			if err != nil {
				pending = append(pending, err)
				continue
			}
		} else if b.State == "active" {
			if t, ok := byID[b.TopicID]; !ok {
				_ = s.DB.MarkTopicMissing(b.AlbumID)
				continue
			} else if t.Name != b.Name && !strings.HasPrefix(t.Name, "Pimg-") {
				if err = s.DB.FollowTelegramTopicName(b.AlbumID, t.Name); err != nil {
					pending = append(pending, err)
					continue
				}
			}
		}
		if b.State != "active" || b.ScanDate == 0 && b.ScanTaskID == 0 {
			continue
		}
		id, err := s.QueueAutomaticTelegramScan(ctx, b.AlbumID)
		if err != nil {
			pending = append(pending, err)
			continue
		}
		task, err := s.DB.GetTask(id)
		if err != nil {
			pending = append(pending, err)
			continue
		}
		var p TelegramScanPayload
		if err = json.Unmarshal([]byte(task.Payload), &p); err != nil {
			pending = append(pending, err)
			continue
		}
		// An interrupted/manual scan retains its original range. Once it finishes,
		// retry this connection event to cover the remaining offline interval too.
		if p.Until < goal {
			pending = append(pending, errors.New("等待已有扫描完成后补齐重连区间"))
		} else {
			scheduled[b.AlbumID] = true
		}
	}
	return errors.Join(pending...)
}
