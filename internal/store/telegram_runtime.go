package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"pimg/internal/dbx"
	"pimg/internal/proxytraffic"
	"pimg/internal/tguser"
)

type telegramUserClient interface {
	tguser.Transport
	Run(context.Context) error
	AuthPhone(context.Context, string) error
	AuthCode(context.Context, string) error
	AuthPassword(context.Context, string) error
	RestartAuth(context.Context) error
	Logout(context.Context) error
}

type telegramRuntime struct {
	mu sync.RWMutex
	// Never wait for Run while holding mu: its callback may need that mutex.
	lifecycle sync.Mutex
	service   *Service
	client    telegramUserClient
	newClient func(tguser.Options) telegramUserClient
	cancel    context.CancelFunc
	done      chan struct{}
	lastError string
	root      context.Context
	apiID     int
	apiHash   string
	chatID    int64
}

func (r *telegramRuntime) current() telegramUserClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.client
}
func (s *Service) pinTelegramAccount(account int64) error {
	if account <= 0 {
		return errors.New("Telegram 账号身份尚未读取")
	}
	pinned, err := s.DB.GetSetting("telegram.account_id")
	if err != nil {
		return errors.New("无法读取固定 Telegram 账号")
	}
	if pinned == "" {
		if _, err = s.DB.Exec("INSERT INTO settings(k,v) VALUES('telegram.account_id',?) ON CONFLICT(k) DO NOTHING", strconv.FormatInt(account, 10)); err != nil {
			return errors.New("无法记录固定 Telegram 账号")
		}
		pinned, err = s.DB.GetSetting("telegram.account_id")
		if err != nil {
			return errors.New("无法读取固定 Telegram 账号")
		}
	}
	if pinned != strconv.FormatInt(account, 10) {
		return errors.New("当前登录的 Telegram 账号与固定归档账号不同，请退出后重新授权原账号")
	}
	return nil
}
func (r *telegramRuntime) Status() tguser.Status {
	r.mu.RLock()
	c, runError := r.client, r.lastError
	r.mu.RUnlock()
	if c == nil {
		return tguser.Status{State: "unconfigured"}
	}
	st := c.Status()
	if runError != "" {
		st.State = "error"
		st.Error = runError
		return st
	}
	if st.State == "authorized" {
		if err := r.service.pinTelegramAccount(st.AccountID); err != nil {
			st.State = "reauth_required"
			st.Error = err.Error()
		}
	}
	return st
}
func (r *telegramRuntime) authorizedClient() (telegramUserClient, error) {
	if c, ok := r.current().(interface{ ProxyTrafficError() error }); ok {
		if err := c.ProxyTrafficError(); err != nil {
			return nil, err
		}
	}
	st := r.Status()
	if st.State != "authorized" {
		if st.Error != "" {
			return nil, errors.New(st.Error)
		}
		return nil, errors.New("请先配置并授权 Telegram 用户账号")
	}
	c := r.current()
	if c == nil {
		return nil, tguser.ErrNotAuthorized
	}
	return c, nil
}
func (r *telegramRuntime) clientForChat(chat int64, write bool) (telegramUserClient, error) {
	c, err := r.authorizedClient()
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	fixed := r.chatID
	r.mu.RUnlock()
	if chat <= 0 || (chat != fixed && !r.service.DB.TelegramChatAllowed(c.Status().AccountID, chat)) {
		return nil, errors.New("Telegram 操作必须使用已固定的归档超级群")
	}
	return c, nil
}
func (r *telegramRuntime) Groups(ctx context.Context) ([]tguser.Group, error) {
	c, e := r.authorizedClient()
	if e != nil {
		return nil, e
	}
	return c.Groups(ctx)
}
func (r *telegramRuntime) Topics(ctx context.Context, chat int64) ([]tguser.Topic, error) {
	c, e := r.clientForChat(chat, false)
	if e != nil {
		return nil, e
	}
	return c.Topics(ctx, chat)
}
func (r *telegramRuntime) CreateTopic(ctx context.Context, chat int64, name string) (tguser.Topic, error) {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return tguser.Topic{}, e
	}
	return c.CreateTopic(ctx, chat, name)
}
func (r *telegramRuntime) RenameTopic(ctx context.Context, chat int64, topic int, name string) error {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return e
	}
	return c.RenameTopic(ctx, chat, topic, name)
}
func (r *telegramRuntime) CloseTopic(ctx context.Context, chat int64, topic int, closed bool) error {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return e
	}
	return c.CloseTopic(ctx, chat, topic, closed)
}
func (r *telegramRuntime) DeleteTopic(ctx context.Context, chat int64, topic int) error {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return e
	}
	return c.DeleteTopic(ctx, chat, topic)
}
func (r *telegramRuntime) Upload(ctx context.Context, chat int64, topic int, name, mime string, data, thumb []byte, caption string, random int64) (tguser.Reference, error) {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return tguser.Reference{}, e
	}
	return c.Upload(ctx, chat, topic, name, mime, data, thumb, caption, random)
}
func (r *telegramRuntime) UploadVideo(ctx context.Context, chat int64, topic int, name, mime string, data, thumb []byte, caption string, random int64, meta tguser.VideoMetadata) (tguser.Reference, error) {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return tguser.Reference{}, e
	}
	v, ok := c.(tguser.VideoUploader)
	if !ok {
		return tguser.Reference{}, errors.New("当前 Telegram 客户端不支持视频上传")
	}
	return v.UploadVideo(ctx, chat, topic, name, mime, data, thumb, caption, random, meta)
}

