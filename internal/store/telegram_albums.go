package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/tguser"
)

type TopicBinding = dbx.TopicBinding

// Deleted albums have no remaining name to synchronize. Their durable tasks
// should finish explicitly instead of retrying a target that cannot return.
var ErrTelegramAlbumSyncObsolete = errors.New("相册已删除或正在删除，无需同步话题名称")

func (s *Service) AlbumWritable(albumID int64) error { return s.DB.AlbumWritable(albumID) }

func (s *Service) BindTelegramTopic(ctx context.Context, albumID int64, topicID int) error {
	if err := s.TGReady(); err != nil {
		return err
	}
	if topicID <= 1 {
		return errors.New("请选择独立话题，不能绑定 General")
	}
	unlock, err := s.lockFile(ctx, dbx.TelegramAlbumTaskKey(albumID))
	if err != nil {
		return err
	}
	defer unlock()
	if old, e := s.DB.GetTopicBinding(albumID); e == nil {
		if old.TopicID == topicID && old.AccountID == s.UserTG.Status().AccountID && old.ChatID == s.AdminTGChatID() && old.State == "active" {
			return nil
		}
		return errors.New("该相册已绑定话题，不能覆盖原归档位置")
	} else if !errors.Is(e, dbx.ErrNotFound) {
		return e
	}
	topics, err := s.UserTG.Topics(ctx, s.AdminTGChatID())
	if err != nil {
		return err
	}
	for _, topic := range topics {
		if topic.ID == topicID {
			return s.DB.BindExistingTelegramTopic(&dbx.TopicBinding{AlbumID: albumID, AccountID: s.UserTG.Status().AccountID, ChatID: s.AdminTGChatID(), TopicID: topicID, Name: topic.Name, State: "active"})
		}
	}
	return errors.New("所选话题不在已配置的超级群中")
}

// EnsureTelegramAlbum never adopts a topic by its display name. Creation has a
// unique temporary remote name so a crash between CreateTopic and the DB write
// can resume without creating a second archive location.
func (s *Service) EnsureTelegramAlbum(ctx context.Context, albumID int64) (TopicBinding, error) {
	if err := s.TGReady(); err != nil {
		return TopicBinding{}, err
	}
	if albumID <= 0 {
		var err error
		albumID, err = s.DB.EnsureDefaultAlbum()
		if err != nil {
			return TopicBinding{}, err
		}
	}
	unlock, err := s.lockFile(ctx, dbx.TelegramAlbumTaskKey(albumID))
	if err != nil {
		return TopicBinding{}, err
	}
	defer unlock()
	return s.ensureTelegramAlbum(ctx, albumID)
}

