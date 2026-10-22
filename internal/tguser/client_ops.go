package tguser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func manageable(ch *tg.Channel) bool {
	return ch != nil && ch.Megagroup && ch.Forum && !ch.Left && !ch.Min && ch.AccessHash != 0 &&
		(ch.Creator || ch.AdminRights.ManageTopics && ch.AdminRights.DeleteMessages)
}

func (c *Client) remember(chats []tg.ChatClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range chats {
		if ch, ok := v.(*tg.Channel); ok && !ch.Min && ch.AccessHash != 0 {
			cp := *ch
			c.channels[ch.ID] = &cp
		}
	}
}

func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	api, _, err := c.ready()
	if err != nil {
		return nil, err
	}
	found := make(map[int64]Group)
	for _, folder := range []int{0, 1} {
		req := &tg.MessagesGetDialogsRequest{Limit: 100, OffsetPeer: &tg.InputPeerEmpty{}}
		req.SetFolderID(folder)
		lastCursor := ""
		for {
			res, err := api.MessagesGetDialogs(ctx, req)
			if err != nil {
				return nil, c.safeError(err)
			}
			full, ok := res.AsModified()
			if !ok {
				return nil, errors.New("Telegram 群组列表响应无效")
			}
			c.remember(full.GetChats())
			for _, v := range full.GetChats() {
				if ch, ok := v.(*tg.Channel); ok && manageable(ch) {
					found[ch.ID] = Group{ID: ch.ID, Name: ch.Title}
				}
			}
			dialogs := full.GetDialogs()
			if _, complete := res.(*tg.MessagesDialogs); complete || len(dialogs) == 0 {
				break
			}
			var d *tg.Dialog
			for i := len(dialogs) - 1; i >= 0; i-- {
				if v, ok := dialogs[i].(*tg.Dialog); ok {
					d = v
					break
				}
			}
			if d == nil {
				break
			}
			p := dialogInput(d.Peer, full.GetChats(), full.GetUsers())
			if p == nil {
				return nil, errors.New("无法取得 Telegram 群组分页定位")
			}
			req.OffsetID = d.TopMessage
			req.OffsetPeer = p
			req.ExcludePinned = true
			req.OffsetDate = 0
			for _, m := range full.GetMessages() {
				// A dialog's latest message may be a service event (creation,
				// topic or title change), which has the same pagination date.
				if msg, ok := m.AsNotEmpty(); ok && msg.GetID() == d.TopMessage && samePeer(msg.GetPeerID(), d.Peer) {
					req.OffsetDate = msg.GetDate()
					break
				}
			}
			cursor := fmt.Sprintf("%T:%v:%d:%d", d.Peer, peerID(d.Peer), req.OffsetID, req.OffsetDate)
			if cursor == lastCursor {
				return nil, errors.New("Telegram 群组分页没有前进")
			}
			lastCursor = cursor
		}
	}
	out := make([]Group, 0, len(found))
	for _, g := range found {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func peerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerChannel:
		return v.ChannelID
	case *tg.PeerChat:
		return v.ChatID
	case *tg.PeerUser:
		return v.UserID
	}
	return 0
}
func samePeer(a, b tg.PeerClass) bool {
	return fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b) && peerID(a) == peerID(b)
}
func dialogInput(p tg.PeerClass, chats []tg.ChatClass, users []tg.UserClass) tg.InputPeerClass {
	switch v := p.(type) {
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		for _, item := range chats {
			switch ch := item.(type) {
			case *tg.Channel:
				if ch.ID == v.ChannelID && !ch.Min {
					return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
				}
			case *tg.ChannelForbidden:
				if ch.ID == v.ChannelID {
					return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
				}
			}
		}
	case *tg.PeerUser:
		for _, item := range users {
			if u, ok := item.(*tg.User); ok && u.ID == v.UserID {
				return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
			}
		}
	}
	return nil
}

func (c *Client) channel(ctx context.Context, id int64) (*tg.Channel, error) {
	if id <= 0 {
		return nil, errors.New("Telegram 群组 ID 无效")
	}
	if _, _, err := c.ready(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	ch := c.channels[id]
	c.mu.RUnlock()
	if ch == nil {
		if _, err := c.Groups(ctx); err != nil {
			return nil, err
		}
		c.mu.RLock()
		ch = c.channels[id]
		c.mu.RUnlock()
	}
	if !manageable(ch) {
		return nil, errors.New("请选择已加入且拥有话题管理和删除消息权限的超级群组")
	}
	return ch, nil
}
func inputChannel(ch *tg.Channel) *tg.InputChannel {
	return &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
}
func inputPeer(ch *tg.Channel) *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
}

