package tguser

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestUserGroupDiscoveryPaginatesNormalAndServiceMessages(t *testing.T) {
	for _, service := range []bool{false, true} {
		name := "normal"
		if service {
			name = "service"
		}
		t.Run(name, func(t *testing.T) {
			calls := map[int]int{}
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				req, ok := in.(*tg.MessagesGetDialogsRequest)
				if !ok {
					t.Fatalf("unexpected group discovery RPC %T", in)
				}
				page := calls[req.FolderID]
				calls[req.FolderID]++
				if page > 0 {
					previousID, previousDate := int64(41+page-1), 1200-(page-1)*100
					if req.FolderID == 1 {
						previousID, previousDate = 44, 900
					}
					p, ok := req.OffsetPeer.(*tg.InputPeerChannel)
					if !ok || p.ChannelID != previousID || p.AccessHash != previousID*10 || req.OffsetID != 10 || req.OffsetDate != previousDate || !req.ExcludePinned {
						t.Fatalf("wrong dialog cursor: folder=%d page=%d date=%d id=%d peer=%#v", req.FolderID, page, req.OffsetDate, req.OffsetID, req.OffsetPeer)
					}
				} else if req.OffsetDate != 0 || req.OffsetID != 0 || req.ExcludePinned {
					t.Fatal("folder pagination reused another folder's cursor")
				}
				if page == 2 || req.FolderID == 1 && page == 1 {
					out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{}
					return nil
				}
				id, date := int64(41+page), 1200-page*100
				if req.FolderID == 1 {
					id, date = 44, 900
				}
				peer := &tg.PeerChannel{ChannelID: id}
				var message tg.MessageClass = &tg.Message{ID: 10, Date: date, PeerID: peer}
				if service {
					message = &tg.MessageService{ID: 10, Date: date, PeerID: peer, Action: &tg.MessageActionChannelCreate{Title: "fixture"}}
				}
				out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogsSlice{
					Count:   3,
					Dialogs: []tg.DialogClass{&tg.Dialog{Peer: peer, TopMessage: 10}},
					// Message IDs are only unique inside a peer. Do not use the
					// date of an unrelated dialog that happens to share the ID.
					Messages: []tg.MessageClass{&tg.Message{ID: 10, Date: 9999, PeerID: &tg.PeerChannel{ChannelID: 99}}, message},
					Chats:    []tg.ChatClass{&tg.Channel{ID: id, AccessHash: id * 10, Title: name, Megagroup: true, Forum: true, Creator: true}},
				}
				return nil
			})
			groups, err := c.Groups(context.Background())
			if err != nil || len(groups) != 3 || calls[0] != 3 || calls[1] != 2 {
				t.Fatal("group discovery did not include all pages and archive folder", groups, calls, err)
			}
		})
	}
}