func (s *Service) ensureTelegramAlbum(ctx context.Context, albumID int64) (TopicBinding, error) {
	a, err := s.DB.GetAlbum(albumID)
	if err != nil {
		return TopicBinding{}, err
	}
	chat, err := s.albumChat(ctx, albumID)
	if err != nil {
		return TopicBinding{}, err
	}
	account := s.UserTG.Status().AccountID
	if account == 0 || chat == 0 {
		return TopicBinding{}, errors.New("请先授权 Telegram 用户账号并选择超级群")
	}
	b, err := s.DB.GetTopicBinding(albumID)
	if errors.Is(err, dbx.ErrNotFound) {
		b = &dbx.TopicBinding{AlbumID: albumID, AccountID: account, ChatID: chat, Name: a.Name, CreateName: "Pimg-" + ids.New(), IsDefault: a.IsDefault, State: "creating"}
		if err = s.DB.InsertTopicBinding(b); err != nil {
			return TopicBinding{}, err
		}
		b, err = s.DB.GetTopicBinding(albumID)
	}
	if err != nil {
		return TopicBinding{}, err
	}
	if b.AccountID != account || b.ChatID != chat {
		return *b, errors.New("相册绑定了其他 Telegram 账号或群，请恢复原账号和群后重试")
	}
	if b.State == "deleting" || b.State == "deleted" {
		return *b, dbx.ErrAlbumFrozen
	}
	topics, err := s.UserTG.Topics(ctx, chat)
	if err != nil {
		return *b, err
	}
	if b.TopicID > 1 {
		found := false
		for _, t := range topics {
			if t.ID == b.TopicID {
				found = true
				if t.Name == b.CreateName && t.Name != b.Name {
					if e := s.UserTG.RenameTopic(ctx, chat, t.ID, b.Name); e != nil {
						_ = s.DB.SetTopicError(albumID, "话题名称同步待重试："+e.Error())
					} else {
						_ = s.DB.SetTopicError(albumID, "")
					}
				}
				break
			}
		}
		if !found {
			err = errors.New("绑定的 Telegram 话题已不存在；图片索引已保留，请修复绑定后重试")
			_ = s.DB.SetTopicError(albumID, err.Error())
			return *b, err
		}
		return *b, nil
	}
	var topic tguser.Topic
	for _, t := range topics {
		if t.Name == b.CreateName {
			topic = t
			break
		}
	}
	if topic.ID == 0 {
		topic, err = s.UserTG.CreateTopic(ctx, chat, b.CreateName)
		if err != nil {
			_ = s.DB.SetTopicError(albumID, err.Error())
			return *b, err
		}
	}
	if err = s.DB.ActivateTopicBinding(albumID, topic.ID); err != nil {
		return *b, err
	}
	b.TopicID, b.State = topic.ID, "active"
	s.WakeTelegramSync()
	if err = s.UserTG.RenameTopic(ctx, chat, topic.ID, b.Name); err != nil {
		_ = s.DB.SetTopicError(albumID, "话题已创建，名称同步待重试："+err.Error())
		if _, e := s.DB.FindActiveTask(dbx.TaskTelegramRenameAlbum, dbx.TelegramAlbumTaskKey(albumID)); errors.Is(e, dbx.ErrNotFound) {
			_, _ = s.DB.EnqueueTask(dbx.TaskTelegramRenameAlbum, dbx.TelegramAlbumTaskKey(albumID), dbx.TaskPayloadJSON(map[string]any{"album_id": albumID, "name": b.Name}))
		}
	}
	return *b, nil
}

func (s *Service) CreateLinkedAlbum(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("相册名称不能为空")
	}
	if utf8.RuneCountInString(name) > 128 {
		return 0, errors.New("Telegram 相册名称最多 128 个字符")
	}
	if err := s.TGReady(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	space, err := s.EnsureTelegramSpace(ctx, dbx.ContextOwner(ctx))
	if err != nil {
		return 0, err
	}
	id, taskID, err := s.DB.CreateLinkedTelegramAlbum(name, s.UserTG.Status().AccountID, space.ChatID, "Pimg-"+ids.New())
	if err != nil {
		return 0, err
	}
	if err = s.RunRenameLinkedAlbum(ctx, id, name); err != nil {
		_ = s.DB.SetTopicError(id, err.Error())
		// Local creation succeeded. The UI can show its durable task and retry
		// that same album instead of posting a second creation request.
		return id, nil
	}
	_ = s.DB.FinishTask(taskID, dbx.TaskPayloadJSON(map[string]any{"album_id": id}))
	return id, nil
}

func (s *Service) CreateLinkedAlbumFromTopic(ctx context.Context, name string, topicID int) (int64, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 128 {
		return 0, errors.New("Telegram 相册名称最多 128 个字符")
	}
	if err := s.TGReady(); err != nil {
		return 0, err
	}
	if topicID <= 1 {
		return 0, errors.New("请选择独立话题，不能绑定 General")
	}
	topics, err := s.UserTG.Topics(ctx, s.AdminTGChatID())
	if err != nil {
		return 0, err
	}
	for _, topic := range topics {
		if topic.ID == topicID {
			if err = ctx.Err(); err != nil {
				return 0, err
			}
			return s.DB.CreateAlbumFromTelegramTopic(&dbx.TopicBinding{AccountID: s.UserTG.Status().AccountID, ChatID: s.AdminTGChatID(), TopicID: topic.ID, Name: topic.Name, State: "active"})
		}
	}
	return 0, errors.New("所选话题不在已配置的超级群中")
}

