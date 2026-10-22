package dbx

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestTelegramUploadBatchFilesTasksAndPartsAreAtomic(t *testing.T) {
	d := telegramTestDB(t)
	a := telegramTestFile("a", 0)
	a.TelegramSource = "11:77:80"
	a.ArchiveState = "pending"
	b := telegramTestFile("b", 0)
	b.Storage = "direct"
	b.ArchiveState = "pending"
	if _, err := d.Exec(`CREATE TRIGGER fail_second_upload BEFORE INSERT ON tasks WHEN NEW.file_id='b' BEGIN SELECT RAISE(ABORT,'simulated enqueue failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertUploadBatch([]*File{a, b}); err == nil {
		t.Fatal("expected mid-batch failure")
	}
	for _, table := range []string{"files", "tasks", "telegram_source_parts"} {
		var count int
		if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s committed: %d %v", table, count, err)
		}
	}
	if _, err := d.Exec(`DROP TRIGGER fail_second_upload`); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertUploadBatch([]*File{a, b}); err != nil {
		t.Fatal(err)
	}
	parts, err := d.TelegramSourceParts(a.TelegramSource)
	if err != nil || len(parts) != 2 || parts[0].FileID != a.ID || parts[1].FileID != b.ID || parts[0].PartIndex != 0 || parts[1].PartIndex != 1 {
		t.Fatalf("incomplete batch receipt: %+v %v", parts, err)
	}
	for _, want := range []struct{ id, typ string }{{a.ID, TaskArchive}, {b.ID, TaskDirectArchive}} {
		var typ string
		if err = d.QueryRow(`SELECT type FROM tasks WHERE file_id=?`, want.id).Scan(&typ); err != nil || typ != want.typ {
			t.Fatal("wrong task for batch part", want, typ, err)
		}
	}
}

func TestTelegramUploadBatchReceiptsSurvivePurgeAndConversion(t *testing.T) {
	d := telegramTestDB(t)
	a := telegramTestFile("first", 0)
	a.TelegramSource = "11:77:81"
	b := telegramTestFile("second", 0)
	if err := d.InsertUploadBatch([]*File{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertTelegramMessage(&TelegramMessage{AccountID: 11, ChatID: 77, TopicID: 10, MessageID: 81, State: "done", FileID: a.ID}); err != nil {
		t.Fatal(err)
	}
	c, err := d.BeginConversion(b.ID, "converted", false, []byte{1}, []byte{2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetArchiveState(c.TargetID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.ConversionPhase(b.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteConversion(b.ID); err != nil {
		t.Fatal(err)
	}
	parts, err := d.TelegramSourceParts(a.TelegramSource)
	if err != nil || len(parts) != 2 || parts[1].FileID != c.TargetID {
		t.Fatal("conversion lost source family", parts, err)
	}
	if err = d.DeleteFile(c.TargetID); err != nil {
		t.Fatal(err)
	}
	parts, _ = d.TelegramSourceParts(a.TelegramSource)
	if parts[1].PurgedAt == 0 || parts[0].PurgedAt != 0 {
		t.Fatal("explicit part purge receipt lost", parts)
	}
	m, _ := d.GetTelegramMessage(11, 77, 81)
	if m.State != "retired" {
		t.Fatal("deleted child can be recreated from source")
	}
	refs, err := d.FileTaskReferences(a.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range refs {
		if err = d.RequestTaskCancel(id); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.DeleteFile(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.FileByTelegramSource(a.TelegramSource); !errors.Is(err, ErrTelegramSourceRetired) {
		t.Fatal("purged first part looked like a new source", err)
	}
	parts, _ = d.TelegramSourceParts(a.TelegramSource)
	if len(parts) != 2 || parts[0].PurgedAt == 0 {
		t.Fatal("purge erased batch receipt", parts)
	}
}

func TestTelegramSourcePartsMigrationFromV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err = old.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = old.Exec(`CREATE TABLE schema_version(v INTEGER NOT NULL); INSERT INTO schema_version VALUES(6);
INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at,telegram_source)
VALUES('legacy','photo','photo.png','png','image/png',1,'hash','image',X'01',X'02','done',1,1,'11:77:82');`); err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parts, err := d.TelegramSourceParts("11:77:82")
	if err != nil || len(parts) != 1 || parts[0].FileID != "legacy" || parts[0].PurgedAt != 0 {
		t.Fatal("single-file source receipt not migrated", parts, err)
	}
}
