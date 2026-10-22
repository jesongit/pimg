package store

import (
	"context"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

type folderTG struct {
	*provisionTG
	folderCalls int
	folderErr   error
}

func (f *folderTG) SyncArchiveFolder(_ context.Context, ids []int64) (tguser.ArchiveFolderResult, error) {
	f.folderCalls++
	return tguser.ArchiveFolderResult{Groups: len(ids)}, f.folderErr
}

func TestArchiveFolderFailureKeepsGroupAndPersistsCooldown(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &folderTG{provisionTG: &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}, folderErr: &tguser.RetryAfterError{Wait: 2 * time.Hour}}
	s.UserTG = fake
	owner := "user:folder"
	if err := s.DB.CreateAccount(owner, "folder", "hash"); err != nil {
		t.Fatal(err)
	}
	space, err := s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureTelegramArchiveFolder(context.Background(), space); err == nil {
		t.Fatal("folder failure accepted")
	}
	saved, err := s.DB.TelegramSpace(owner)
	if err != nil || saved.State != "ready" || saved.ChatID != space.ChatID || saved.RetryAfter < time.Now().Add(119*time.Minute).Unix() {
		t.Fatal(saved, err)
	}
	if err = s.EnsureTelegramArchiveFolder(context.Background(), saved); err == nil || fake.folderCalls != 1 {
		t.Fatal("folder cooldown ignored", err)
	}
	// After restart, a ready binding is reused even though its folder step failed.
	s.locks = fileLocks{}
	s.DB.Exec(`UPDATE account_telegram SET retry_after=0 WHERE owner_id=?`, owner)
	fake.folderErr = nil
	space, err = s.EnsureTelegramSpace(context.Background(), owner)
	if err != nil || fake.calls != 1 {
		t.Fatal("folder retry recreated group", err, fake.calls)
	}
	if err = s.EnsureTelegramArchiveFolder(context.Background(), space); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureTelegramArchiveFolder(context.Background(), space); err != nil || fake.folderCalls != 2 {
		t.Fatal("saved folder completion lost", err)
	}
}

func TestArchiveFolderReconcileIncludesExistingAdminGroup(t *testing.T) {
	s, transport := workflowEnv(t)
	fake := &folderTG{provisionTG: &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}}
	s.UserTG = fake
	space, err := s.EnsureTelegramSpace(context.Background(), dbx.AdminOwner)
	if err != nil || space.ChatID != 77 {
		t.Fatal(space, err)
	}
	if err = s.ReconcileAccountSpaces(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type=? AND file_id=?`, TaskAccountSpace, "tg-space:"+dbx.AdminOwner).Scan(&count); err != nil || count != 1 {
		t.Fatal("legacy group omitted from folder task", count, err)
	}
	if err = s.EnsureTelegramArchiveFolder(context.Background(), space); err != nil || fake.calls != 0 {
		t.Fatal("legacy group recreated", err)
	}
}