// Rename persists the desired name before scheduling remote work. The task can
// replay RenameTopic after a reconnect or process restart.
func (s *Service) RenameLinkedAlbum(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("相册名称不能为空")
	}
	if utf8.RuneCountInString(name) > 128 {
		return errors.New("Telegram 相册名称最多 128 个字符")
	}
	if _, err := s.EnsureTelegramAlbum(ctx, id); err != nil {
		return err
	}
	unlock, err := s.lockFile(ctx, dbx.TelegramAlbumTaskKey(id))
	if err != nil {
		return err
	}
	defer unlock()
	return s.DB.RenameLinkedAlbum(id, name)
}

func (s *Service) RunRenameLinkedAlbum(ctx context.Context, id int64, name string) error {
	unlock, err := s.lockFile(ctx, dbx.TelegramAlbumTaskKey(id))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.DB.GetAlbum(id); errors.Is(err, dbx.ErrNotFound) {
		return ErrTelegramAlbumSyncObsolete
	} else if err != nil {
		return err
	}
	b, err := s.DB.GetTopicBinding(id)
	if err != nil && !errors.Is(err, dbx.ErrNotFound) {
		return err
	}
	if b != nil && (b.State == "deleting" || b.State == "deleted") {
		return ErrTelegramAlbumSyncObsolete
	}
	if err := s.TGReady(); err != nil {
		return err
	}
	// Ordinary-user album creation queues this task before any topic binding
	// exists. Use the internal initializer under the already-held album lock.
	if errors.Is(err, dbx.ErrNotFound) || b.State == "creating" {
		if _, err = s.ensureTelegramAlbum(ctx, id); err != nil {
			_ = s.DB.SetTopicError(id, err.Error())
			return err
		}
		b, err = s.DB.GetTopicBinding(id)
		if err != nil {
			return err
		}
	}
	if b.State != "active" {
		return dbx.ErrAlbumFrozen
	}
	if b.AccountID != s.UserTG.Status().AccountID {
		return errors.New("Telegram 授权账号与话题绑定不一致")
	}
	// A retry of an earlier rename must not overwrite a newer desired name.
	name = b.Name
	if err = s.UserTG.RenameTopic(ctx, b.ChatID, b.TopicID, name); err != nil {
		_ = s.DB.SetTopicError(id, err.Error())
		return err
	}
	return s.DB.SetTopicError(id, "")
}

func (s *Service) QueueDeleteLinkedAlbum(ctx context.Context, id int64) (int64, error) {
	b, err := s.DB.GetTopicBinding(id)
	if err == nil && b.State == "deleting" {
		return s.DB.QueueLinkedAlbumDelete(id, dbx.TaskPayloadJSON(map[string]any{"album_id": id}))
	}
	if _, err = s.EnsureTelegramAlbum(ctx, id); err != nil {
		return 0, err
	}
	return s.DB.QueueLinkedAlbumDelete(id, dbx.TaskPayloadJSON(map[string]any{"album_id": id}))
}