func (c *Client) Topics(ctx context.Context, chatID int64) ([]Topic, error) {
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return nil, err
	}
	api, _, err := c.ready()
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesGetForumTopicsRequest{Peer: inputPeer(ch), Limit: 100}
	out := []Topic{}
	seen := map[int]bool{}
	for {
		res, err := api.MessagesGetForumTopics(ctx, req)
		if err != nil {
			return nil, c.safeError(err)
		}
		c.remember(res.Chats)
		var last *tg.ForumTopic
		for _, v := range res.Topics {
			if t, ok := v.(*tg.ForumTopic); ok {
				last = t
				if !seen[t.ID] {
					out = append(out, Topic{ID: t.ID, Name: t.Title, Closed: t.Closed})
					seen[t.ID] = true
				}
			}
		}
		if len(res.Topics) == 0 || last == nil || len(seen) >= res.Count {
			break
		}
		date := last.Date
		if !res.OrderByCreateDate {
			for _, m := range res.Messages {
				if msg, ok := m.(*tg.Message); ok && msg.ID == last.TopMessage {
					date = msg.Date
					break
				}
			}
		}
		if req.OffsetTopic == last.ID && req.OffsetID == last.TopMessage {
			return nil, errors.New("Telegram 话题分页没有前进")
		}
		req.OffsetTopic = last.ID
		req.OffsetID = last.TopMessage
		req.OffsetDate = date
	}
	return out, nil
}

