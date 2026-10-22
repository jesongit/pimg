package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type conversionColdFault struct {
	storage.Cold
	failPut, failDelete bool
}

func (b *conversionColdFault) Put(ctx context.Context, key string, data []byte, caption string) (storage.Reference, error) {
	if b.failPut {
		return storage.Reference{}, errors.New("injected conversion upload interruption")
	}
	return b.Cold.Put(ctx, key, data, caption)
}

func (b *conversionColdFault) Delete(ctx context.Context, ref storage.Reference) error {
	if b.failDelete {
		return errors.New("injected retained-source deletion failure")
	}
	return b.Cold.Delete(ctx, ref)
}

func conversionFixture(t *testing.T, direct bool) (*Service, *dbx.File, []byte) {
	t.Helper()
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	f, err := s.receiveLegacyUploadFixture(context.Background(), bytes.NewReader(data.Bytes()), "conversion.png", "png", sql.NullInt64{}, nil, 0, direct)
	if err != nil {
		t.Fatal(err)
	}
	if direct {
		err = s.RunDirectArchive(context.Background(), f.ID)
	} else {
		err = s.RunArchive(context.Background(), f.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, data.Bytes()
}

func assertConversionOriginal(t *testing.T, s *Service, fileID string, expected []byte) {
	t.Helper()
	f, err := s.DB.GetFile(fileID)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.SourceStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(stream)
	stream.Close()
	if readErr != nil || !bytes.Equal(actual, expected) {
		t.Fatal("recoverable original was lost or changed", readErr)
	}
}

func TestConversionResumesAfterRestartAndCleanupFailureDoesNotBlockCutover(t *testing.T) {
	s, source, plain := conversionFixture(t, true)
	ctx := context.Background()
	c, err := s.QueueConversion(ctx, source.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	fault := &conversionColdFault{Cold: s.coldBackends["local"], failPut: true}
	s.coldBackends["local"] = fault
	if _, err := s.RunConversion(ctx, source.ID); err == nil {
		t.Fatal("conversion upload fault did not fail")
	}
	c, err = s.DB.GetConversion(source.ID)
	if err != nil || c.Phase != "archive" || c.LastError == "" {
		t.Fatal("failed conversion lost its resumable phase/error", err)
	}
	if _, err := s.DB.GetVisibleFile(c.TargetID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("unverified staging target became visible", err)
	}
	if files, total, err := s.DB.ListFiles(dbx.FileFilter{}); err != nil || total != 1 || files[0].ID != source.ID {
		t.Fatal("staging conversion duplicated the library", err)
	}
	assertConversionOriginal(t, s, source.ID, plain)
	cipherPath := s.ctPath(c.TargetID)
	before, err := os.ReadFile(cipherPath)
	if err != nil || len(before) == 0 {
		t.Fatal("encrypted staging work was not persisted", err)
	}
	s.CleanupLocalTemps()
	after, err := os.ReadFile(cipherPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("restart GC discarded resumable staging ciphertext", err)
	}
	if _, err := s.DB.Exec("UPDATE tasks SET state='running' WHERE id=?", c.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ResetRunningTasks(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.Cfg, s.DB, s.Master, s.TG, s.R2, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := restarted.QueueConversion(ctx, source.ID, true)
	if err != nil || reused.TaskID != c.TaskID || reused.TargetID != c.TargetID {
		t.Fatal("retry created another conversion instead of resuming", err)
	}
	targetID, err := restarted.RunConversion(ctx, source.ID)
	if err != nil || targetID != c.TargetID {
		t.Fatal("conversion failed to resume its archived work", err)
	}
	target, err := s.DB.GetVisibleFile(targetID)
	if err != nil || target.Storage == "direct" || target.Published || target.ShowHome || target.HotBackend != "" || target.ColdBackend != source.ColdBackend || target.CreatedAt != source.CreatedAt {
		t.Fatal("replacement changed provider/metadata or published automatically", err)
	}
	if _, err := s.DB.GetVisibleFile(source.ID); !errors.Is(err, dbx.ErrNotFound) {
		t.Fatal("retired source remained visible", err)
	}
	assertConversionOriginal(t, restarted, targetID, plain)
	cleanupFault := &conversionColdFault{Cold: restarted.coldBackends["local"], failDelete: true}
	restarted.coldBackends["local"] = cleanupFault
	if err := restarted.RunConvertedCleanup(ctx, source.ID); err == nil {
		t.Fatal("source cleanup fault did not fail")
	}
	c, err = s.DB.GetConversion(source.ID)
	if err != nil || c.Phase != "done" || c.State != "done" || !c.CleanupPending || c.CleanupError == "" {
		t.Fatal("cleanup failure rolled back conversion or lost its retry receipt", err)
	}
	if chunks, err := s.DB.ListChunks(source.ID, "data"); err != nil || len(chunks) == 0 {
		t.Fatal("failed cleanup discarded the remaining source locators", err)
	}
	assertConversionOriginal(t, restarted, targetID, plain)
	cleanupFault.failDelete = false
	if err := restarted.RunConvertedCleanup(ctx, source.ID); err != nil {
		t.Fatal("retired cleanup could not resume", err)
	}
	c, _ = s.DB.GetConversion(source.ID)
	if c.CleanupPending || c.CleanupError != "" {
		t.Fatal("successful retired cleanup remained pending")
	}
	assertConversionOriginal(t, restarted, targetID, plain)
}

func TestConversionDuplicateRequestsAndCancelledLocksAreSafe(t *testing.T) {
	s, source, plain := conversionFixture(t, false)
	ctx := context.Background()
	var group sync.WaitGroup
	results := make(chan *dbx.Conversion, 8)
	failures := make(chan error, 8)
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			c, err := s.QueueConversion(ctx, source.ID, false)
			if err != nil {
				failures <- err
			} else {
				results <- c
			}
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first *dbx.Conversion
	for c := range results {
		if first == nil {
			first = c
		} else if first.TaskID != c.TaskID || first.TargetID != c.TargetID {
			t.Fatal("concurrent conversion requests created duplicate replacements")
		}
	}
	if _, err := s.QueueConversion(ctx, source.ID, true); !errors.Is(err, dbx.ErrConverting) {
		t.Fatal("opposing conversion request was accepted", err)
	}
	if _, err := s.DB.EnqueuePurgeTask(source.ID); !errors.Is(err, dbx.ErrConverting) {
		t.Fatal("purge raced an unfinished conversion", err)
	}
	if err := s.RunTempMaterialize(ctx, source.ID, "original"); !errors.Is(err, dbx.ErrConverting) {
		t.Fatal("a producer recreated serving copies while converting", err)
	}
	if err := s.DB.UpdateFileMeta(source.ID, "racing rename", sql.NullInt64{}, nil); !errors.Is(err, dbx.ErrConverting) {
		t.Fatal("metadata changed after its replacement was staged", err)
	}
	release, err := s.lockFile(ctx, first.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	if _, err := s.RunConversion(cancelled, source.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("conversion lock wait was not cancellable", err)
	}
	cancel()
	release()
	available, finish := context.WithTimeout(ctx, 3*time.Second)
	defer finish()
	targetID, err := s.RunConversion(available, source.ID)
	if err != nil || targetID != first.TargetID {
		t.Fatal("cancelled lock wait left conversion deadlocked", err)
	}
	assertConversionOriginal(t, s, targetID, plain)
	if _, err := s.RunConversion(ctx, source.ID); err != nil {
		t.Fatal("completed conversion was not idempotent", err)
	}
	var cleanupTasks int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE type='cleanup_converted' AND file_id=?", source.ID).Scan(&cleanupTasks); err != nil || cleanupTasks != 1 {
		t.Fatal("completed conversion queued duplicate source cleanups", cleanupTasks, err)
	}
}

func TestLegacyUnpublishSurvivesFailedConversion(t *testing.T) {
	s, source, plain := conversionFixture(t, false)
	ctx := context.Background()
	if err := s.localHot.PutObject(ctx, storage.Permanent, origKey(source), bytes.NewReader(plain), int64(len(plain)), source.Mime, "no-store"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE files SET hot_backend='local' WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.SetPublished(source.ID, true, origKey(source), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RunTempMaterialize(ctx, source.ID, "original"); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueueEncryptedUnpublishes(); err != nil {
		t.Fatal(err)
	}
	unpublish, err := s.DB.FindActiveTask(dbx.TaskUnpublish, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueConversion(ctx, source.ID, false); err != nil {
		t.Fatal(err)
	}
	if preserved, err := s.DB.GetTask(unpublish.ID); err != nil || preserved.State != "pending" {
		t.Fatal("conversion cancelled mandatory legacy revocation", err)
	}
	s.coldBackends["local"] = &conversionColdFault{Cold: s.coldBackends["local"], failPut: true}
	if _, err := s.RunConversion(ctx, source.ID); err == nil {
		t.Fatal("conversion fixture upload fault did not fail")
	}
	if err := s.DB.QueueEncryptedUnpublishes(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE tasks SET state='failed' WHERE id=?", unpublish.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.RetryTask(unpublish.ID); err != nil {
		t.Fatal("failed conversion blocked retry of mandatory revocation", err)
	}
	if err := s.RunUnpublish(ctx, source.ID); err != nil {
		t.Fatal("failed conversion prevented legacy revocation", err)
	}
	fresh, _ := s.DB.GetFile(source.ID)
	if fresh.Published {
		t.Fatal("legacy permanent publication remained after successful revocation")
	}
	assertConversionOriginal(t, s, source.ID, plain)
	object, err := s.DB.LiveTempObject(source.ID, "original", time.Now().Unix())
	if err != nil {
		t.Fatal("legacy revocation removed a legal temporary copy", err)
	}
	if _, err := s.localHot.HeadSize(ctx, storage.Temporary, object.ObjectKey); err != nil {
		t.Fatal("legacy revocation deleted temporary bytes", err)
	}
	publishTask, err := s.DB.EnqueueTask(dbx.TaskPublish, source.ID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE tasks SET state='failed' WHERE id=?", publishTask); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.RetryTask(publishTask); !errors.Is(err, dbx.ErrPermanentEncrypted) {
		t.Fatal("a forbidden permanent publish task could be retried", err)
	}
}