func (s *Service) RunDeleteLinkedAlbum(ctx context.Context, id int64) (retErr error) {
	if err := s.TGReady(); err != nil {
		return err
	}
	// This is a task lock, distinct from EnsureTelegramAlbum's binding lock.
	// Archive/move holds file -> binding; taking binding -> file here would deadlock.
	unlock, err := s.lockFile(ctx, fmt.Sprintf("tg-operation:%d", id))
	if err != nil {
		return err
	}
	defer unlock()
	b, err := s.DB.GetTopicBinding(id)
	if err != nil {
		return err
	}
	if b.State == "deleted" {
		return nil
	}
	if b.IsDefault {
		return errors.New("未分类是固定话题，不能删除")
	}
	if b.State != "deleting" {
		return errors.New("尚未提交相册删除任务")
	}
	defer func() {
		if retErr != nil {
			_ = s.DB.SetTopicError(id, retErr.Error())
		}
	}()
	if b.AccountID != s.UserTG.Status().AccountID {
		return errors.New("Telegram 授权账号与话题绑定不一致")
	}
	// Drain accepted archive/conversion writes before closing the remote topic.
	// New intake/conversion is already frozen atomically in SQLite.
	accepted, err := s.DB.LinkedAlbumFileIDs(id)
	if err != nil {
		return err
	}
	album, err := s.DB.GetAlbum(id)
	if err != nil {
		return err
	}
	defaultID, err := s.DB.EnsureOwnerDefaultAlbum(album.OwnerID)
	if err != nil {
		return err
	}
	for _, fileID := range accepted {
		f, e := s.DB.GetFile(fileID)
		if e != nil {
			return e
		}
		active, e := s.DB.ConversionActive(fileID)
		if e != nil {
			return e
		}
		moving, e := s.DB.TelegramMoveActive(fileID)
		if e != nil {
			return e
		}
		// This deletion task owns and resumes its own move into Uncategorized.
		if move, e := s.DB.GetTelegramMove(fileID); e == nil && move.AlbumID == defaultID {
			moving = false
		}
		if f.ArchiveState != "done" || active || moving {
			return fmt.Errorf("文件 %s 正在归档、转换或迁移，等待完成后自动重试删除；TG 话题暂不关闭", fileID)
		}
	}
	destination, err := s.EnsureTelegramAlbum(ctx, defaultID)
	if err != nil {
		return err
	}
	topics, err := s.UserTG.Topics(ctx, b.ChatID)
	if err != nil {
		return err
	}
	present := false
	for _, topic := range topics {
		if topic.ID == b.TopicID {
			present = true
			break
		}
	}
	if !present {
		if b.DeletePhase == "verified" || b.DeletePhase == "remote_deleted" {
			return s.DB.FinishLinkedAlbumDelete(id, destination.AlbumID)
		}
		return errors.New("源 Telegram 话题已不存在，无法核对未扫描内容；已保留相册与图片索引")
	}
	if err = s.UserTG.CloseTopic(ctx, b.ChatID, b.TopicID, true); err != nil {
		return err
	}
	// A moved file already belongs to its destination, while its old message
	// may still await deletion. Drain those checkpoints even when the source
	// scan correctly skips the retired message.
	rows, err := s.DB.Query(`SELECT file_id,album_id,refs FROM tg_moves WHERE phase='cleanup'`)
	if err != nil {
		return err
	}
	type pendingMove struct {
		id    string
		album int64
	}
	var pending []pendingMove
	for rows.Next() {
		var id, refs string
		var album int64
		if err = rows.Scan(&id, &album, &refs); err != nil {
			rows.Close()
			return err
		}
		var parts []telegramMovingPart
		if err = json.Unmarshal([]byte(refs), &parts); err != nil {
			rows.Close()
			return err
		}
		for _, p := range parts {
			if p.Old.TopicID == b.TopicID {
				pending = append(pending, pendingMove{id, album})
				break
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range pending {
		if err = s.MoveTelegramFile(ctx, m.id, m.album); err != nil {
			return err
		}
	}
	// Deletion intentionally ignores the user's normal discovery date range.
	first, err := s.scanTelegramTopic(ctx, *b, TelegramScanPayload{AlbumID: id, AccountID: b.AccountID, ChatID: b.ChatID, TopicID: b.TopicID, Mode: "all"}, destination.AlbumID)
	if err != nil {
		return err
	}
	files, err := s.DB.LinkedAlbumFileIDs(id)
	if err != nil {
		return err
	}
	for _, fileID := range files {
		if err = s.MoveTelegramFile(ctx, fileID, destination.AlbumID); err != nil {
			return fmt.Errorf("迁移图片 %s：%w", fileID, err)
		}
	}
	// A second full sweep catches messages written by the topic creator/admins
	// while a closed topic was being drained. New managed media postpones
	// deletion; unrelated content is deleted together with the topic.
	last, err := s.scanTelegramTopic(ctx, *b, TelegramScanPayload{AlbumID: id, AccountID: b.AccountID, ChatID: b.ChatID, TopicID: b.TopicID, Mode: "all"}, destination.AlbumID)
	if err != nil {
		return err
	}
	if last.MaxID > first.MaxID {
		return errors.New("迁移期间话题出现新图片或视频，已保留话题；请等待归档完成后重试")
	}
	// A source inbox message is safe to remove only after its imported archive
	// has actually completed, not merely after ReceiveUpload returned an ID.
	seen := map[string]bool{}
	toVerify := append([]string(nil), last.Files...)
	for _, part := range last.SourceParts {
		if part.PurgedAt == 0 {
			toVerify = append(toVerify, part.FileID)
		}
	}
	for _, fileID := range toVerify {
		if fileID == "" {
			continue
		}
		f, e := s.resolveTelegramSourceFile(fileID)
		if e != nil {
			return fmt.Errorf("来源文件 %s 尚未核对完成，已保留话题：%w", fileID, e)
		}
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		if f.ArchiveState != "done" {
			return fmt.Errorf("文件 %s 尚未归档完成，请等待归档任务后重试", f.ID)
		}
		// A retained inbox message or an orphaned old copy may refer to a file
		// the user already moved elsewhere. Keep that authoritative album; only
		// files still belonging to the deleted album move to Uncategorized.
		if f.AlbumID.Int64 == id {
			if e = s.MoveTelegramFile(ctx, f.ID, destination.AlbumID); e != nil {
				return e
			}
		} else if move, e := s.DB.GetTelegramMove(f.ID); e == nil && move.Phase == "cleanup" && move.AlbumID == f.AlbumID.Int64 {
			if e = s.MoveTelegramFile(ctx, f.ID, move.AlbumID); e != nil {
				return e
			}
		}
		if _, e = s.Verify(ctx, f.ID); e != nil {
			return fmt.Errorf("迁移后校验图片 %s：%w", f.ID, e)
		}
	}
	if err = s.DB.SetTopicDeletePhase(id, "verified"); err != nil {
		return err
	}
	if err = s.UserTG.DeleteTopic(ctx, b.ChatID, b.TopicID); err != nil {
		return err
	}
	if err = s.DB.SetTopicDeletePhase(id, "remote_deleted"); err != nil {
		return err
	}
	return s.DB.FinishLinkedAlbumDelete(id, destination.AlbumID)
}

func (s *Service) recordManagedTelegram(ref tguser.Reference, fileID, name, mime string) error {
	if ref.AccountID == 0 {
		ref.AccountID = s.UserTG.Status().AccountID
	}
	if ref.ChatID == 0 {
		ref.ChatID = s.TGChatID()
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return s.DB.UpsertTelegramMessage(&dbx.TelegramMessage{AccountID: ref.AccountID, ChatID: ref.ChatID, TopicID: ref.TopicID, MessageID: ref.MessageID, Date: time.Now().Unix(), Reference: string(b), Name: name, MIME: mime, State: "managed", FileID: fileID})
}
func (s *Service) MarkRetiredTelegram(ref tguser.Reference) error {
	if ref.AccountID == 0 {
		ref.AccountID = s.UserTG.Status().AccountID
	}
	if ref.ChatID == 0 {
		ref.ChatID = s.TGChatID()
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return s.DB.UpsertTelegramMessage(&dbx.TelegramMessage{AccountID: ref.AccountID, ChatID: ref.ChatID, TopicID: ref.TopicID, MessageID: ref.MessageID, Reference: string(b), State: "retired"})
}