func (r *telegramRuntime) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, source io.Reader, size int64, thumb []byte, caption string, random int64, video *tguser.VideoMetadata) (tguser.Reference, error) {
	c, err := r.clientForChat(chat, true)
	if err != nil {
		return tguser.Reference{}, err
	}
	stream, ok := c.(tguser.StreamTransport)
	if !ok {
		return tguser.Reference{}, errors.New("当前 Telegram 客户端不支持流式传输")
	}
	return stream.UploadStream(ctx, chat, topic, name, mime, source, size, thumb, caption, random, video)
}
func (r *telegramRuntime) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	c, err := r.clientForChat(ref.ChatID, false)
	if err != nil {
		return nil, err
	}
	stream, ok := c.(tguser.StreamTransport)
	if !ok {
		return nil, errors.New("当前 Telegram 客户端不支持流式传输")
	}
	return stream.OpenStream(ctx, ref)
}
func (r *telegramRuntime) Download(ctx context.Context, ref tguser.Reference) ([]byte, error) {
	c, e := r.clientForChat(ref.ChatID, false)
	if e != nil {
		return nil, e
	}
	return c.Download(ctx, ref)
}
func (r *telegramRuntime) Copy(ctx context.Context, ref tguser.Reference, chat int64, topic int, random int64) (tguser.Reference, error) {
	c, e := r.clientForChat(chat, true)
	if e != nil {
		return tguser.Reference{}, e
	}
	if ref.ChatID != chat {
		return tguser.Reference{}, errors.New("Telegram 归档不支持静默更换群")
	}
	return c.Copy(ctx, ref, chat, topic, random)
}
func (r *telegramRuntime) Delete(ctx context.Context, ref tguser.Reference) error {
	c, e := r.clientForChat(ref.ChatID, true)
	if e != nil {
		return e
	}
	return c.Delete(ctx, ref)
}
func (r *telegramRuntime) History(ctx context.Context, chat int64, topic int, since, until time.Time, offset, limit int) (tguser.Page, error) {
	c, e := r.clientForChat(chat, false)
	if e != nil {
		return tguser.Page{}, e
	}
	return c.History(ctx, chat, topic, since, until, offset, limit)
}

func (s *Service) initTelegramRuntime() error {
	m, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	c := s.storageConnection()
	id, hash := c.APIID, c.APIHash
	chat := s.Cfg.Telegram.ChatID
	if v := m["telegram.chat_id"]; v != "" {
		chat, _ = strconv.ParseInt(v, 10, 64)
	} else if chat > 0 {
		if err = s.DB.SetSetting("telegram.chat_id", strconv.FormatInt(chat, 10)); err != nil {
			return err
		}
	}
	r := &telegramRuntime{service: s, apiID: id, apiHash: hash, chatID: chat}
	s.telegram = r
	s.UserTG = r
	r.makeClientLocked()
	return nil
}
func (r *telegramRuntime) sessionFile() string {
	path := r.service.Cfg.Telegram.SessionFile
	if path == "" {
		path = filepath.Join(r.service.Cfg.Paths.DataDir, "telegram", "session.enc")
	}
	return path
}
func (r *telegramRuntime) makeClientLocked() {
	r.mu.RLock()
	id, hash := r.apiID, r.apiHash
	r.mu.RUnlock()
	options := tguser.Options{APIID: id, APIHash: hash, SessionFile: r.sessionFile(), SessionKey: r.service.Master.Raw(), ProxyURL: r.service.storageConnection().ProxyURL, ProxyTraffic: r.service.proxyTraffic, Logger: r.service.Log, OnMessage: r.onMessage, OnReady: r.service.WakeTelegramSync, OnGap: r.onGap}
	var c telegramUserClient
	if r.newClient != nil {
		c = r.newClient(options)
	} else {
		c = tguser.New(options)
	}
	r.mu.Lock()
	r.client = c
	r.lastError = ""
	r.mu.Unlock()
}

