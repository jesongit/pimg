package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

func workflowArchivedFile(t *testing.T, s *Service, album int64) *dbx.File {
	t.Helper()
	f, err := s.receiveLegacyTelegramFixture(context.Background(), workflowPNG(t), "photo.png", "image/png", album, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTelegramMoveCopiesAllPartsAndResumesSavedCopy(t *testing.T) {
	s, tg := workflowEnv(t)
	album, oldTopic := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	destination, err := s.EnsureTelegramAlbum(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	oldChunks := map[int64]string{}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, _ := s.DB.ListChunks(f.ID, kind)
		for _, c := range chunks {
			oldChunks[c.ID] = c.ObjectKey
		}
	}
	if len(oldChunks) != 1 {
		t.Fatal("fixture must contain only one original")
	}
	tg.failNextCopyDownload = true
	if err = s.MoveTelegramFile(context.Background(), f.ID, 0); err == nil {
		t.Fatal("expected interrupted copy verification")
	}
	unchanged, _ := s.DB.GetFile(f.ID)
	if unchanged.AlbumID.Int64 != album {
		t.Fatal("album switched before all copies verified")
	}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, _ := s.DB.ListChunks(f.ID, kind)
		for _, c := range chunks {
			if c.ObjectKey != oldChunks[c.ID] {
				t.Fatal("partial locator switch escaped transaction")
			}
		}
	}
	move, err := s.DB.GetTelegramMove(f.ID)
	if err != nil || move.Phase != "copy" {
		t.Fatal("copy checkpoint missing", err)
	}
	if err = s.MoveTelegramFile(context.Background(), f.ID, 0); err != nil {
		t.Fatal(err)
	}
	if tg.copyCalls != len(oldChunks) {
		t.Fatalf("saved copy repeated after retry: calls=%d parts=%d", tg.copyCalls, len(oldChunks))
	}
	updated, _ := s.DB.GetFile(f.ID)
	if updated.AlbumID.Int64 != destination.AlbumID {
		t.Fatal("destination album not switched")
	}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, _ := s.DB.ListChunks(f.ID, kind)
		for _, c := range chunks {
			r, err := s.TGReference(c)
			if err != nil || r.TopicID != destination.TopicID || r.TopicID == oldTopic {
				t.Fatal("cover/original locator did not move", r, err)
			}
		}
	}
	if _, err = s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("moved plaintext differs", err)
	}
	if _, err = s.DB.GetTelegramMove(f.ID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("finished move retains unfinished state", err)
	}
}

func TestTelegramMoveCleanupBlocksDestructiveActionsAndDefaultRetry(t *testing.T) {
	s, tg := workflowEnv(t)
	album, _ := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	chunks, _ := s.DB.ListChunks(f.ID, "data")
	oldRef, err := s.TGReference(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	tg.failDelete[oldRef.MessageID] = 1
	if err = s.MoveTelegramFile(context.Background(), f.ID, 0); err == nil {
		t.Fatal("expected cleanup failure")
	}
	move, err := s.DB.GetTelegramMove(f.ID)
	if err != nil || move.Phase != "cleanup" {
		t.Fatal("cleanup locators lost", err)
	}
	current, _ := s.DB.GetFile(f.ID)
	if current.AlbumID.Int64 == album {
		t.Fatal("verified target not visible during cleanup failure")
	}
	if _, err = s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("cleanup failure broke new archive", err)
	}
	if _, err = s.QueueConversion(context.Background(), f.ID, false); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("conversion queued over pending cleanup", err)
	}
	if _, err = s.DB.EnqueuePurgeTask(f.ID); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("purge queued over pending cleanup", err)
	}
	if err = s.RunPurgeFile(context.Background(), f.ID); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("direct purge bypassed pending cleanup", err)
	}
	if _, err = s.DB.BeginConversion(f.ID, "bad-conversion", false, []byte{1}, []byte{2}, 1); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("DB conversion bypassed pending cleanup", err)
	}
	taskID, err := s.QueueTelegramMove(context.Background(), f.ID, 0)
	if err != nil {
		t.Fatal("default alias cannot resume stored destination", err)
	}
	task, _ := s.DB.GetTask(taskID)
	var payload struct {
		AlbumID int64 `json:"album_id"`
	}
	if err = json.Unmarshal([]byte(task.Payload), &payload); err != nil || payload.AlbumID != move.AlbumID || payload.AlbumID == 0 {
		t.Fatal("queued default was not normalized", task.Payload, err)
	}
	copyCount := tg.copyCalls
	if err = s.MoveTelegramFile(context.Background(), f.ID, 0); err != nil {
		t.Fatal("cleanup did not resume", err)
	}
	if tg.copyCalls != copyCount {
		t.Fatal("cleanup retry recopied verified archive")
	}
	if err = s.DB.FinishTask(taskID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueConversion(context.Background(), f.ID, false); err != nil {
		t.Fatal("completed move kept conversion locked", err)
	}
}

func TestTelegramMoveRejectsCorruptCopyWithoutCutover(t *testing.T) {
	s, tg := workflowEnv(t)
	album, _ := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	tg.corruptCopy = true
	if err := s.MoveTelegramFile(context.Background(), f.ID, 0); err == nil {
		t.Fatal("corrupted copy was accepted")
	}
	got, _ := s.DB.GetFile(f.ID)
	if got.AlbumID.Int64 != album || tg.deleteCalls != 0 {
		t.Fatal("corruption retired original")
	}
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("corrupt replacement harmed source", err)
	}
}

