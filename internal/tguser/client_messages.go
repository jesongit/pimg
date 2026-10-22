package tguser

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
)

func updateItems(updates tg.UpdatesClass) []tg.UpdateClass {
	switch v := updates.(type) {
	case *tg.Updates:
		return v.Updates
	case *tg.UpdatesCombined:
		return v.Updates
	case *tg.UpdateShort:
		return []tg.UpdateClass{v.Update}
	}
	return nil
}
func updatesMessages(updates tg.UpdatesClass) []tg.MessageClass {
	out := []tg.MessageClass{}
	for _, u := range updateItems(updates) {
		switch v := u.(type) {
		case *tg.UpdateNewChannelMessage:
			out = append(out, v.Message)
		case *tg.UpdateNewMessage:
			out = append(out, v.Message)
		case *tg.UpdateEditChannelMessage:
			out = append(out, v.Message)
		case *tg.UpdateEditMessage:
			out = append(out, v.Message)
		}
	}
	return out
}

func (c *Client) handleUpdates(_ context.Context, updates tg.UpdatesClass) error {
	gaps := map[int64]bool{}
	switch v := updates.(type) {
	case *tg.Updates:
		c.remember(v.Chats)
	case *tg.UpdatesCombined:
		c.remember(v.Chats)
	case *tg.UpdatesTooLong:
		gaps[0] = true
	}
	for _, u := range updateItems(updates) {
		switch update := u.(type) {
		case *tg.UpdateChannelTooLong:
			gaps[update.ChannelID] = true
		case *tg.UpdatePtsChanged:
			gaps[0] = true
		}
	}
	if c.opts.OnGap != nil {
		for chat := range gaps {
			c.opts.OnGap(chat)
		}
	}
	if c.opts.OnMessage == nil {
		return nil
	}
	c.mu.RLock()
	account := c.status.AccountID
	c.mu.RUnlock()
	if account == 0 {
		return nil
	}
	for _, v := range updatesMessages(updates) {
		if m, ok := c.normalize(v, account); ok {
			c.opts.OnMessage(m)
		}
	}
	return nil
}

func topicOf(reply tg.MessageReplyHeaderClass) int {
	if h, ok := reply.(*tg.MessageReplyHeader); ok && h.ForumTopic {
		if h.ReplyToTopID > 0 {
			return h.ReplyToTopID
		}
		if h.ReplyToMsgID > 0 {
			return h.ReplyToMsgID
		}
	}
	return 1
}

func (c *Client) normalize(raw tg.MessageClass, account int64) (Message, bool) {
	var m Message
	m.Reference.AccountID = account
	switch v := raw.(type) {
	case *tg.Message:
		peer, ok := v.PeerID.(*tg.PeerChannel)
		if !ok {
			return m, false
		}
		m.Reference.ChatID = peer.ChannelID
		m.Reference.MessageID = v.ID
		m.Reference.TopicID = topicOf(v.ReplyTo)
		m.Date = int64(v.Date)
		m.Caption = v.Message
		switch media := v.Media.(type) {
		case *tg.MessageMediaDocument:
			if doc, ok := media.Document.(*tg.Document); ok {
				m.Reference.DocumentID = doc.ID
				m.Reference.AccessHash = doc.AccessHash
				m.Reference.FileReference = append([]byte(nil), doc.FileReference...)
				m.Reference.DCID = doc.DCID
				m.Reference.Size = doc.Size
				m.MIME = doc.MimeType
				for _, attr := range doc.Attributes {
					if a, ok := attr.(*tg.DocumentAttributeFilename); ok {
						m.Name = a.FileName
						break
					}
				}
				if m.Name == "" {
					m.Name = fmt.Sprintf("document-%d", doc.ID)
				}
			}
		case *tg.MessageMediaPhoto:
			if p, ok := media.Photo.(*tg.Photo); ok {
				_, size := photoSize(p)
				m.Reference.DocumentID = p.ID
				m.Reference.AccessHash = p.AccessHash
				m.Reference.FileReference = append([]byte(nil), p.FileReference...)
				m.Reference.DCID = p.DCID
				m.Reference.Size = size
				m.MIME = "image/jpeg"
				m.Name = fmt.Sprintf("photo-%d.jpg", p.ID)
			}
		}
		return m, true
	case *tg.MessageService:
		peer, ok := v.PeerID.(*tg.PeerChannel)
		if !ok {
			return m, false
		}
		m.Reference.ChatID = peer.ChannelID
		m.Reference.MessageID = v.ID
		m.Reference.TopicID = topicOf(v.ReplyTo)
		m.Date = int64(v.Date)
		m.IsService = true
		switch action := v.Action.(type) {
		case *tg.MessageActionTopicCreate:
			m.Name = action.Title
			m.Reference.TopicID = v.ID
			m.Caption = "topic_created"
		case *tg.MessageActionTopicEdit:
			m.Name = action.Title
			m.Caption = "topic_edited"
		default:
			m.Caption = "service_message"
		}
		return m, true
	}
	return m, false
}

func photoSize(photo *tg.Photo) (string, int64) {
	kind := ""
	var largest int64
	for _, raw := range photo.Sizes {
		switch s := raw.(type) {
		case *tg.PhotoSize:
			if int64(s.Size) > largest {
				kind = s.Type
				largest = int64(s.Size)
			}
		case *tg.PhotoSizeProgressive:
			for _, n := range s.Sizes {
				if int64(n) > largest {
					kind = s.Type
					largest = int64(n)
				}
			}
		}
	}
	return kind, largest
}

func mediaLocation(raw tg.MessageClass) (tg.InputFileLocationClass, int64, int, error) {
	m, ok := raw.(*tg.Message)
	if !ok {
		return nil, 0, 0, ErrNotFound
	}
	switch media := m.Media.(type) {
	case *tg.MessageMediaDocument:
		if d, ok := media.Document.(*tg.Document); ok {
			return &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}, d.Size, d.DCID, nil
		}
	case *tg.MessageMediaPhoto:
		if p, ok := media.Photo.(*tg.Photo); ok {
			kind, size := photoSize(p)
			if kind == "" || size <= 0 {
				return nil, 0, 0, errors.New("Telegram 图片缺少可下载尺寸")
			}
			return &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: kind}, size, p.DCID, nil
		}
	}
	return nil, 0, 0, ErrNotFound
}
