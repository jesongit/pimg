package dbx

import (
	"errors"
	"fmt"
	"testing"
)

func ownerTransferFixture(t *testing.T) (*DB, *File, int64) {
	t.Helper()
	d := telegramTestDB(t)
	if err := d.CreateAccount("user:destination", "Destination", "hash"); err != nil {
		t.Fatal(err)
	}
	album, err := d.InsertOwnerAlbum("user:destination", "Target")
	if err != nil {
		t.Fatal(err)
	}
	f := operationFixtureFile(t, d, "transfer-source")
	return d, f, album
}

func TestOwnerTransferGuardsAndOperationReplay(t *testing.T) {
	d, f, album := ownerTransferFixture(t)
	op, err := d.BeginTaskOperation(AdminOwner, "move_album", "transfer-one", "album:"+fmt.Sprint(album), 1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginOwnerTransfer(f.ID, "target", album, op.ID, []byte{3}, []byte{4}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.BeginOwnerTransfer(f.ID, "unused", album, op.ID, []byte{5}, []byte{6}, 1); err != nil {
		t.Fatal("same operation replay failed", err)
	}
	var count int
	if err = d.QueryRow(`SELECT COUNT(*) FROM tasks WHERE type='transfer_owner'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replay duplicated job", count, err)
	}
	other, err := d.BeginTaskOperation(AdminOwner, "move_album", "transfer-two", "album:"+fmt.Sprint(album), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.BeginOwnerTransfer(f.ID, "unused", album, other.ID, []byte{5}, []byte{6}, 1); !errors.Is(err, ErrTaskOperationConflict) {
		t.Fatal("replay escaped operation membership", err)
	}
	if err = d.SealTaskOperation(op.ID, AdminOwner); err != nil {
		t.Fatal(err)
	}
	if _, err = d.BeginOwnerTransfer(f.ID, "unused", album, op.ID, []byte{5}, []byte{6}, 1); !errors.Is(err, ErrTaskOperationClosed) {
		t.Fatal("replay reopened sealed input", err)
	}
	if _, err = d.MutationFile(f.ID); !errors.Is(err, ErrConverting) {
		t.Fatal("staged source allowed mutation", err)
	}
	if err = d.DeleteAlbum(album); err == nil {
		t.Fatal("target album deleted during transfer")
	}
	if _, err = d.QueueAccountDelete("user:destination"); err == nil {
		t.Fatal("target account deleted during transfer")
	}
	if _, err = d.BeginConversion(f.ID, "wrong-kind", true, []byte{1}, []byte{2}, 1); !errors.Is(err, ErrConverting) {
		t.Fatal("conversion reused an ownership transfer", err)
	}
	if err = d.ConversionPhase(f.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err == nil {
		t.Fatal("unarchived destination promoted")
	}
	if err = d.SetArchiveState(c.TargetID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE accounts SET disabled=1 WHERE id='user:destination'`); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err == nil {
		t.Fatal("disabled target accepted at cutover")
	}
	if original, err := d.GetVisibleFile(f.ID); err != nil || original.OwnerID != AdminOwner {
		t.Fatal("failed cutover changed source", original, err)
	}
	if _, err = d.Exec(`UPDATE accounts SET disabled=0 WHERE id='user:destination'`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET cancel_requested=1 WHERE id=?`, c.TaskID); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err == nil {
		t.Fatal("canceled transfer committed")
	}
	if err = d.RetryTask(c.TaskID); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(f.ID); err != nil {
		t.Fatal(err)
	}
	var cleanup int64
	if err = d.QueryRow(`SELECT id FROM tasks WHERE type='cleanup_converted' AND file_id=?`, f.ID).Scan(&cleanup); err != nil {
		t.Fatal(err)
	}
	got, item := followupOperation(t, d, cleanup)
	if got != op.ID || item != f.ID {
		t.Fatal("cleanup lost original operation", got, item)
	}
}

func TestOwnerTransferRejectedInputsLeaveNoStagingOrTaskChanges(t *testing.T) {
	for _, scenario := range []string{"same-owner", "disabled", "deleting", "target-frozen", "source-frozen", "running", "unarchived", "account-operation"} {
		t.Run(scenario, func(t *testing.T) {
			d, f, album := ownerTransferFixture(t)
			operation := int64(0)
			var err error
			switch scenario {
			case "same-owner":
				album, err = d.InsertAlbum("Own")
			case "disabled":
				_, err = d.Exec(`UPDATE accounts SET disabled=1 WHERE id='user:destination'`)
			case "deleting":
				_, err = d.QueueAccountDelete("user:destination")
			case "target-frozen":
				err = d.InsertTopicBinding(&TopicBinding{AlbumID: album, AccountID: 1, ChatID: 1, TopicID: 1, State: "deleting"})
			case "source-frozen":
				a, e := d.InsertAlbum("Frozen")
				if e != nil {
					t.Fatal(e)
				}
				_, err = d.Exec(`UPDATE files SET album_id=? WHERE id=?`, a, f.ID)
				if err == nil {
					err = d.InsertTopicBinding(&TopicBinding{AlbumID: a, AccountID: 1, ChatID: 1, TopicID: 1, State: "deleting"})
				}
			case "running":
				_, err = d.Exec(`INSERT INTO tasks(type,file_id,state,run_after,created_at,updated_at) VALUES('verify',?,'running',1,1,1)`, f.ID)
			case "unarchived":
				err = d.SetArchiveState(f.ID, "pending", "")
			case "account-operation":
				var op *TaskOperation
				op, err = d.BeginTaskOperation("user:destination", "move_album", "unauthorized", "album:"+fmt.Sprint(album), 1)
				if err == nil {
					operation = op.ID
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.BeginOwnerTransfer(f.ID, "rejected-target", album, operation, []byte{1}, []byte{2}, 1); err == nil {
				t.Fatal("unsafe transfer accepted")
			}
			var staged, jobs int
			if err = d.QueryRow(`SELECT (SELECT COUNT(*) FROM file_conversions),(SELECT COUNT(*) FROM tasks WHERE type='transfer_owner')`).Scan(&staged, &jobs); err != nil || staged != 0 || jobs != 0 {
				t.Fatal("rejection left transfer side effects", staged, jobs, err)
			}
			if _, err = d.GetFile("rejected-target"); !errors.Is(err, ErrNotFound) {
				t.Fatal("rejection left staging file", err)
			}
		})
	}
}
