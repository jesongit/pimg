package store

import (
	"context"
	"errors"
	"fmt"
	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/tguser"
	"strings"
	"time"
)

const TaskAccountSpace = "account_space"

var ErrTelegramArchiveDisabled = errors.New("Telegram 归档未启用")

func (s *Service) AdminTGChatID() int64 {
	if b, e := s.DB.TelegramSpace(dbx.AdminOwner); e == nil && b.State == "ready" {
		return b.ChatID
	}
	return s.TGChatID()
}

// Choose an archive that this owner can actually use. Pending TG provisioning
// must not block an already available local/object-store archive.
func (s *Service) initialArchiveForOwner(owner string) string {
	for _, id := range s.rankedArchives() {
		instance, err := s.instance(id)
		if err != nil || !instance.row.Enabled || instance.cold == nil {
			continue
		}
		if instance.row.Driver == "telegram" && s.UserTG != nil {
			if s.TGReady() != nil {
				continue
			}
			space, e := s.DB.TelegramSpace(owner)
			if e != nil || space.State != "ready" {
				if dbx.NormalizeOwner(owner) != dbx.AdminOwner || s.TGChatID() <= 0 {
					continue
				}
			}
		}
		return id
	}
	return ""
}

func (s *Service) ReconcileAccountSpaces() error {
	owners, err := s.DB.AccountIDs()
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if _, err = s.DB.EnsureOwnerDefaultAlbum(owner); err != nil {
			return err
		}
		if s.Cfg.Paths.LegacyDir != "" {
			if _, err = s.DB.Exec(`UPDATE files SET album_id=(SELECT id FROM albums WHERE owner_id=? AND is_default=1) WHERE owner_id=? AND album_id IS NULL AND purge_state IS NULL`, owner, owner); err != nil {
				return err
			}
		}
		if !s.UsesStorageDriver("telegram") || s.UserTG == nil {
			continue
		}
		if b, e := s.DB.TelegramSpace(owner); e == nil && b.State == "ready" && s.telegramFolderSynced(b) {
			continue
		}
		if _, err = s.DB.EnqueueUniqueTask(TaskAccountSpace, "tg-space:"+owner, dbx.JSONValue(map[string]string{"owner_id": owner})); err != nil {
			return err
		}
	}
	return s.queuePendingGroupTitles()
}

func telegramFolderKey(space *dbx.TelegramSpace) string {
	return fmt.Sprintf("telegram.album_folder.%d.%d", space.AccountID, space.ChatID)
}

func (s *Service) telegramFolderSynced(space *dbx.TelegramSpace) bool {
	value, err := s.DB.GetSetting(telegramFolderKey(space))
	return err == nil && value == "synced"
}

