package tguser

import (
	"context"
	"errors"
	"slices"

	"github.com/gotd/td/tg"
)

const ArchiveFolderTitle = "相册"

type ArchiveFolderResult struct {
	FolderID int  `json:"folder_id"`
	Groups   int  `json:"groups"`
	Added    int  `json:"added"`
	Created  bool `json:"created"`
}

// ArchiveFolderOrganizer changes only the explicit membership of Pimg groups.
// It never exports a folder invite or changes any group's visibility.
type ArchiveFolderOrganizer interface {
	SyncArchiveFolder(context.Context, []int64) (ArchiveFolderResult, error)
}

func archiveFilter(filters []tg.DialogFilterClass) (tg.DialogFilterClass, int, error) {
	var found tg.DialogFilterClass
	used := map[int]bool{0: true, 1: true}
	for _, filter := range filters {
		var id int
		var title string
		switch f := filter.(type) {
		case *tg.DialogFilter:
			id, title = f.ID, f.Title.Text
		case *tg.DialogFilterChatlist:
			id, title = f.ID, f.Title.Text
		default:
			continue
		}
		used[id] = true
		if title == ArchiveFolderTitle {
			if found != nil {
				return nil, 0, errors.New("TG 存在多个同名“相册”文件夹，需明确保留哪一个")
			}
			found = filter
		}
	}
	if found != nil {
		switch f := found.(type) {
		case *tg.DialogFilter:
			return found, f.ID, nil
		case *tg.DialogFilterChatlist:
			return found, f.ID, nil
		}
	}
	for id := 2; ; id++ {
		if !used[id] {
			return nil, id, nil
		}
	}
}

func hasFolderChannel(peers []tg.InputPeerClass, id int64) bool {
	return slices.ContainsFunc(peers, func(peer tg.InputPeerClass) bool {
		p, ok := peer.(*tg.InputPeerChannel)
		return ok && p.ChannelID == id
	})
}

func filterPeers(filter tg.DialogFilterClass) (pinned, included, excluded []tg.InputPeerClass) {
	switch f := filter.(type) {
	case *tg.DialogFilter:
		return f.PinnedPeers, f.IncludePeers, f.ExcludePeers
	case *tg.DialogFilterChatlist:
		return f.PinnedPeers, f.IncludePeers, nil
	}
	return nil, nil, nil
}

// SyncArchiveFolder reuses the exact named folder, preserving its flags, title
// entities, icon, color, pinned order and unrelated members/exclusions. A retry
// always re-reads Telegram, including after a response was lost after commit.
func (c *Client) SyncArchiveFolder(ctx context.Context, chatIDs []int64) (ArchiveFolderResult, error) {
	c.folderMu.Lock()
	defer c.folderMu.Unlock()
	ctx = context.WithValue(ctx, durableProvisionContextKey{}, true)
	var result ArchiveFolderResult
	if len(chatIDs) == 0 {
		return result, nil
	}
	api, _, err := c.ready()
	if err != nil {
		return result, err
	}
	peers := make([]tg.InputPeerClass, 0, len(chatIDs))
	for _, id := range chatIDs {
		if hasFolderChannel(peers, id) {
			continue
		}
		ch, err := c.channel(ctx, id)
		if err != nil {
			return result, err
		}
		peers = append(peers, inputPeer(ch))
	}
	result.Groups = len(peers)
	current, err := api.MessagesGetDialogFilters(ctx)
	if err != nil {
		return result, c.safeError(err)
	}
	filter, id, err := archiveFilter(current.Filters)
	if err != nil {
		return result, err
	}
	result.FolderID, result.Created = id, filter == nil
	var merged tg.DialogFilterClass
	var include, exclude *[]tg.InputPeerClass
	var pinned []tg.InputPeerClass
	switch f := filter.(type) {
	case nil:
		next := &tg.DialogFilter{ID: id, Title: tg.TextWithEntities{Text: ArchiveFolderTitle}}
		merged, include, exclude = next, &next.IncludePeers, &next.ExcludePeers
	case *tg.DialogFilter:
		next := *f
		next.IncludePeers, next.ExcludePeers = slices.Clone(f.IncludePeers), slices.Clone(f.ExcludePeers)
		merged, include, exclude, pinned = &next, &next.IncludePeers, &next.ExcludePeers, next.PinnedPeers
	case *tg.DialogFilterChatlist:
		next := *f
		next.IncludePeers = slices.Clone(f.IncludePeers)
		merged, include, pinned = &next, &next.IncludePeers, next.PinnedPeers
	}
	changed := result.Created
	for _, peer := range peers {
		chat := peer.(*tg.InputPeerChannel).ChannelID
		if exclude != nil {
			before := len(*exclude)
			*exclude = slices.DeleteFunc(*exclude, func(p tg.InputPeerClass) bool { return hasFolderChannel([]tg.InputPeerClass{p}, chat) })
			changed = changed || before != len(*exclude)
		}
		if !hasFolderChannel(*include, chat) && !hasFolderChannel(pinned, chat) {
			*include = append(*include, peer)
			result.Added++
			changed = true
		}
	}
	if !changed {
		return result, nil // The initial read is also the membership verification.
	}
	req := &tg.MessagesUpdateDialogFilterRequest{ID: id}
	req.SetFilter(merged)
	ok, err := api.MessagesUpdateDialogFilter(ctx, req)
	if err != nil {
		return result, c.safeError(err)
	}
	if !ok {
		return result, errors.New("TG 未确认“相册”文件夹更新")
	}
	check, err := api.MessagesGetDialogFilters(ctx)
	if err != nil {
		return result, c.safeError(err)
	}
	saved, savedID, err := archiveFilter(check.Filters)
	if err != nil {
		return result, err
	}
	if saved == nil || savedID != id {
		return result, errors.New("TG “相册”文件夹更新结果待核对")
	}
	pins, includes, excludes := filterPeers(saved)
	for _, peer := range peers {
		chat := peer.(*tg.InputPeerChannel).ChannelID
		if hasFolderChannel(excludes, chat) || !hasFolderChannel(includes, chat) && !hasFolderChannel(pins, chat) {
			return result, errors.New("TG “相册”文件夹尚未包含全部归档群")
		}
	}
	return result, nil
}
