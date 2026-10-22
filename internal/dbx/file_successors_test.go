package dbx

import "testing"

func TestFileSuccessorsFollowTransferAndConversionWithoutChangingAuditIdentity(t *testing.T) {
	d, f, album := ownerTransferFixture(t)
	c, err := d.BeginOwnerTransfer(f.ID, "successor-transfer", album, 0, []byte{7}, []byte{8}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := d.FileSuccessors([]string{f.ID}); err != nil || len(m) != 0 {
		t.Fatal("unfinished transfer exposed", m, err)
	}
	d.SetArchiveState(c.TargetID, "done", "")
	d.ConversionPhase(f.ID, "archive")
	if err = d.CompleteConversion(f.ID); err != nil {
		t.Fatal(err)
	}
	m, err := d.FileSuccessors([]string{f.ID})
	if err != nil || m[f.ID] == nil || m[f.ID].Kind != "transferred" || m[f.ID].ID != c.TargetID {
		t.Fatal(m, err)
	}
	target, _ := d.GetFile(c.TargetID)
	converted, err := d.BeginConversion(target.ID, "successor-converted", target.Storage == "direct", []byte{5}, []byte{6}, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.SetArchiveState(converted.TargetID, "done", "")
	d.ConversionPhase(target.ID, "archive")
	if err = d.CompleteConversion(target.ID); err != nil {
		t.Fatal(err)
	}
	m, err = d.FileSuccessors([]string{f.ID, target.ID})
	if err != nil || m[f.ID].ID != converted.TargetID || m[f.ID].Kind != "transferred" || m[target.ID].Kind != "converted" {
		t.Fatal(m, err)
	}
	original, _ := d.GetFile(f.ID)
	if original.OwnerID != f.OwnerID || original.ID != f.ID {
		t.Fatal("historical identity changed")
	}
	d.Exec(`UPDATE files SET hidden=1 WHERE id=?`, converted.TargetID)
	if m, err = d.FileSuccessors([]string{f.ID}); err != nil || len(m) != 0 {
		t.Fatal("unavailable successor exposed", m, err)
	}
}