// Folder membership is a separate, retryable step after saving the group ID.
// A folder failure must never turn a committed group into another create.
func (s *Service) EnsureTelegramArchiveFolder(ctx context.Context, space *dbx.TelegramSpace) error {
	if space == nil || space.State != "ready" || space.ChatID <= 0 {
		return errors.New("TG 归档群尚未就绪")
	}
	if space.AccountID != s.UserTG.Status().AccountID {
		return tguser.ErrAccountMismatch
	}
	if s.telegramFolderSynced(space) {
		return nil
	}
	if space.RetryAfter > time.Now().Unix() {
		return fmt.Errorf("TG 文件夹整理等待至 %d 后重试", space.RetryAfter)
	}
	organizer, ok := s.UserTG.(tguser.ArchiveFolderOrganizer)
	if !ok {
		return errors.New("TG 连接不支持归档文件夹整理")
	}
	_, err := organizer.SyncArchiveFolder(ctx, []int64{space.ChatID})
	if err != nil {
		after := time.Now().Add(time.Minute).Unix()
		var wait *tguser.RetryAfterError
		if errors.As(err, &wait) {
			after = time.Now().Add(wait.Wait).Unix()
		}
		_, saveErr := s.DB.Exec(`UPDATE account_telegram SET last_error=?,retry_after=? WHERE owner_id=? AND account_id=? AND chat_id=? AND state='ready'`, "加入 TG “相册”文件夹失败: "+err.Error(), after, space.OwnerID, space.AccountID, space.ChatID)
		return errors.Join(err, saveErr)
	}
	if err = s.DB.SetSetting(telegramFolderKey(space), "synced"); err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE account_telegram SET last_error='',retry_after=0 WHERE owner_id=? AND account_id=? AND chat_id=? AND state='ready'`, space.OwnerID, space.AccountID, space.ChatID)
	return err
}

func (r *telegramRuntime) SyncArchiveFolder(ctx context.Context, chats []int64) (tguser.ArchiveFolderResult, error) {
	c, err := r.authorizedClient()
	if err != nil {
		return tguser.ArchiveFolderResult{}, err
	}
	organizer, ok := c.(tguser.ArchiveFolderOrganizer)
	if !ok {
		return tguser.ArchiveFolderResult{}, errors.New("TG 客户端不支持归档文件夹整理")
	}
	return organizer.SyncArchiveFolder(ctx, chats)
}

func (s *Service) EnsureTelegramSpace(ctx context.Context, owner string) (*dbx.TelegramSpace, error) {
	owner = dbx.NormalizeOwner(owner)
	if !s.DB.AccountActive(owner) {
		return nil, errors.New("账号已停用或不存在")
	}
	if err := s.TGReady(); err != nil {
		return nil, err
	}
	unlock, err := s.lockFile(ctx, "tg-space:"+owner)
	if err != nil {
		return nil, err
	}
	defer unlock()
	account := s.UserTG.Status().AccountID
	legacy := int64(0)
	if owner == dbx.AdminOwner {
		legacy = s.TGChatID()
	}
	state := "pending"
	if legacy > 0 {
		state = "ready"
	}
	_, err = s.DB.Exec(`INSERT INTO account_telegram(owner_id,account_id,chat_id,create_token,state) VALUES(?,?,?,?,?) ON CONFLICT(owner_id) DO NOTHING`, owner, account, legacy, ids.New(), state)
	if err != nil {
		return nil, err
	}
	b, err := s.DB.TelegramSpace(owner)
	if err != nil {
		return nil, err
	}
	if b.AccountID != account {
		return b, errors.New("归档空间绑定的 TG 登录身份不一致")
	}
	if b.State == "ready" {
		return b, nil
	}
	if !s.UsesStorageDriver("telegram") {
		return b, ErrTelegramArchiveDisabled
	}
	if b.RetryAfter > time.Now().Unix() {
		return b, fmt.Errorf("TG 建群等待至 %d 后重试", b.RetryAfter)
	}
	provisioner, ok := s.UserTG.(tguser.GroupProvisioner)
	if !ok {
		return b, errors.New("TG 连接不支持自动创建归档群")
	}
	marker := "pimg-managed-group:v1:" + b.Token
	group, found, err := provisioner.FindManagedGroup(ctx, marker)
	if err != nil {
		var retry *tguser.RetryAfterError
		if errors.As(err, &retry) {
			_, saveErr := s.DB.Exec(`UPDATE account_telegram SET retry_after=?,last_error=? WHERE owner_id=?`, time.Now().Add(retry.Wait).Unix(), err.Error(), owner)
			return b, errors.Join(err, saveErr)
		}
		return b, err
	}
	if !found {
		if b.State == "creating" || b.State == "uncertain" {
			return b, errors.New("TG 建群结果待核对，尚未发现原群；保留任务，避免重复创建")
		}
		if b.State == "blocked" {
			return b, errors.New(b.LastError)
		}
		title, titleErr := s.accountGroupTitle(owner)
		if titleErr != nil {
			return b, titleErr
		}
		claim, e := s.DB.Exec(`UPDATE account_telegram SET state='creating',attempts=attempts+1 WHERE owner_id=? AND state IN ('pending','failed')`, owner)
		if e != nil {
			return b, e
		}
		n, _ := claim.RowsAffected()
		if n != 1 {
			return b, errors.New("归档群正在由另一任务创建")
		}
		group, err = provisioner.CreateManagedGroup(ctx, title, marker)
		if err != nil {
			// A transport timeout is ambiguous. Never blindly issue a second create.
			next := "uncertain"
			retry := time.Now().Add(time.Minute).Unix()
			msg := err.Error()
			var wait *tguser.RetryAfterError
			if errors.As(err, &wait) {
				next = "failed"
				retry = time.Now().Add(wait.Wait).Unix()
			}
			if strings.Contains(msg, "FLOOD_WAIT") || strings.Contains(msg, "CHANNELS_TOO_MUCH") || strings.Contains(msg, "USER_RESTRICTED") {
				next = "failed"
				if strings.Contains(msg, "CHANNELS_TOO_MUCH") || strings.Contains(msg, "USER_RESTRICTED") {
					next = "blocked"
				}
				if wait == nil {
					retry = time.Now().Add(5 * time.Minute).Unix()
				}
			}
			_, e = s.DB.Exec(`UPDATE account_telegram SET state=?,last_error=?,retry_after=? WHERE owner_id=?`, next, msg, retry, owner)
			return b, errors.Join(err, e)
		}
	}
	if group.ID <= 0 {
		return b, errors.New("TG 建群结果没有群 ID")
	}
	_, err = s.DB.Exec(`UPDATE account_telegram SET chat_id=?,chat_title=?,state='ready',last_error='',retry_after=0 WHERE owner_id=? AND account_id=?`, group.ID, group.Name, owner, account)
	if err != nil {
		return b, err
	}
	return s.DB.TelegramSpace(owner)
}

func (r *telegramRuntime) FindManagedGroup(ctx context.Context, marker string) (tguser.Group, bool, error) {
	c, err := r.authorizedClient()
	if err != nil {
		return tguser.Group{}, false, err
	}
	p, ok := c.(tguser.GroupProvisioner)
	if !ok {
		return tguser.Group{}, false, errors.New("TG 客户端不支持归档群查找")
	}
	return p.FindManagedGroup(ctx, marker)
}
func (r *telegramRuntime) CreateManagedGroup(ctx context.Context, title, marker string) (tguser.Group, error) {
	c, err := r.authorizedClient()
	if err != nil {
		return tguser.Group{}, err
	}
	p, ok := c.(tguser.GroupProvisioner)
	if !ok {
		return tguser.Group{}, errors.New("TG 客户端不支持建群")
	}
	return p.CreateManagedGroup(ctx, title, marker)
}

func (s *Service) albumChat(ctx context.Context, albumID int64) (int64, error) {
	a, err := s.DB.GetAlbum(albumID)
	if err != nil {
		return 0, err
	}
	b, err := s.EnsureTelegramSpace(ctx, a.OwnerID)
	if err != nil {
		return 0, err
	}
	return b.ChatID, nil
}
