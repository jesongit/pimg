package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pimg/internal/dbx"
)

func (s *Server) telegramError(w http.ResponseWriter, err error) {
	if errors.Is(err, dbx.ErrNotFound) {
		fail(w, 404, "相册或话题不存在")
		return
	}
	fail(w, 409, err.Error())
}
func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Svc.TelegramStatus())
}
func (s *Server) handleTelegramConfig(w http.ResponseWriter, r *http.Request) {
	var p struct {
		APIID   *int    `json:"api_id"`
		APIHash *string `json:"api_hash"`
		ChatID  *int64  `json:"chat_id"`
	}
	if err := readJSONBody(r, &p, 4096); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if p.APIID != nil || p.APIHash != nil {
		if p.ChatID != nil {
			fail(w, 400, "应用凭据和归档群请分别保存")
			return
		}
		if err := s.Svc.SaveStorageConnection(r.Context(), nil, p.APIID, p.APIHash); err != nil {
			s.telegramError(w, err)
			return
		}
		writeJSON(w, 200, s.Svc.TelegramStatus())
		return
	}

	if p.ChatID == nil {
		fail(w, 400, "请选择归档超级群")
		return
	}
	if err := s.Svc.ConfigureTelegramGroup(r.Context(), *p.ChatID); err != nil {
		s.telegramError(w, err)
		return
	}
	s.Svc.WakeTelegramSync()
	writeJSON(w, 200, s.Svc.TelegramStatus())
}
func (s *Server) handleTelegramAuth(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var p struct {
		Phone    string `json:"phone"`
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if action != "logout" && action != "restart" {
		if err := readJSONBody(r, &p, 4096); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	value := ""
	switch action {
	case "phone":
		value = p.Phone
	case "code":
		value = p.Code
	case "password":
		value = p.Password
	case "logout", "restart":
	default:
		fail(w, 404, "授权操作不存在")
		return
	}
	if action != "logout" && action != "restart" && strings.TrimSpace(value) == "" {
		fail(w, 400, "请填写授权信息")
		return
	}
	if err := s.Svc.TelegramAuth(r.Context(), action, value); err != nil {
		fail(w, 409, err.Error())
		return
	}
	s.Svc.WakeTelegramSync()
	writeJSON(w, 200, s.Svc.TelegramStatus())
}
func (s *Server) handleTelegramGroups(w http.ResponseWriter, r *http.Request) {
	if s.Svc.UserTG == nil {
		fail(w, 409, "请先配置 Telegram 用户账号")
		return
	}
	groups, err := s.Svc.UserTG.Groups(r.Context())
	if err != nil {
		s.telegramError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": groups})
}
func (s *Server) handleTelegramTopics(w http.ResponseWriter, r *http.Request) {
	chat := s.Svc.AdminTGChatID()
	if raw := r.URL.Query().Get("chat_id"); raw != "" {
		var err error
		chat, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fail(w, 400, "群编号无效")
			return
		}
	}
	if chat != s.Svc.AdminTGChatID() || chat <= 0 {
		fail(w, 400, "只能管理已固定的归档群")
		return
	}
	if s.Svc.UserTG == nil {
		fail(w, 409, "用户账号未配置")
		return
	}
	topics, err := s.Svc.UserTG.Topics(r.Context(), chat)
	if err != nil {
		s.telegramError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": topics})
}
func parseTelegramDate(raw string, end bool) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix(), nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return 0, fmt.Errorf("日期格式应为 YYYY-MM-DD")
	}
	if end {
		t = t.AddDate(0, 0, 1).Add(-time.Second)
	}
	return t.Unix(), nil
}
func (s *Server) handleTelegramScan(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "相册编号无效")
		return
	}
	var p struct {
		Mode  string `json:"mode"`
		Since string `json:"since"`
		Until string `json:"until"`
	}
	if err = readJSONBody(r, &p, 2048); err != nil {
		fail(w, 400, err.Error())
		return
	}
	since, err := parseTelegramDate(p.Since, false)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	until, err := parseTelegramDate(p.Until, true)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if p.Mode == "" {
		p.Mode = "latest"
	}
	if p.Mode == "date" && since == 0 {
		fail(w, 400, "请选择扫描起始日期")
		return
	}
	if until != 0 && until < since {
		fail(w, 400, "结束日期不能早于起始日期")
		return
	}
	task, err := s.Svc.QueueTelegramScan(r.Context(), id, p.Mode, since, until)
	if err != nil {
		s.telegramError(w, err)
		return
	}
	s.Eng.Wake()
	writeJSON(w, 202, map[string]any{"task_id": task})
}
func (s *Server) handleTelegramBind(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "相册编号无效")
		return
	}
	var p struct {
		TopicID int `json:"topic_id"`
	}
	if err = readJSONBody(r, &p, 2048); err != nil || p.TopicID <= 1 {
		fail(w, 400, "请选择独立话题")
		return
	}
	if err = s.Svc.BindTelegramTopic(r.Context(), id, p.TopicID); err != nil {
		s.telegramError(w, err)
		return
	}
	s.Svc.WakeTelegramSync()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
