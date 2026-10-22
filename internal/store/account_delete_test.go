package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

func TestAccountDeleteCleansRecordedArchivesAndRetainsFailuresForRetry(t *testing.T) {
	s, transport := workflowEnv(t)
	s.UserTG = &provisionTG{workflowTG: transport, groups: map[string]tguser.Group{}}
	owner, other := "user:delete-target", "user:delete-other"
	files := map[string]*dbx.File{}
	for _, account := range []string{owner, other} {
		if err := s.DB.CreateAccount(account, account, "hash"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnsureTelegramSpace(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		file, err := s.receiveLegacyUploadFixture(dbx.WithOwner(t.Context(), account), bytes.NewReader(workflowPNG(t)), "test.png", "png", sql.NullInt64{}, nil, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RunArchive(t.Context(), file.ID); err != nil {
			t.Fatal(err)
		}
		files[account] = file
	}
	chunks, err := s.DB.ListChunks(files[owner].ID, "data")
	if err != nil || len(chunks) == 0 {
		t.Fatal(chunks, err)
	}
	ref, err := s.telegramObject(chunks[0].ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	transport.failDelete[ref.MessageID] = 1
	task, err := s.DB.QueueAccountDelete(owner)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.DB.QueueAccountDelete(owner)
	if err != nil || duplicate != task {
		t.Fatal("duplicate deletion task", duplicate, err)
	}
	if err = s.RunAccountDelete(WithTaskID(t.Context(), task), owner); err == nil || errors.Is(err, ErrAccountDeleteWaiting) {
		t.Fatal("failed remote delete reported success", err)
	}
	if _, err = s.DB.GetFile(files[owner].ID); err != nil || s.DB.AccountActive(owner) {
		t.Fatal("lost pending cleanup record or active account", err)
	}
	active := false
	if err = s.DB.UpdateManagedAccount(owner, nil, &active, ""); err == nil {
		t.Fatal("deleting account can be modified")
	}
	// Clear process-local locks as after restart; remote deletion progress remains
	// in the file/chunk records, and the owner is not removed before cleanup.
	s.locks = fileLocks{}
	for range 3 {
		err = s.RunAccountDelete(WithTaskID(context.Background(), task), owner)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrAccountDeleteWaiting) {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.GetFile(files[owner].ID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("file index survived deletion", err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id=?`, owner).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err = s.DB.GetFile(files[other].ID); err != nil || !s.DB.AccountActive(other) {
		t.Fatal("another account was changed", err)
	}
	if _, err = s.DB.TelegramSpace(other); err != nil {
		t.Fatal("another TG binding was removed", err)
	}
	if _, err = s.DB.InsertOwnerAlbum(owner, "late album"); err == nil {
		t.Fatal("late intake recreated a deleted account's album")
	}
}