type canceledMoveTG struct {
	*workflowTG
	cancel    context.CancelFunc
	readBytes int
	closed    bool
}

func (f *canceledMoveTG) UploadStream(context.Context, int64, int, string, string, io.Reader, int64, []byte, string, int64, *tguser.VideoMetadata) (tguser.Reference, error) {
	return tguser.Reference{}, errors.New("unexpected upload during move")
}

func (f *canceledMoveTG) OpenStream(ctx context.Context, ref tguser.Reference) (io.ReadCloser, error) {
	data, err := f.Download(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &canceledMoveReader{ctx: ctx, data: bytes.NewReader(data), transport: f}, nil
}

type canceledMoveReader struct {
	ctx       context.Context
	data      *bytes.Reader
	transport *canceledMoveTG
}

func (r *canceledMoveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// Deliver a real wire prefix before cancellation. A failed download that
	// returns no reader would not exercise the migration's io.Copy error path.
	n, err := r.data.Read(p[:min(len(p), 64)])
	if n > 0 {
		r.transport.readBytes += n
		r.transport.cancel()
	}
	return n, err
}

func (r *canceledMoveReader) Close() error {
	r.transport.closed = true
	return nil
}

func TestTelegramMoveMidReadCancellationKeepsOriginalAndCause(t *testing.T) {
	s, tg := workflowEnv(t)
	album, _ := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	oldKeys := map[int64]string{}
	oldMedia := map[int][]byte{}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, err := s.DB.ListChunks(f.ID, kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			oldKeys[chunk.ID] = chunk.ObjectKey
			ref, err := s.TGReference(chunk)
			if err != nil {
				t.Fatal(err)
			}
			oldMedia[ref.MessageID], err = tg.Download(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamTG := &canceledMoveTG{workflowTG: tg, cancel: cancel}
	s.UserTG = streamTG
	err := s.MoveTelegramFile(ctx, f.ID, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-read cancellation lost its cause: %v", err)
	}
	if streamTG.readBytes == 0 || !streamTG.closed {
		t.Fatal("fixture did not cancel a partially read stream and close it")
	}
	current, err := s.DB.GetFile(f.ID)
	if err != nil || current.AlbumID.Int64 != album {
		t.Fatal("canceled copy changed the authoritative album", err)
	}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		chunks, err := s.DB.ListChunks(f.ID, kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			if chunk.ObjectKey != oldKeys[chunk.ID] {
				t.Fatal("canceled stream committed a partial locator switch")
			}
		}
	}
	if tg.deleteCalls != 0 {
		t.Fatal("canceled verification deleted source media")
	}
	for messageID, data := range oldMedia {
		if _, exists := tg.messages[messageID]; !exists || !bytes.Equal(tg.data[messageID], data) {
			t.Fatal("canceled verification changed an original message")
		}
	}
	move, err := s.DB.GetTelegramMove(f.ID)
	if err != nil || move.Phase != "copy" {
		t.Fatal("canceled verification did not retain the copy checkpoint", err)
	}
	var parts []telegramMovingPart
	if err := json.Unmarshal([]byte(move.References), &parts); err != nil || len(parts) == 0 || parts[0].New == nil || parts[0].Cleaned {
		t.Fatal("saved remote copy was lost after cancellation", err)
	}
	s.UserTG = tg
	if _, err := s.Verify(context.Background(), f.ID); err != nil {
		t.Fatal("original archive is no longer recoverable after cancellation", err)
	}
}

func TestTelegramMoveQueuedAndLegacyConversionRunGuards(t *testing.T) {
	s, tg := workflowEnv(t)
	album, _ := workflowBound(t, s, tg)
	f := workflowArchivedFile(t, s, album)
	taskID, err := s.QueueTelegramMove(context.Background(), f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueConversion(context.Background(), f.ID, false); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("pending move task ignored by conversion", err)
	}
	if _, err = s.DB.EnqueuePurgeTask(f.ID); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("pending move task ignored by purge", err)
	}
	if err = s.DB.FinishTask(taskID, ""); err != nil {
		t.Fatal(err)
	}
	c, err := s.QueueConversion(context.Background(), f.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.SaveTelegramMove(&dbx.TelegramMove{FileID: f.ID, AlbumID: album, Phase: "cleanup"}); err == nil {
		t.Fatal("new move intent bypassed an active conversion")
	}
	// Simulate persisted conflicting state from before the intent guard existed.
	if _, err = s.DB.Exec(`INSERT INTO tg_moves(file_id,album_id,phase,refs,created_at,updated_at) VALUES(?,?,'cleanup','[]',0,0)`, f.ID, album); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunConversion(context.Background(), f.ID); !errors.Is(err, dbx.ErrTelegramMoving) {
		t.Fatal("legacy conflicting conversion bypassed run guard", err)
	}
	target, _ := s.DB.GetFile(c.TargetID)
	if !target.Hidden || target.ArchiveState != "pending" {
		t.Fatal("conversion wrote conflicting replacement")
	}
}
