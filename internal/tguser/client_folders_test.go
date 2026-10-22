package tguser

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type folderFixture struct {
	filters             []tg.DialogFilterClass
	writes              int
	lostReply, noCommit bool
}

func (f *folderFixture) client(t *testing.T) *Client {
	t.Helper()
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.MessagesGetDialogFiltersRequest:
			out.(*tg.MessagesDialogFilters).Filters = f.filters
		case *tg.MessagesUpdateDialogFilterRequest:
			filter, ok := req.GetFilter()
			if !ok {
				t.Fatal("folder delete requested")
			}
			f.writes++
			if !f.noCommit {
				replaced := false
				for i, v := range f.filters {
					switch old := v.(type) {
					case *tg.DialogFilter:
						if old.ID == req.ID {
							f.filters[i], replaced = filter, true
						}
					case *tg.DialogFilterChatlist:
						if old.ID == req.ID {
							f.filters[i], replaced = filter, true
						}
					}
				}
				if !replaced {
					f.filters = append(f.filters, filter)
				}
			}
			if f.lostReply {
				f.lostReply = false
				return errors.New("response lost after commit")
			}
			out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		default:
			t.Fatalf("unexpected RPC %T", req)
		}
		return nil
	})
	c.channels[42] = &tg.Channel{ID: 42, AccessHash: 422, Forum: true, Megagroup: true, Creator: true}
	return c
}

func TestArchiveFolderPreservesExistingConfigurationAndMembers(t *testing.T) {
	original := &tg.DialogFilter{ID: 7, Title: tg.TextWithEntities{Text: ArchiveFolderTitle, Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Length: 2}}}, Groups: true, ExcludeRead: true, ExcludeMuted: true, ExcludeArchived: true, TitleNoanimate: true,
		PinnedPeers:  []tg.InputPeerClass{&tg.InputPeerChannel{ChannelID: 42, AccessHash: 422}},
		IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 41, AccessHash: 99}},
		ExcludePeers: []tg.InputPeerClass{&tg.InputPeerChannel{ChannelID: 41}, &tg.InputPeerChat{ChatID: 41}}}
	original.SetEmoticon("📷")
	original.SetColor(3)
	unrelated := &tg.DialogFilter{ID: 3, Title: tg.TextWithEntities{Text: "工作"}}
	fixture := &folderFixture{filters: []tg.DialogFilterClass{unrelated, original}}
	c := fixture.client(t)
	result, err := c.SyncArchiveFolder(context.Background(), []int64{41, 42, 41})
	if err != nil || result.Groups != 2 || result.Added != 1 || fixture.writes != 1 {
		t.Fatal(result, err, fixture.writes)
	}
	expected := *original
	expected.IncludePeers = append(append([]tg.InputPeerClass{}, original.IncludePeers...), inputPeer(c.channels[41]))
	expected.ExcludePeers = []tg.InputPeerClass{original.ExcludePeers[1]}
	if !reflect.DeepEqual(fixture.filters[1], &expected) || fixture.filters[0] != unrelated {
		t.Fatal("existing folder configuration or unrelated membership changed")
	}
	if len(original.IncludePeers) != 1 || len(original.ExcludePeers) != 2 {
		t.Fatal("mutated fetched snapshot before commit")
	}
	if _, err = c.SyncArchiveFolder(context.Background(), []int64{41, 42}); err != nil || fixture.writes != 1 {
		t.Fatal("repeat was not idempotent", err)
	}
}

func TestArchiveFolderCreatesMissingAndRecoversLostReply(t *testing.T) {
	fixture := &folderFixture{filters: []tg.DialogFilterClass{&tg.DialogFilterDefault{}, &tg.DialogFilter{ID: 2, Title: tg.TextWithEntities{Text: "工作"}}}, lostReply: true}
	c := fixture.client(t)
	if _, err := c.SyncArchiveFolder(context.Background(), []int64{41}); err == nil {
		t.Fatal("lost response accepted")
	}
	result, err := c.SyncArchiveFolder(context.Background(), []int64{41})
	if err != nil || result.FolderID != 3 || result.Created || fixture.writes != 1 {
		t.Fatal(result, err, fixture.writes)
	}
}

func TestArchiveFolderConcurrentAccountsMergeWithoutLostMembers(t *testing.T) {
	fixture := &folderFixture{}
	c := fixture.client(t)
	var wg sync.WaitGroup
	for _, id := range []int64{41, 42} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, err := c.SyncArchiveFolder(context.Background(), []int64{id}); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if len(fixture.filters) != 1 {
		t.Fatal("duplicate folders", len(fixture.filters))
	}
	_, includes, _ := filterPeers(fixture.filters[0])
	if !hasFolderChannel(includes, 41) || !hasFolderChannel(includes, 42) {
		t.Fatal("lost account membership")
	}
}

func TestArchiveFolderPreservesSharedFolderAndNeverExportsInvites(t *testing.T) {
	original := &tg.DialogFilterChatlist{ID: 6, Title: tg.TextWithEntities{Text: ArchiveFolderTitle}, HasMyInvites: true, TitleNoanimate: true, IncludePeers: []tg.InputPeerClass{&tg.InputPeerChat{ChatID: 9}}}
	original.SetColor(2)
	fixture := &folderFixture{filters: []tg.DialogFilterClass{original}}
	c := fixture.client(t)
	if _, err := c.SyncArchiveFolder(context.Background(), []int64{41}); err != nil {
		t.Fatal(err)
	}
	saved, ok := fixture.filters[0].(*tg.DialogFilterChatlist)
	if !ok || !saved.HasMyInvites || !saved.TitleNoanimate || len(saved.IncludePeers) != 2 || saved.Color != 2 {
		t.Fatal("shared folder properties changed")
	}
}

func TestArchiveFolderRejectsDuplicateNamesAndUnconfirmedMembership(t *testing.T) {
	t.Run("duplicate names", func(t *testing.T) {
		fixture := &folderFixture{filters: []tg.DialogFilterClass{&tg.DialogFilter{ID: 2, Title: tg.TextWithEntities{Text: ArchiveFolderTitle}}, &tg.DialogFilter{ID: 3, Title: tg.TextWithEntities{Text: ArchiveFolderTitle}}}}
		if _, err := fixture.client(t).SyncArchiveFolder(context.Background(), []int64{41}); err == nil || fixture.writes != 0 {
			t.Fatal("ambiguous folder changed")
		}
	})
	t.Run("missing readback", func(t *testing.T) {
		fixture := &folderFixture{noCommit: true}
		if _, err := fixture.client(t).SyncArchiveFolder(context.Background(), []int64{41}); err == nil {
			t.Fatal("unconfirmed update accepted")
		}
	})
}

func TestArchiveFolderRetainsProviderFloodWait(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error { return tgerr.New(420, "FLOOD_WAIT_7200") })
	_, err := c.SyncArchiveFolder(context.Background(), []int64{41})
	var wait *RetryAfterError
	if !errors.As(err, &wait) || wait.Wait != 2*time.Hour {
		t.Fatal("provider wait lost", err)
	}
}
