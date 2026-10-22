package tguser

import (
	"context"
	"errors"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// GroupProvisioner is optional so injected transports remain compatible.
type GroupProvisioner interface {
	FindManagedGroup(context.Context, string) (Group, bool, error)
	CreateManagedGroup(context.Context, string, string) (Group, error)
}

func (c *Client) FindManagedGroup(ctx context.Context, marker string) (Group, bool, error) {
	ctx = context.WithValue(ctx, durableProvisionContextKey{}, true)
	groups, err := c.Groups(ctx)
	if err != nil {
		return Group{}, false, err
	}
	api, _, err := c.ready()
	if err != nil {
		return Group{}, false, err
	}
	var found Group
	for _, g := range groups {
		// Names are editable presentation. Recovery verifies the immutable
		// provisioning marker even if the group was renamed in Telegram.
		ch, err := c.channel(ctx, g.ID)
		if err != nil {
			return Group{}, false, err
		}
		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(ch))
		if err != nil {
			return Group{}, false, c.safeError(err)
		}
		data, ok := full.FullChat.(*tg.ChannelFull)
		if !ok {
			return Group{}, false, errors.New("TG 群详情响应无效")
		}
		if data.About == marker {
			if found.ID != 0 {
				return Group{}, false, errors.New("检测到重复的归档群身份，需管理员处理")
			}
			found = g
		}
	}
	// Recovery completes the same post-create configuration. If the create
	// succeeded but its reply or the tabs update was lost, never create again.
	if found.ID > 0 {
		if err := c.managedGroupForumTabs(ctx, found.ID); err != nil {
			return found, true, err
		}
	}
	return found, found.ID > 0, nil
}

func (c *Client) CreateManagedGroup(ctx context.Context, title, marker string) (Group, error) {
	ctx = context.WithValue(ctx, durableProvisionContextKey{}, true)
	api, _, err := c.ready()
	if err != nil {
		return Group{}, err
	}
	req := &tg.ChannelsCreateChannelRequest{Title: title, About: marker}
	req.SetMegagroup(true)
	req.SetForum(true)
	req.SetTTLPeriod(0)
	result, err := api.ChannelsCreateChannel(ctx, req)
	if err != nil {
		if wait, ok := tgerr.AsFloodWait(err); ok {
			return Group{}, &RetryAfterError{Wait: wait}
		}
		return Group{}, c.safeError(err)
	}
	// Only accept the group returned by this operation, never a same-name group.
	updates, ok := result.(*tg.Updates)
	if !ok {
		return Group{}, errors.New("TG 建群响应待核对")
	}
	c.remember(updates.Chats)
	var group Group
	for _, v := range updates.Chats {
		if ch, ok := v.(*tg.Channel); ok && ch.Creator && manageable(ch) {
			if group.ID != 0 {
				return Group{}, errors.New("TG 建群响应包含多个群")
			}
			group = Group{ID: ch.ID, Name: ch.Title}
		}
	}
	if group.ID == 0 {
		return Group{}, errors.New("TG 建群响应缺少论坛超级群")
	}
	if err := c.managedGroupForumTabs(ctx, group.ID); err != nil {
		return group, err
	}
	return group, nil
}

// createChannel has no tabs option. Set the forum layout on the exact newly
// created (or marker-recovered) group before reporting provisioning complete.
func (c *Client) managedGroupForumTabs(ctx context.Context, chatID int64) error {
	ch, err := c.channel(ctx, chatID)
	if err != nil {
		return err
	}
	if ch.ForumTabs {
		return nil
	}
	api, _, err := c.ready()
	if err != nil {
		return err
	}
	result, err := api.ChannelsToggleForum(ctx, &tg.ChannelsToggleForumRequest{
		Channel: inputChannel(ch), Enabled: true, Tabs: true,
	})
	if tgerr.Is(err, "CHAT_NOT_MODIFIED") {
		return nil
	}
	if err != nil {
		return c.safeError(err)
	}
	if updates, ok := result.(*tg.Updates); ok {
		c.remember(updates.Chats)
	}
	return nil
}
