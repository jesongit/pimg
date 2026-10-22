package tguser

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestManagedGroupRecoverySurvivesArbitraryRename(t *testing.T) {
	marker := "pimg-managed-group:v1:secret-token"
	channel := &tg.Channel{ID: 41, AccessHash: 412, Title: "家庭原片备份", Creator: true, Megagroup: true, Forum: true}
	toggles := 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch in.(type) {
		case *tg.MessagesGetDialogsRequest:
			out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{Chats: []tg.ChatClass{channel}}
		case *tg.ChannelsGetFullChannelRequest:
			full := out.(*tg.MessagesChatFull)
			full.FullChat = &tg.ChannelFull{About: marker}
			full.Chats = []tg.ChatClass{channel}
		case *tg.ChannelsToggleForumRequest:
			toggles++
			out.(*tg.UpdatesBox).Updates = &tg.Updates{}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	group, found, err := c.FindManagedGroup(context.Background(), marker)
	if err != nil || !found || group.ID != 41 || group.Name != channel.Title || toggles != 1 {
		t.Fatal(group, found, err)
	}
}

func TestGroupRenameTargetsStableIDAndVerifiesRemoteResult(t *testing.T) {
	var renamed string
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.ChannelsEditTitleRequest:
			if req.Channel.(*tg.InputChannel).ChannelID != 41 {
				t.Fatal("wrong group")
			}
			renamed = req.Title
			out.(*tg.UpdatesBox).Updates = &tg.Updates{}
		case *tg.ChannelsGetFullChannelRequest:
			out.(*tg.MessagesChatFull).Chats = []tg.ChatClass{&tg.Channel{ID: 41, AccessHash: 412, Title: renamed, Creator: true, Forum: true, Megagroup: true}}
		default:
			t.Fatalf("unexpected RPC %T", req)
		}
		return nil
	})
	if err := c.RenameGroup(context.Background(), 41, "随意修改的群名"); err != nil || renamed != "随意修改的群名" {
		t.Fatal(renamed, err)
	}
	for _, title := range []string{" ", "bad\nname", strings.Repeat("字", 129)} {
		if err := c.RenameGroup(context.Background(), 41, title); err == nil {
			t.Fatal("invalid title accepted")
		}
	}
}
