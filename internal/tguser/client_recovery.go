package tguser

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/gotd/td/tg"
)

// The caption is a durable operation identity after a lost sendMedia response;
// Telegram random_id deduplication alone need not return the original message.
type pimgCaption struct {
	App    string `json:"app"`
	V      int    `json:"v"`
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Index  int    `json:"idx"`
	SHA256 string `json:"sha256"`
	Op     string `json:"op"`
}

func parsePimgCaption(caption string) (pimgCaption, bool) {
	var parsed struct {
		pimgCaption
		Index *int `json:"idx"`
	}
	if json.Unmarshal([]byte(caption), &parsed) != nil || parsed.App != "pimg" || parsed.V != 1 ||
		parsed.File == "" || parsed.Kind == "" || parsed.Index == nil || *parsed.Index < 0 {
		return pimgCaption{}, false
	}
	if sum, err := hex.DecodeString(parsed.SHA256); err != nil || len(sum) != 32 {
		return pimgCaption{}, false
	}
	parsed.pimgCaption.Index = *parsed.Index
	return parsed.pimgCaption, true
}

func uploadOperation(caption string, random int64) (pimgCaption, bool, error) {
	parsed, managed := parsePimgCaption(caption)
	if !managed || parsed.Op == "" {
		return pimgCaption{}, false, nil
	}
	if parsed.Op != strconv.FormatInt(random, 16) {
		return pimgCaption{}, false, errors.New("Telegram 归档操作标识与持久化编号不符")
	}
	return parsed, true, nil
}

func consumeRecoveredUpload(reader *exactStreamReader) error {
	_, err := io.Copy(io.Discard, reader)
	if ctxErr := reader.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if reader.lengthError || reader.remaining != 0 || !reader.complete {
		return errStreamLength
	}
	return err
}

// Search is deliberately scoped to a channel/thread and each returned message
// is independently checked: search text and thread filtering are not proof of
// identity. A pagination failure or ambiguous match must not authorize a resend.
func (c *Client) findPimgMessage(ctx context.Context, ch *tg.Channel, topicID int, expected pimgCaption, size, documentID int64, exactCaption string) (Message, bool, error) {
	api, account, err := c.ready()
	if err != nil {
		return Message{}, false, err
	}
	query := expected.Op
	if query == "" {
		query = expected.File
	}
	req := &tg.MessagesSearchRequest{Peer: inputPeer(ch), Q: query, Filter: &tg.InputMessagesFilterEmpty{}, Limit: 100}
	if topicID > 1 {
		req.SetTopMsgID(topicID)
	}
	var candidate *Message
	seen := make(map[int]bool)
	for {
		res, err := api.MessagesSearch(ctx, req)
		if err != nil {
			return Message{}, false, c.safeError(err)
		}
		full, ok := res.AsModified()
		if !ok {
			return Message{}, false, errors.New("Telegram 归档恢复查询响应无效")
		}
		c.remember(full.GetChats())
		raw := full.GetMessages()
		next := 0
		for _, item := range raw {
			id := item.GetID()
			if id > 0 && (next == 0 || id < next) {
				next = id
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			message, ok := item.(*tg.Message)
			if !ok {
				continue
			}
			media, ok := message.Media.(*tg.MessageMediaDocument)
			if !ok {
				continue
			}
			if _, ok := media.Document.(*tg.Document); !ok {
				continue
			}
			m, ok := c.normalize(item, account)
			if !ok || m.Reference.ChatID != ch.ID || m.Reference.TopicID != topicID ||
				m.Reference.MessageID <= 0 || m.Reference.DocumentID == 0 || m.Reference.Size != size ||
				documentID != 0 && m.Reference.DocumentID != documentID {
				continue
			}
			identity, managed := parsePimgCaption(m.Caption)
			if !managed || identity != expected || exactCaption != "" && m.Caption != exactCaption {
				continue
			}
			if candidate != nil {
				return Message{}, false, errors.New("Telegram 归档恢复找到多个相同操作，请保留现有定位后核对")
			}
			candidate = &m
		}
		_, complete := res.(*tg.MessagesMessages)
		if len(raw) == 0 || complete {
			if candidate != nil {
				return *candidate, true, nil
			}
			return Message{}, false, nil
		}
		if next == 0 || req.OffsetID > 0 && next >= req.OffsetID {
			return Message{}, false, errors.New("Telegram 归档恢复查询分页没有前进")
		}
		req.OffsetID = next
	}
}

func (c *Client) recoveredReference(message Message) Reference {
	if c.opts.OnMessage != nil {
		c.opts.OnMessage(message)
	}
	return message.Reference
}
