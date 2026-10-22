package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
	"testing"
)

func TestTelegramDeleteLetsAcceptedArchiveFinishBeforeMoving(t *testing.T) {
	s, remote := workflowEnv(t)
	album, topic := workflowBound(t, s, remote)
	f, err := s.receiveLegacyTelegramFixture(context.Background(), workflowPNG(t), "accepted.png", "image/png", album, "11:77:123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil {
		t.Fatal("pending archive was ignored")
	}
	if remote.topics[topic].Closed {
		t.Fatal("topic closed before accepted archive could finish")
	}
	if err := s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatalf("album freeze trapped the accepted archive: %v", err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	current, err := s.DB.GetFile(f.ID)
	if err != nil || current.AlbumID.Int64 == album {
		t.Fatal("accepted archive was not moved", err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramDeletingOldInboxPreservesFilesAlreadyMovedElsewhere(t *testing.T) {
	s, remote := workflowEnv(t)
	source, topic := workflowBound(t, s, remote)
	dest, err := s.CreateLinkedAlbum(context.Background(), "already moved destination")
	if err != nil {
		t.Fatal(err)
	}
	data := workflowPNG(t)
	remote.add(topic, 123, 100, "intake.png", "image/png", "", data)
	f, err := s.receiveLegacyTelegramFixture(context.Background(), data, "intake.png", "image/png", source, "11:77:123")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveTelegramFile(context.Background(), f.ID, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	current, err := s.DB.GetFile(f.ID)
	if err != nil || current.AlbumID.Int64 != dest {
		t.Fatal("old inbox deletion moved a file out of its new album", err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramDeleteDrainsExistingConversionAndRejectsNewOnes(t *testing.T) {
	s, remote := workflowEnv(t)
	album, topic := workflowBound(t, s, remote)
	first, err := s.receiveLegacyTelegramFixture(context.Background(), workflowPNG(t), "converting.png", "image/png", album, "11:77:128")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.receiveLegacyTelegramFixture(context.Background(), append(workflowPNG(t), 2), "frozen.png", "image/png", album, "11:77:129")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := s.RunArchive(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.QueueConversion(context.Background(), first.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueConversion(context.Background(), second.ID, false); !errors.Is(err, dbx.ErrAlbumFrozen) {
		t.Fatalf("conversion entered frozen album: %v", err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil {
		t.Fatal("pending conversion was ignored")
	}
	if remote.topics[topic].Closed {
		t.Fatal("topic closed before conversion finished")
	}
	newID, err := s.RunConversion(context.Background(), first.ID)
	if err != nil {
		t.Fatal("accepted conversion could not drain", err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), newID); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramDeleteResumesItsMigrationAfterCopyOrCleanupFailure(t *testing.T) {
	for _, stage := range []string{"copy", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			s, remote := workflowEnv(t)
			album, topic := workflowBound(t, s, remote)
			f := workflowArchivedFile(t, s, album)
			chunks, err := s.DB.ListChunks(f.ID, "data")
			if err != nil || len(chunks) != 1 {
				t.Fatal("missing source", err)
			}
			old, err := s.TGReference(chunks[0])
			if err != nil {
				t.Fatal(err)
			}
			if stage == "copy" {
				remote.failNextCopyDownload = true
			} else {
				remote.failDelete[old.MessageID] = 1
			}
			if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
				t.Fatal(err)
			}
			if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil {
				t.Fatal("expected interrupted migration")
			}
			move, err := s.DB.GetTelegramMove(f.ID)
			if err != nil || move.Phase != stage {
				t.Fatalf("lost %s checkpoint: %+v, %v", stage, move, err)
			}
			if remote.deleteTopicCalls != 0 {
				t.Fatal("topic removed before migration completed")
			}
			if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
				t.Fatal("deletion could not resume its own migration", err)
			}
			if _, err := s.DB.GetTelegramMove(f.ID); !errors.Is(err, dbx.ErrNotFound) {
				t.Fatal("unfinished cleanup left after topic deletion", err)
			}
			if _, exists := remote.topics[topic]; exists {
				t.Fatal("source topic was not deleted")
			}
			current, _ := s.DB.GetFile(f.ID)
			if current.AlbumID.Int64 == album {
				t.Fatal("file did not move into Uncategorized")
			}
			if _, err := s.Verify(context.Background(), f.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type committedUploadTG struct {
	*videoWorkflowTG
	receipts    map[int64]tguser.Reference
	loseReceipt bool
}

func (f *committedUploadTG) UploadStream(ctx context.Context, chat int64, topic int, name, mime string, input io.Reader, size int64, thumb []byte, caption string, random int64, video *tguser.VideoMetadata) (tguser.Reference, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return tguser.Reference{}, err
	}
	if ref, ok := f.receipts[random]; ok {
		return ref, nil
	}
	ref, err := f.videoWorkflowTG.UploadStream(ctx, chat, topic, name, mime, bytes.NewReader(data), size, thumb, caption, random, video)
	if err != nil {
		return ref, err
	}
	f.receipts[random] = ref
	if f.loseReceipt {
		f.loseReceipt = false
		return tguser.Reference{}, errors.New("connection lost after Telegram commit")
	}
	return ref, nil
}

func TestTelegramRetryAfterLostReceiptAndRenameKeepsCorrectDocument(t *testing.T) {
	s, remote := workflowEnv(t)
	fake := &committedUploadTG{videoWorkflowTG: &videoWorkflowTG{workflowTG: remote, limit: 4 << 20}, receipts: map[int64]tguser.Reference{}, loseReceipt: true}
	s.UserTG = fake
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(workflowPNG(t)), "rename.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), f.ID); err == nil {
		t.Fatal("missing lost receipt fault")
	}
	if err := s.DB.UpdateFileMeta(f.ID, "new name after failed receipt", sql.NullInt64{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal("retry reused a different PNG document", err)
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
}