func (c *Client) CreateTopic(ctx context.Context, chatID int64, name string) (Topic, error) {
	if name == "" || len([]rune(name)) > 128 {
		return Topic{}, errors.New("话题名称须为1至128个字符")
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return Topic{}, err
	}
	api, _, err := c.ready()
	if err != nil {
		return Topic{}, err
	}
	id, err := randomID()
	if err != nil {
		return Topic{}, err
	}
	res, err := api.MessagesCreateForumTopic(ctx, &tg.MessagesCreateForumTopicRequest{Peer: inputPeer(ch), Title: name, RandomID: id})
	if err != nil {
		return Topic{}, c.safeError(err)
	}
	for _, m := range updatesMessages(res) {
		if service, ok := m.(*tg.MessageService); ok {
			if _, ok := service.Action.(*tg.MessageActionTopicCreate); ok {
				return Topic{ID: service.ID, Name: name}, nil
			}
		}
	}
	return Topic{}, errors.New("Telegram 未返回新话题定位")
}
func (c *Client) RenameTopic(ctx context.Context, chatID int64, topicID int, name string) error {
	if topicID <= 1 {
		return errors.New("系统话题不能重命名")
	}
	if name == "" || len([]rune(name)) > 128 {
		return errors.New("话题名称须为1至128个字符")
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return err
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	req := &tg.MessagesEditForumTopicRequest{Peer: inputPeer(ch), TopicID: topicID}
	req.SetTitle(name)
	_, err = api.MessagesEditForumTopic(ctx, req)
	if tgerr.Is(err, "TOPIC_NOT_MODIFIED") {
		return nil
	}
	return c.safeError(err)
}
func (c *Client) CloseTopic(ctx context.Context, chatID int64, topicID int, closed bool) error {
	if topicID <= 1 {
		return errors.New("系统话题不能关闭")
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return err
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	req := &tg.MessagesEditForumTopicRequest{Peer: inputPeer(ch), TopicID: topicID}
	req.SetClosed(closed)
	_, err = api.MessagesEditForumTopic(ctx, req)
	if tgerr.Is(err, "TOPIC_NOT_MODIFIED") {
		return nil
	}
	return c.safeError(err)
}
func (c *Client) DeleteTopic(ctx context.Context, chatID int64, topicID int) error {
	if topicID <= 1 {
		return errors.New("系统话题不能删除")
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return err
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	for {
		res, err := api.MessagesDeleteTopicHistory(ctx, &tg.MessagesDeleteTopicHistoryRequest{Peer: inputPeer(ch), TopMsgID: topicID})
		if tgerr.Is(err, "TOPIC_DELETED") {
			return nil
		}
		if err != nil {
			return c.safeError(err)
		}
		if res.Offset == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (c *Client) Upload(ctx context.Context, chatID int64, topicID int, name, mime string, data, thumb []byte, caption string, random int64) (Reference, error) {
	return c.upload(ctx, chatID, topicID, name, mime, data, thumb, caption, random, nil)
}

func (c *Client) upload(ctx context.Context, chatID int64, topicID int, name, mime string, data, thumb []byte, caption string, random int64, video *VideoMetadata) (Reference, error) {
	return c.UploadStream(ctx, chatID, topicID, name, mime, bytes.NewReader(data), int64(len(data)), thumb, caption, random, video)
}

func (c *Client) sentReference(ctx context.Context, updates tg.UpdatesClass, chatID int64, topicID int, random, account int64) (Reference, error) {
	id := 0
	if short, ok := updates.(*tg.UpdateShortSentMessage); ok {
		id = short.ID
	}
	for _, u := range updateItems(updates) {
		if mapping, ok := u.(*tg.UpdateMessageID); ok && mapping.RandomID == random {
			id = mapping.ID
		}
	}
	var candidate *Reference
	for _, v := range updatesMessages(updates) {
		m, ok := c.normalize(v, account)
		if !ok || m.Reference.ChatID != chatID || m.Reference.DocumentID == 0 || id > 0 && m.Reference.MessageID != id {
			continue
		}
		if m.Reference.TopicID != topicID {
			return Reference{}, errors.New("Telegram 返回的文件不在目标话题")
		}
		if candidate != nil {
			return Reference{}, errors.New("Telegram 返回了不明确的文件定位")
		}
		r := m.Reference
		candidate = &r
	}
	if candidate != nil {
		return *candidate, nil
	}
	if id > 0 {
		m, err := c.sourceMessage(ctx, Reference{AccountID: account, ChatID: chatID, TopicID: topicID, MessageID: id})
		if err != nil {
			return Reference{}, err
		}
		if m.Reference.DocumentID != 0 && m.Reference.TopicID == topicID {
			return m.Reference, nil
		}
	}
	return Reference{}, errors.New("Telegram 未返回上传文件定位，重试时请保留原操作编号")
}

func (c *Client) checkReference(ref Reference) error {
	_, account, err := c.ready()
	if err != nil {
		return err
	}
	if ref.AccountID == 0 || ref.AccountID != account {
		return ErrAccountMismatch
	}
	if ref.ChatID <= 0 || ref.MessageID <= 0 {
		return errors.New("Telegram 归档定位无效")
	}
	return nil
}

func (c *Client) sourceMessage(ctx context.Context, ref Reference) (Message, error) {
	if err := c.checkReference(ref); err != nil {
		return Message{}, err
	}
	ch, err := c.channel(ctx, ref.ChatID)
	if err != nil {
		return Message{}, err
	}
	api, account, err := c.ready()
	if err != nil {
		return Message{}, err
	}
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: inputChannel(ch), ID: []tg.InputMessageClass{&tg.InputMessageID{ID: ref.MessageID}}})
	if err != nil {
		return Message{}, c.safeError(err)
	}
	full, ok := res.AsModified()
	if !ok {
		return Message{}, ErrNotFound
	}
	c.remember(full.GetChats())
	for _, raw := range full.GetMessages() {
		if m, ok := c.normalize(raw, account); ok && m.Reference.ChatID == ref.ChatID && m.Reference.MessageID == ref.MessageID {
			if ref.DocumentID != 0 && m.Reference.DocumentID != ref.DocumentID {
				return Message{}, errors.New("Telegram 原消息的文件已更改")
			}
			return m, nil
		}
	}
	return Message{}, ErrNotFound
}

func (c *Client) sourceRaw(ctx context.Context, ref Reference) (tg.MessageClass, error) {
	if err := c.checkReference(ref); err != nil {
		return nil, err
	}
	ch, err := c.channel(ctx, ref.ChatID)
	if err != nil {
		return nil, err
	}
	api, _, err := c.ready()
	if err != nil {
		return nil, err
	}
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: inputChannel(ch), ID: []tg.InputMessageClass{&tg.InputMessageID{ID: ref.MessageID}}})
	if err != nil {
		return nil, c.safeError(err)
	}
	full, ok := res.AsModified()
	if !ok {
		return nil, ErrNotFound
	}
	c.remember(full.GetChats())
	for _, raw := range full.GetMessages() {
		if m, ok := raw.(*tg.Message); ok && m.ID == ref.MessageID && peerID(m.PeerID) == ref.ChatID {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

// Download refreshes location from the durable source message on every call;
// cached access hashes/file references are never treated as permanent locators.
func (c *Client) Download(ctx context.Context, ref Reference) ([]byte, error) {
	stream, err := c.OpenStream(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	return data, err
}

type sizeWriter struct {
	io.Writer
	remaining int64
}

func (w *sizeWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errStreamLength
	}
	n, err := w.Writer.Write(b)
	w.remaining -= int64(n)
	return n, err
}

func (c *Client) Copy(ctx context.Context, ref Reference, chatID int64, topicID int, random int64) (Reference, error) {
	if err := c.checkReference(ref); err != nil {
		return Reference{}, err
	}
	if random == 0 || topicID <= 0 {
		return Reference{}, errors.New("Telegram 复制参数或操作编号无效")
	}
	original, err := c.sourceMessage(ctx, ref)
	if err != nil {
		return Reference{}, err
	}
	if ref.Size > 0 && original.Reference.Size != ref.Size {
		return Reference{}, errors.New("Telegram 原文件大小与归档记录不符")
	}
	source, err := c.channel(ctx, ref.ChatID)
	if err != nil {
		return Reference{}, err
	}
	target, err := c.channel(ctx, chatID)
	if err != nil {
		return Reference{}, err
	}
	api, account, err := c.ready()
	if err != nil {
		return Reference{}, err
	}
	operation, managed := parsePimgCaption(original.Caption)
	if managed {
		message, found, err := c.findPimgMessage(ctx, target, topicID, operation, original.Reference.Size, original.Reference.DocumentID, original.Caption)
		if err != nil {
			return Reference{}, err
		}
		if found {
			return c.recoveredReference(message), nil
		}
	}
	req := &tg.MessagesForwardMessagesRequest{FromPeer: inputPeer(source), ToPeer: inputPeer(target), ID: []int{ref.MessageID}, RandomID: []int64{random}, DropAuthor: true, Silent: true}
	if topicID != 1 {
		req.SetTopMsgID(topicID)
	}
	updates, err := api.MessagesForwardMessages(ctx, req)
	if err != nil {
		if managed && ctx.Err() == nil {
			message, found, recoveryErr := c.findPimgMessage(ctx, target, topicID, operation, original.Reference.Size, original.Reference.DocumentID, original.Caption)
			if recoveryErr != nil {
				return Reference{}, recoveryErr
			}
			if found {
				return c.recoveredReference(message), nil
			}
		}
		return Reference{}, c.safeError(err)
	}
	result, err := c.sentReference(ctx, updates, chatID, topicID, random, account)
	if err != nil && managed && ctx.Err() == nil {
		message, found, recoveryErr := c.findPimgMessage(ctx, target, topicID, operation, original.Reference.Size, original.Reference.DocumentID, original.Caption)
		if recoveryErr != nil {
			return Reference{}, recoveryErr
		}
		if found {
			return c.recoveredReference(message), nil
		}
	}
	if err == nil && (result.DocumentID != original.Reference.DocumentID || result.Size != original.Reference.Size) {
		return Reference{}, errors.New("Telegram 复制文件标识或大小与原件不符")
	}
	if err == nil {
		_ = c.handleUpdates(ctx, updates)
	}
	return result, err
}

func (c *Client) Delete(ctx context.Context, ref Reference) error {
	if err := c.checkReference(ref); err != nil {
		return err
	}
	if ref.DocumentID != 0 {
		if _, err := c.sourceMessage(ctx, ref); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
	}
	ch, err := c.channel(ctx, ref.ChatID)
	if err != nil {
		return err
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	_, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: inputChannel(ch), ID: []int{ref.MessageID}})
	return c.safeError(err)
}

// History returns every message type, including text/services, so callers can
// detect unmanaged contents before deleting a topic. Date bounds are inclusive.
func (c *Client) History(ctx context.Context, chatID int64, topicID int, from, to time.Time, offset, limit int) (Page, error) {
	if offset < 0 || topicID < 0 || !from.IsZero() && !to.IsZero() && from.After(to) {
		return Page{}, errors.New("Telegram 扫描范围无效")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return Page{}, err
	}
	api, account, err := c.ready()
	if err != nil {
		return Page{}, err
	}
	date := 0
	if offset == 0 && !to.IsZero() {
		date = int(to.Unix() + 1)
	}
	var res tg.MessagesMessagesClass
	if topicID > 1 {
		res, err = api.MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{Peer: inputPeer(ch), MsgID: topicID, OffsetID: offset, OffsetDate: date, Limit: limit})
	} else {
		res, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: inputPeer(ch), OffsetID: offset, OffsetDate: date, Limit: limit})
	}
	if err != nil {
		return Page{}, c.safeError(err)
	}
	full, ok := res.AsModified()
	if !ok {
		return Page{}, errors.New("Telegram 历史消息响应无效")
	}
	c.remember(full.GetChats())
	page := Page{Messages: []Message{}}
	raw := full.GetMessages()
	page.Done = len(raw) == 0
	for _, item := range raw {
		id := item.GetID()
		if id > 0 && (page.NextOffset == 0 || id < page.NextOffset) {
			page.NextOffset = id
		}
		m, ok := c.normalize(item, account)
		if !ok {
			continue
		}
		if topicID > 0 && m.Reference.TopicID != topicID {
			continue
		}
		if !from.IsZero() && m.Date < from.Unix() {
			page.Done = true
			continue
		}
		if !to.IsZero() && m.Date > to.Unix() {
			continue
		}
		page.Messages = append(page.Messages, m)
	}
	if page.NextOffset == 0 || offset > 0 && page.NextOffset >= offset {
		page.Done = true
	}
	return page, nil
}
