package tguser

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

type GroupRenamer interface {
	RenameGroup(context.Context, int64, string) error
}

func ValidGroupTitle(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= 128 && !strings.ContainsAny(name, "\r\n\x00")
}

func (c *Client) RenameGroup(ctx context.Context, chat int64, name string) error {
	if !ValidGroupTitle(name) {
		return errors.New("群名须为 1–128 个字符，不能包含换行")
	}
	ctx = context.WithValue(ctx, durableProvisionContextKey{}, true)
	ch, err := c.channel(ctx, chat)
	if err != nil {
		return err
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	if _, err = api.ChannelsEditTitle(ctx, &tg.ChannelsEditTitleRequest{Channel: inputChannel(ch), Title: name}); err != nil {
		return c.safeError(err)
	}
	full, err := api.ChannelsGetFullChannel(ctx, inputChannel(ch))
	if err != nil {
		return c.safeError(err)
	}
	c.remember(full.Chats)
	for _, item := range full.Chats {
		if channel, ok := item.(*tg.Channel); ok && channel.ID == chat && channel.Title == name {
			return nil
		}
	}
	return errors.New("TG 群名更新结果待核对，任务将保留重试")
}
