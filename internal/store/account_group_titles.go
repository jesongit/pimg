package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

const TaskAccountGroupTitle = "account_group_title"

func (s *Service) accountGroupTitle(owner string) (string, error) {
	var username string
	var err error
	if owner == dbx.AdminOwner {
		username, _, err = s.DB.GetAdmin()
	} else {
		err = s.DB.QueryRow(`SELECT username FROM accounts WHERE id=?`, owner).Scan(&username)
	}
	if err != nil {
		return "", err
	}
	return username, nil
}

func (s *Service) queueAccountGroupTitle(owner, name string) (int64, error) {
	id, err := s.DB.EnqueueUniqueTask(TaskAccountGroupTitle, "tg-title:"+owner, dbx.JSONValue(map[string]string{"owner_id": owner, "name": name}))
	if err == nil {
		_, err = s.DB.Exec(`UPDATE tasks SET state='pending',attempts=0,run_after=MAX(unixepoch(),COALESCE((SELECT title_retry_after FROM account_telegram WHERE owner_id=?),0)),last_error=NULL WHERE id=? AND state='failed'`, owner, id)
	}
	return id, err
}

func (s *Service) QueueAccountGroupTitle(owner, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if !tguser.ValidGroupTitle(name) {
		return 0, errors.New("群名须为 1–128 个字符，不能包含换行")
	}
	b, err := s.DB.TelegramSpace(owner)
	if err != nil {
		return 0, err
	}
	if b.State != "ready" || b.ChatID <= 0 {
		return 0, errors.New("归档群尚未就绪，请先完成分配")
	}
	if _, err = s.DB.Exec(`UPDATE account_telegram SET desired_title=?,title_error='' WHERE owner_id=?`, name, owner); err != nil {
		return 0, err
	}
	return s.queueAccountGroupTitle(owner, name)
}

func (s *Service) queuePendingGroupTitles() error {
	rows, err := s.DB.Query(`SELECT owner_id,desired_title FROM account_telegram WHERE state='ready' AND desired_title!=''`)
	if err != nil {
		return err
	}
	var pending [][2]string
	for rows.Next() {
		var p [2]string
		if err = rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range pending {
		if _, err = s.queueAccountGroupTitle(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RunAccountGroupTitle(ctx context.Context, owner, name string) error {
	unlock, err := s.lockFile(ctx, "tg-space:"+owner)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := s.DB.TelegramSpace(owner)
	if err != nil {
		return err
	}
	if b.DesiredTitle != name {
		return nil
	} // A later edit owns a separate durable task.
	if b.State != "ready" || b.ChatID <= 0 {
		return errors.New("归档群尚未就绪")
	}
	if b.TitleRetryAfter > time.Now().Unix() {
		return fmt.Errorf("TG 群名等待至 %d 后重试", b.TitleRetryAfter)
	}
	err = s.TGReady()
	if err == nil && s.UserTG.Status().AccountID != b.AccountID {
		err = tguser.ErrAccountMismatch
	}
	if err == nil {
		if rename, ok := s.UserTG.(tguser.GroupRenamer); ok {
			err = rename.RenameGroup(ctx, b.ChatID, name)
		} else {
			err = errors.New("TG 连接不支持修改群名")
		}
	}
	if err != nil {
		after := time.Now().Add(time.Minute).Unix()
		var wait *tguser.RetryAfterError
		if errors.As(err, &wait) {
			after = time.Now().Add(wait.Wait).Unix()
		}
		_, saveErr := s.DB.Exec(`UPDATE account_telegram SET title_error=?,title_retry_after=? WHERE owner_id=? AND desired_title=?`, err.Error(), after, owner, name)
		return errors.Join(err, saveErr)
	}
	_, err = s.DB.Exec(`UPDATE account_telegram SET chat_title=?,desired_title=CASE WHEN desired_title=? THEN '' ELSE desired_title END,title_error=CASE WHEN desired_title=? THEN '' ELSE title_error END,title_retry_after=0 WHERE owner_id=? AND account_id=? AND chat_id=?`, name, name, name, owner, b.AccountID, b.ChatID)
	return err
}

func (s *Service) RefreshAccountGroupNames(ctx context.Context) error {
	if err := s.TGReady(); err != nil {
		return err
	}
	groups, err := s.UserTG.Groups(ctx)
	if err != nil {
		return err
	}
	account := s.UserTG.Status().AccountID
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range groups {
		if _, err = tx.Exec(`UPDATE account_telegram SET chat_title=? WHERE account_id=? AND chat_id=? AND state='ready'`, g.Name, account, g.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *telegramRuntime) RenameGroup(ctx context.Context, chat int64, name string) error {
	c, err := r.clientForChat(chat, true)
	if err != nil {
		return err
	}
	if rename, ok := c.(tguser.GroupRenamer); ok {
		return rename.RenameGroup(ctx, chat, name)
	}
	return errors.New("TG 客户端不支持修改群名")
}
