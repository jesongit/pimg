package dbx

import (
	"fmt"
	"testing"
)

func TestActivityMoveKeepsOriginalAndDestinationAfterLaterChanges(t *testing.T) {
	d := activityDB(t)
	from, _ := d.InsertAlbum("原相册")
	to, _ := d.InsertAlbum("目标相册")
	later, _ := d.InsertAlbum("之后的相册")
	f := operationFixtureFile(t, d, "context-move")
	if _, e := d.Exec(`UPDATE files SET album_id=? WHERE id=?`, from, f.ID); e != nil {
		t.Fatal(e)
	}
	op, e := d.BeginTaskOperation(AdminOwner, "move_album", "context-first", fmt.Sprintf("album:%d", to), 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.MoveFileInOperation(f.ID, to, op.ID); e != nil {
		t.Fatal(e)
	}
	if e = d.SealTaskOperation(op.ID, AdminOwner); e != nil {
		t.Fatal(e)
	}
	second, e := d.BeginTaskOperation(AdminOwner, "move_album", "context-second", fmt.Sprintf("album:%d", later), 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.MoveFileInOperation(f.ID, later, second.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = d.Exec(`UPDATE albums SET name='已重命名' WHERE id IN (?,?)`, from, to); e != nil {
		t.Fatal(e)
	}
	task, e := d.GetTaskGroup(op.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	c, e := d.ActivityContext(task)
	if e != nil || c.ToAlbum == nil || c.ToAlbum.Name != "目标相册" || len(c.FromAlbums) != 1 || c.FromAlbums[0].Name != "原相册" {
		t.Fatalf("snapshot lost: %+v %v", c, e)
	}
	rows, total, e := d.QueryUnifiedLogs(LogQuery{Search: "目标相册", Page: 1, Limit: 50})
	if e != nil || total != 1 || rows[0].Task.ID != op.TaskID {
		t.Fatal("snapshot search lost", total, e)
	}
}

func TestLogFileLinksUseEachFilesOwnerForAdministratorBatches(t *testing.T) {
	d := activityDB(t)
	if e := d.CreateAccount("user:log-other", "log-other", "hash"); e != nil {
		t.Fatal(e)
	}
	if e := d.InsertFile(&File{ID: "other-log-file", OwnerID: "user:log-other", Name: "other", OrigName: "other.jpg", Ext: "jpg", Mime: "image/jpeg", Kind: "image", Size: 1, SHA256: "hash", Storage: "direct", ColdBackend: "local", ArchiveState: "done", KeyWrapped: []byte{1}, KeyNonce: []byte{2}}); e != nil {
		t.Fatal(e)
	}
	op, e := d.BeginTaskOperation(AdminOwner, "purge", "cross-owner-log", "", 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.RecordTaskOperationItem(op.ID, AdminOwner, "other-log-file", "other-log-file", "other.jpg", "done", ""); e != nil {
		t.Fatal(e)
	}
	items, total, e := d.TaskOperationItems(op.ID, AdminOwner, "all", 1, 100)
	if e != nil || total != 1 || items[0].OwnerID != "user:log-other" || !items[0].FileExists {
		t.Fatal("operation link owner lost", items, total, e)
	}
	id, e := d.EnqueueTask(TaskVerify, "other-log-file", "{}")
	if e != nil {
		t.Fatal(e)
	}
	items, total, e = d.LogTaskFiles(id, 1, 100)
	if e != nil || total != 1 || items[0].OwnerID != "user:log-other" || !items[0].FileExists {
		t.Fatal("legacy link owner lost", items, total, e)
	}
}
