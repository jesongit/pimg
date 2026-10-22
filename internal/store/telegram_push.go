package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

// RunTelegramReceive reads the durable pushed message, never topic history.
// History cursors only advance after catch-up, not on out-of-order pushes.
func (s *Service) RunTelegramReceive(ctx context.Context, payload string) error {
	var p struct {
		AlbumID   int64 `json:"album_id"`
		AccountID int64 `json:"account_id"`
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return err
	}
	if p.AlbumID <= 0 || p.AccountID <= 0 || p.ChatID <= 0 || p.MessageID <= 0 {
		return errors.New("TG 推送任务参数无效")
	}
	if err := s.TGReady(); err != nil {
		return err
	}
	unlock, err := s.lockFile(ctx, fmt.Sprintf("tg-operation:%d", p.AlbumID))
	if err != nil {
		return err
	}
	defer unlock()
	b, err := s.DB.GetTopicBinding(p.AlbumID)
	if errors.Is(err, dbx.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if b.State != "active" {
		return nil
	}
	if b.AccountID != p.AccountID || b.ChatID != p.ChatID || s.UserTG.Status().AccountID != p.AccountID {
		return errors.New("TG 推送任务来源与当前绑定不一致")
	}
	m, err := s.DB.GetTelegramMessage(p.AccountID, p.ChatID, p.MessageID)
	if err != nil {
		return err
	}
	var ref tguser.Reference
	if err = json.Unmarshal([]byte(m.Reference), &ref); err != nil {
		return err
	}
	if ref.AccountID != b.AccountID || ref.ChatID != b.ChatID || ref.TopicID != b.TopicID || ref.MessageID != p.MessageID {
		return errors.New("TG 推送消息不属于绑定话题")
	}
	var parentTask int64
	if e := s.DB.QueryRow(`SELECT id FROM tasks WHERE type='tg_receive' AND json_valid(payload) AND json_extract(payload,'$.album_id')=? AND json_extract(payload,'$.account_id')=? AND json_extract(payload,'$.chat_id')=? AND json_extract(payload,'$.message_id')=? ORDER BY id DESC LIMIT 1`, p.AlbumID, p.AccountID, p.ChatID, p.MessageID).Scan(&parentTask); e == nil {
		ctx = WithIntakeParent(ctx, parentTask)
	}
	_, _, err = s.receiveTelegramMessage(context.WithValue(ctx, automaticTelegramScanKey{}, true), tguser.Message{Reference: ref, Date: m.Date, Name: m.Name, MIME: m.MIME, Caption: m.Caption, IsService: m.IsService}, p.AlbumID)
	return err
}