func (r *telegramRuntime) onGap(chat int64) {
	r.mu.RLock()
	fixed := r.chatID
	r.mu.RUnlock()
	if chat == 0 || chat == fixed || r.service.DB.TelegramChatAllowed(r.Status().AccountID, chat) {
		r.service.WakeTelegramSync()
	}
}

func (r *telegramRuntime) onMessage(message tguser.Message) {
	r.mu.RLock()
	chatID := r.chatID
	r.mu.RUnlock()
	ref := message.Reference
	if ref.ChatID != chatID && !r.service.DB.TelegramChatAllowed(ref.AccountID, ref.ChatID) {
		return
	}
	status := r.Status()
	if status.State != "authorized" || ref.AccountID != status.AccountID {
		return
	}
	binding, err := r.service.DB.TopicBindingBySource(ref.AccountID, ref.ChatID, ref.TopicID)
	if err != nil || binding.State != "active" {
		return
	}
	if message.IsService {
		if (message.Caption == "topic_created" || message.Caption == "topic_edited") && message.Name != "" && !strings.HasPrefix(message.Name, "Pimg-") && message.Name != binding.Name {
			if err := r.service.DB.FollowTelegramTopicName(binding.AlbumID, message.Name); err != nil {
				r.service.Log.Warn("TG 话题名称更新失败", "err", err)
			}
		}
		return
	}
	if !isTelegramMedia(message) || binding.ScanDate == 0 && binding.ScanTaskID == 0 {
		return
	}
	var caption struct {
		App string `json:"app"`
	}
	_ = json.Unmarshal([]byte(message.Caption), &caption)
	// Own acknowledgements can arrive before their managed receipts. Uploader
	// files still enter the dedicated intake branch even with Pimg captions.
	if !uploaderName.MatchString(message.Name) && (caption.App == "pimg" || caption.App == "pimg-part2") {
		return
	}
	refJSON, err := json.Marshal(ref)
	if err == nil {
		_, err = r.service.DB.QueueTelegramMessage(binding.AlbumID, &dbx.TelegramMessage{AccountID: ref.AccountID, ChatID: ref.ChatID, TopicID: ref.TopicID, MessageID: ref.MessageID, Date: message.Date, Reference: string(refJSON), Name: message.Name, MIME: message.MIME, Caption: message.Caption, State: "pending"})
	}
	if err != nil {
		r.service.Log.Warn("TG 推送保存失败，安排增量补漏", "err", err)
		r.service.WakeTelegramSync()
	}
}
func channelDone(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
func (r *telegramRuntime) stopLocked(ctx context.Context) error {
	r.mu.RLock()
	cancel, done := r.cancel, r.done
	r.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("等待旧 Telegram 会话退出超时；未启动第二个会话")
		}
	}
	r.mu.Lock()
	r.cancel = nil
	r.done = nil
	r.mu.Unlock()
	return nil
}

