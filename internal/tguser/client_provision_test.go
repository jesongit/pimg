package tguser

import (
	"context"
	"errors"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"testing"
	"time"
)

func TestManagedGroupCreationUsesForumTabsAndDisablesMessageExpiry(t *testing.T) {
	var calls int
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		calls++
		switch request := in.(type) {
		case *tg.ChannelsCreateChannelRequest:
			ttl, hasTTL := request.GetTTLPeriod()
			if calls != 1 || !request.Megagroup || !request.Forum || !hasTTL || ttl != 0 || request.About != "marker" {
				t.Fatal("wrong archive group flags", request)
			}
			out.(*tg.UpdatesBox).Updates = &tg.Updates{Chats: []tg.ChatClass{&tg.Channel{ID: 123, AccessHash: 456, Title: request.Title, Creator: true, Megagroup: true, Forum: true}}}
		case *tg.ChannelsToggleForumRequest:
			channel, ok := request.Channel.(*tg.InputChannel)
			if calls != 2 || !ok || channel.ChannelID != 123 || channel.AccessHash != 456 || !request.Enabled || !request.Tabs {
				t.Fatal("wrong forum layout or target", request)
			}
			out.(*tg.UpdatesBox).Updates = &tg.Updates{}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	group, err := c.CreateManagedGroup(context.Background(), "家庭相册", "marker")
	if err != nil || group.ID != 123 || group.Name != "家庭相册" || calls != 2 {
		t.Fatal(group, err)
	}
}

func TestManagedGroupTabsFailureRecoversSameGroupWithoutRecreation(t *testing.T) {
	for _, failure := range []error{tgerr.New(420, "FLOOD_WAIT_7200"), errors.New("lost tabs response")} {
		t.Run(failure.Error(), func(t *testing.T) {
			creates, toggles := 0, 0
			channel := &tg.Channel{ID: 123, AccessHash: 456, Title: "家庭相册", Creator: true, Megagroup: true, Forum: true}
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch request := in.(type) {
				case *tg.ChannelsCreateChannelRequest:
					creates++
					out.(*tg.UpdatesBox).Updates = &tg.Updates{Chats: []tg.ChatClass{channel}}
				case *tg.ChannelsToggleForumRequest:
					if request.Channel.(*tg.InputChannel).ChannelID != channel.ID || !request.Enabled || !request.Tabs {
						t.Fatal("recovery targeted another group or layout", request)
					}
					toggles++
					if toggles == 1 {
						return failure
					}
					// The first update may have committed despite its lost reply.
					return tgerr.New(400, "CHAT_NOT_MODIFIED")
				case *tg.MessagesGetDialogsRequest:
					out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{Chats: []tg.ChatClass{channel}}
				case *tg.ChannelsGetFullChannelRequest:
					out.(*tg.MessagesChatFull).FullChat = &tg.ChannelFull{About: "marker"}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			group, err := c.CreateManagedGroup(context.Background(), channel.Title, "marker")
			if err == nil || group.ID != channel.ID {
				t.Fatal("unfinished setup reported success", group, err)
			}
			if _, isFlood := tgerr.AsFloodWait(failure); isFlood {
				var wait *RetryAfterError
				if !errors.As(err, &wait) || wait.Wait != 2*time.Hour {
					t.Fatal("tabs update lost provider cooldown", err)
				}
			}
			c.channels = map[int64]*tg.Channel{} // Simulate a new client after restart.
			group, found, err := c.FindManagedGroup(context.Background(), "marker")
			if err != nil || !found || group.ID != channel.ID || creates != 1 || toggles != 2 {
				t.Fatal("same-group recovery failed", group, found, err, creates, toggles)
			}
		})
	}
}

func TestManagedGroupRecoveryDoesNotChangeOtherOrAlreadyTabbedGroups(t *testing.T) {
	for _, matching := range []bool{false, true} {
		marker := "another-marker"
		if matching {
			marker = "marker"
		}
		channel := &tg.Channel{ID: 41, AccessHash: 412, Title: "unchanged", Creator: true, Megagroup: true, Forum: true, ForumTabs: matching}
		c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
			switch in.(type) {
			case *tg.MessagesGetDialogsRequest:
				out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{Chats: []tg.ChatClass{channel}}
			case *tg.ChannelsGetFullChannelRequest:
				out.(*tg.MessagesChatFull).FullChat = &tg.ChannelFull{About: marker}
			default:
				t.Fatalf("unnecessary mutation of other or already configured group: %T", in)
			}
			return nil
		})
		_, found, err := c.FindManagedGroup(context.Background(), "marker")
		if err != nil || found != matching {
			t.Fatal(found, err)
		}
	}
}

func TestManagedGroupCreationRetainsRequiredFloodWait(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error { return tgerr.New(420, "FLOOD_WAIT_7200") })
	_, err := c.CreateManagedGroup(context.Background(), "Pimg-token", "marker")
	var wait *RetryAfterError
	if !errors.As(err, &wait) || wait.Wait != 2*time.Hour {
		t.Fatal("TG wait duration lost", err)
	}
}
