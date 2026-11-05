package dbx

import (
	"context"
	"testing"
)

func TestFilePresentationsRespectVisibilityAndStoredCopies(t *testing.T) {
	d := telegramTestDB(t)
	album, err := d.InsertAlbum("exhibition")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"shown", "private", "hidden", "pending", "unpublished", "outside"}
	for _, id := range ids {
		if err := d.InsertFile(telegramTestFile(id, album)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetAlbumDisplayed(album, true); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE files SET home_visibility='private' WHERE id='private'`,
		`UPDATE files SET hidden=1 WHERE id='hidden'`,
		`UPDATE files SET archive_state='pending' WHERE id='pending'`,
		`UPDATE files SET media_version=2,hot_backend='hot',published=1,published_path='current' WHERE id='shown'`,
		`INSERT INTO media_publications(file_id,backend,object_key,size,sha256,mime,encrypted,payload_size,payload_sha256,ready)
		 VALUES('shown','hot','current',1,'hash','image/png',0,1,'hash',1),
		 ('shown','hot','obsolete',1,'hash','image/png',1,1,'hash',1),
		 ('unpublished','hot','not-ready',1,'hash','image/png',1,1,'hash',0)`,
		`UPDATE files SET hot_backend='hot',published_path='not-ready' WHERE id='unpublished'`,
		`INSERT INTO temp_objects(id,file_id,kind,object_key,created_at,expires_at,deleted_at)
		 VALUES('live','shown','original','a',1,200,NULL),('expired','shown','original','b',1,99,NULL),
		 ('deleted','shown','original','c',1,200,1),('legacy','shown','original','d',1,200,NULL),
		 ('outside-copy','outside','original','e',1,200,NULL)`,
		`INSERT INTO media_temporary(object_id,mime,size,sha256,encrypted,payload_size,payload_sha256)
		 SELECT id,'image/png',1,'hash',1,1,'hash' FROM temp_objects WHERE id!='legacy'`,
	} {
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	requested := append(append([]string{}, ids[:len(ids)-1]...), "missing")
	states, err := d.FilePresentations(context.Background(), requested, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != len(ids)-1 {
		t.Fatalf("unexpected page membership: %v", states)
	}
	for _, id := range requested {
		if states[id].Exhibited != d.FileExhibited(id) {
			t.Fatalf("visibility differs for %s", id)
		}
	}
	shown := states["shown"]
	if shown.PublicationEncrypted == nil || *shown.PublicationEncrypted {
		t.Fatal("current clear publication confused with obsolete encrypted copy")
	}
	if states["unpublished"].PublicationEncrypted != nil {
		t.Fatal("unready publication exposed as confirmed")
	}
	if len(shown.TemporaryEncrypted) != 1 || !shown.TemporaryEncrypted["live"] {
		t.Fatalf("expired, deleted, legacy or unrelated temporary copy included: %v", shown.TemporaryEncrypted)
	}
	if err := d.SetAlbumDisplayed(album, false); err != nil {
		t.Fatal(err)
	}
	states, err = d.FilePresentations(context.Background(), requested, 100)
	if err != nil || states["shown"].Exhibited {
		t.Fatal("page retained revoked exhibition grant", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.FilePresentations(ctx, requested, 100); err == nil {
		t.Fatal("canceled request still queried page metadata")
	}
}