// The OS lock spans Run, including final session writes. Never unlink a held
// lock file: the operating system releases its lock on normal exit or a crash.
func (r *telegramRuntime) launchLocked() error {
	r.mu.RLock()
	root, c, id, hash, done := r.root, r.client, r.apiID, r.apiHash, r.done
	r.mu.RUnlock()
	if root == nil || id <= 0 || hash == "" {
		return nil
	}
	if !channelDone(done) {
		return nil
	}
	if root.Err() != nil {
		return root.Err()
	}
	if err := os.MkdirAll(filepath.Dir(r.sessionFile()), 0o700); err != nil {
		return errors.New("无法创建 Telegram 会话目录")
	}
	lock := flock.New(r.sessionFile()+".lock", flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		_ = lock.Close()
		message := "Telegram 会话正由另一个 PIMG 进程使用；请停止服务后再运行 CLI 校验或恢复"
		if err != nil {
			message = "无法锁定 Telegram 会话，请检查目录权限"
		}
		r.mu.Lock()
		r.lastError = message
		r.mu.Unlock()
		return errors.New(message)
	}
	ctx, cancel := context.WithCancel(root)
	finished := make(chan struct{})
	r.mu.Lock()
	r.cancel = cancel
	r.done = finished
	r.lastError = ""
	r.mu.Unlock()
	go func() {
		defer close(finished)
		defer lock.Close()
		defer cancel()
		monitorDone := make(chan struct{})
		go func() {
			defer close(monitorDone)
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = r.Status()
				}
			}
		}()
		var err error
		for {
			err = c.Run(ctx)
			if ctx.Err() != nil || !errors.Is(err, proxytraffic.ErrLimit) {
				break
			}
			// A quota pause must not require a manual Telegram reconnect.
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				if r.service.proxyTraffic.Check() == nil {
					break
				}
			}
		}
		if ctx.Err() == nil {
			message := "Telegram 用户连接已停止，请在设置中重新连接"
			if err == nil {
				message = "Telegram 用户连接已退出，请重新连接"
			}
			r.mu.Lock()
			if r.client == c {
				r.lastError = message
			}
			r.mu.Unlock()
			if r.service.Log != nil {
				r.service.Log.Warn(message)
			}
		}
		cancel()
		<-monitorDone
	}()
	return nil
}
func (s *Service) StartTelegramRuntime(ctx context.Context) {
	if s.telegram == nil {
		return
	}
	r := s.telegram
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.RLock()
	active := r.root != nil && r.root.Err() == nil && !channelDone(r.done)
	r.mu.RUnlock()
	if active {
		return
	}
	if err := r.stopLocked(ctx); err != nil {
		return
	}
	r.mu.Lock()
	r.root = ctx
	r.mu.Unlock()
	r.makeClientLocked()
	_ = r.launchLocked()
}

func (s *Service) StopTelegramRuntime(ctx context.Context) error {
	if s.telegram == nil {
		return nil
	}
	r := s.telegram
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if err := r.stopLocked(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.root = nil
	r.mu.Unlock()
	return nil
}
func (s *Service) TGChatID() int64 {
	if s.telegram != nil {
		s.telegram.mu.RLock()
		defer s.telegram.mu.RUnlock()
		return s.telegram.chatID
	}
	return s.Cfg.Telegram.ChatID
}
func (s *Service) TGReady() error {
	if s.telegram != nil {
		if c, ok := s.telegram.current().(interface{ ProxyTrafficError() error }); ok {
			if err := c.ProxyTrafficError(); err != nil {
				return err
			}
		}
	}
	if s.UserTG == nil {
		return errors.New("Telegram 用户账号未配置")
	}
	st := s.UserTG.Status()
	if st.State != "authorized" {
		if st.Error != "" {
			return errors.New(st.Error)
		}
		return errors.New("请先在设置中完成 Telegram 用户账号授权")
	}
	if err := s.pinTelegramAccount(st.AccountID); err != nil {
		return err
	}
	return nil
}
func (s *Service) TelegramStatus() map[string]any {
	st := tguser.Status{State: "unconfigured"}
	if s.UserTG != nil {
		st = s.UserTG.Status()
	}
	b, _ := json.Marshal(st)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	out["chat_id"] = s.AdminTGChatID()
	spaces := []map[string]any{}
	if owners, e := s.DB.AccountIDs(); e == nil {
		for _, owner := range owners {
			item := map[string]any{"owner_id": owner, "state": "pending", "chat_id": int64(0)}
			if b, e := s.DB.TelegramSpace(owner); e == nil {
				item["state"] = b.State
				item["chat_id"] = b.ChatID
				item["last_error"] = b.LastError
			}
			spaces = append(spaces, item)
		}
	}
	out["archive_spaces"] = spaces
	return out
}
func (s *Service) ConfigureTelegramGroup(ctx context.Context, chat int64) error {
	if s.telegram == nil {
		return errors.New("开发模拟环境不支持真实账号授权")
	}
	if space, e := s.DB.TelegramSpace(dbx.AdminOwner); e == nil && space.ChatID != chat {
		return errors.New("归档群由系统分配，已有空间不能直接更换群")
	}
	r := s.telegram
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.RLock()
	oldChat := r.chatID
	r.mu.RUnlock()
	if chat <= 0 {
		return errors.New("请从群列表选择论坛超级群")
	}
	if oldChat > 0 && oldChat != chat {
		return errors.New("归档群已固定，不能直接更换或清空")
	}
	{
		groups, err := r.Groups(ctx)
		if err != nil {
			return err
		}
		valid := false
		for _, g := range groups {
			if g.ID == chat {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("所选群不是拥有话题管理及删除权限的论坛超级群")
		}
	}
	if _, err := s.DB.Exec("INSERT INTO settings(k,v) VALUES('telegram.chat_id',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", strconv.FormatInt(chat, 10)); err != nil {
		return err
	}
	r.mu.Lock()
	r.chatID = chat
	r.mu.Unlock()
	return nil
}

func (s *Service) ReconnectTelegram(ctx context.Context) error {
	if s.telegram == nil {
		return errors.New("Telegram 用户运行时不可用")
	}
	r := s.telegram
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if err := r.stopLocked(ctx); err != nil {
		return err
	}
	if c := s.storageConnection(); c.APIID > 0 && c.APIHash != "" {
		r.mu.Lock()
		r.apiID, r.apiHash = c.APIID, c.APIHash
		r.mu.Unlock()
	}
	r.makeClientLocked()
	return r.launchLocked()
}
func (s *Service) TelegramAuth(ctx context.Context, action, value string) error {
	if s.telegram == nil {
		return errors.New("用户账号授权不可用")
	}
	r := s.telegram
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.RLock()
	done, root := r.done, r.root
	r.mu.RUnlock()
	if action != "logout" && channelDone(done) {
		if root == nil || root.Err() != nil {
			return errors.New("Telegram 运行时尚未启动")
		}
		if err := r.stopLocked(ctx); err != nil {
			return err
		}
		r.makeClientLocked()
		if err := r.launchLocked(); err != nil {
			return err
		}
	}
	c := r.current()
	if c == nil {
		return errors.New("请先保存 API ID 和 API Hash")
	}
	var err error
	switch action {
	case "phone":
		err = c.AuthPhone(ctx, value)
	case "code":
		err = c.AuthCode(ctx, value)
	case "password":
		err = c.AuthPassword(ctx, value)
	case "restart":
		err = c.RestartAuth(ctx)
	case "logout":
		return c.Logout(ctx)
	default:
		return fmt.Errorf("未知授权步骤")
	}
	if err != nil {
		return err
	}
	if st := c.Status(); st.State == "authorized" {
		return s.pinTelegramAccount(st.AccountID)
	}
	return nil
}

// ConnectTelegramSession restores only an existing session for offline CLI use.
// Interactive login belongs to the web UI. A server holding the same auth key
// must be stopped first; no second client may write the session concurrently.
func (s *Service) ConnectTelegramSession(ctx context.Context) (func(), error) {
	if s.telegram == nil {
		return nil, errors.New("Telegram 用户会话运行时不可用")
	}
	r := s.telegram
	r.lifecycle.Lock()
	r.mu.RLock()
	started := r.root != nil
	r.mu.RUnlock()
	if started {
		r.lifecycle.Unlock()
		return nil, errors.New("Telegram 运行时已启动，不能另外打开同一会话")
	}
	info, err := os.Stat(r.sessionFile())
	if err != nil || !info.Mode().IsRegular() {
		r.lifecycle.Unlock()
		return nil, errors.New("缺少已授权的 Telegram 会话，请先在网页完成登录")
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.root = runtimeCtx
	r.mu.Unlock()
	r.makeClientLocked()
	err = r.launchLocked()
	r.lifecycle.Unlock()
	cleanup := func() {
		cancel()
		r.lifecycle.Lock()
		defer r.lifecycle.Unlock()
		_ = r.stopLocked(context.Background())
		r.mu.Lock()
		r.root = nil
		r.mu.Unlock()
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	waitCtx, stopWaiting := context.WithTimeout(ctx, 60*time.Second)
	defer stopWaiting()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		st := r.Status()
		switch st.State {
		case "authorized":
			if err = s.TGReady(); err != nil {
				cleanup()
				return nil, err
			}
			return cleanup, nil
		case "phone", "code", "password", "reauth_required", "unconfigured":
			cleanup()
			if st.Error != "" {
				return nil, errors.New(st.Error)
			}
			return nil, errors.New("Telegram 会话尚未授权或已失效，请先在网页完成登录")
		case "error":
			cleanup()
			return nil, errors.New(st.Error)
		}
		select {
		case <-waitCtx.Done():
			cleanup()
			return nil, errors.New("等待 Telegram 已授权会话连接超时")
		case <-ticker.C:
		}
	}
}
