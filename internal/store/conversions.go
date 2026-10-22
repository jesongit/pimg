package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/ids"
)

func (s *Service) QueueConversion(ctx context.Context, sourceID string, encrypted bool, operationIDs ...int64) (*dbx.Conversion, error) {
	unlock, err := s.lockFile(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if moving, err := s.DB.TelegramMoveActive(sourceID); err != nil {
		return nil, err
	} else if moving {
		return nil, dbx.ErrTelegramMoving
	}
	source, err := s.DB.GetVisibleFile(sourceID)
	if err != nil {
		return nil, err
	}
	var policies []dbx.MediaReplica
	current, existingErr := s.DB.GetConversion(sourceID)
	if existingErr == nil && (current.Kind != dbx.TaskConvert || current.Phase == "done" || current.TargetEncrypted != encrypted) {
		return nil, dbx.ErrConverting
	}
	if existingErr == nil {
		// Resume with the accepted policy; never reinterpret a running conversion.
		policies, err = s.DB.MediaReplicas(current.TargetID)
	} else if !errors.Is(existingErr, dbx.ErrNotFound) {
		return nil, existingErr
	} else {
		policies, err = s.conversionPolicies(source, encrypted)
	}
	if err != nil {
		return nil, err
	}
	wrapped, nonce, err := s.Master.WrapFileKey(cryptox.GenerateFileKey())
	if err != nil {
		return nil, err
	}
	var operationID int64
	if len(operationIDs) > 0 {
		operationID = operationIDs[0]
	}
	conversion, err := s.DB.BeginConversionInOperation(sourceID, ids.New(), encrypted, wrapped, nonce, cryptox.CryptoVer, operationID, policies...)
	if err == nil {
		if f, e := s.DB.GetFile(sourceID); e == nil {
			s.ForgetMediaViews(f)
		}
	}
	return conversion, err
}

func (s *Service) lockConversion(ctx context.Context, sourceID, targetID string) (func(), error) {
	keys := []string{sourceID, targetID}
	sort.Strings(keys)
	var releases []func()
	for _, id := range keys {
		release, err := s.lockFile(ctx, id)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}, nil
}

// RunConversion verifies a separate hidden replacement before atomically
// retiring its source. Retired copies are cleaned by an independent task.
func (s *Service) RunConversion(ctx context.Context, sourceID string) (newFileID string, retErr error) {
	c, err := s.DB.GetConversion(sourceID)
	if err != nil {
		return "", err
	}
	if c.Kind != dbx.TaskConvert {
		return "", dbx.ErrConverting
	}
	if c.Phase == "done" {
		return c.TargetID, nil
	}
	unlock, err := s.lockConversion(ctx, sourceID, c.TargetID)
	if err != nil {
		return "", err
	}
	defer unlock()
	for _, id := range []string{sourceID, c.TargetID} {
		if moving, err := s.DB.TelegramMoveActive(id); err != nil {
			return "", err
		} else if moving {
			return "", dbx.ErrTelegramMoving
		}
	}
	c, err = s.DB.GetConversion(sourceID)
	if err != nil {
		return "", err
	}
	if c.Phase == "done" {
		return c.TargetID, nil
	}
	defer func() {
		if retErr != nil {
			s.DB.ConversionError(sourceID, retErr.Error())
		}
	}()
	source, err := s.DB.GetFile(sourceID)
	if err != nil {
		return "", err
	}
	target, err := s.DB.GetFile(c.TargetID)
	if err != nil {
		return "", err
	}
	if source.Hidden || source.PurgeState.String != "" || !target.Hidden || target.PurgeState.String != "" {
		return "", dbx.ErrConverting
	}
	if source.MediaVersion == 2 {
		if source.OwnerID != target.OwnerID || target.MediaVersion != 2 {
			return "", dbx.ErrConverting
		}
		if err = s.DB.ConversionPhase(sourceID, "archive"); err != nil {
			return "", err
		}
		if err = s.copyMediaReplacement(ctx, source, target); err != nil {
			return "", conversionFailure("写入并校验副本", err)
		}
		if _, err = s.Verify(ctx, target.ID); err != nil {
			return "", conversionFailure("校验新归档", err)
		}
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if err = s.DB.CompleteConversion(sourceID); err != nil {
			return "", err
		}
		return target.ID, nil
	}
	if c.Phase == "prepare" {
		if err := s.prepareConversionOriginal(ctx, source, target); err != nil {
			return "", conversionFailure("恢复原件", err)
		}
		if err := s.DB.ConversionPhase(sourceID, "archive"); err != nil {
			return "", err
		}
		c.Phase = "archive"
	}
	if c.Phase == "archive" {
		if c.TargetEncrypted {
			err = s.runEncryptedArchive(ctx, target)
		} else {
			err = s.runDirectArchive(ctx, target, false)
		}
		if err != nil {
			return "", conversionFailure("写入新归档", err)
		}
	}
	// Only a verified replacement can become visible.
	if _, err := s.Verify(ctx, target.ID); err != nil {
		return "", conversionFailure("校验新归档", err)
	}
	if err := s.DB.CompleteConversion(sourceID); err != nil {
		return "", err
	}
	return target.ID, nil
}

func conversionFailure(stage string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// API/task errors do not include remote URLs, credentials or file keys.
	return fmt.Errorf("转换%s未完成，已保留恢复数据和清理定位，请重试", stage)
}

func (s *Service) prepareConversionOriginal(ctx context.Context, source, target *dbx.File) error {
	plain := s.plainPath(target.ID)
	if existing, err := os.Open(plain); err == nil {
		hash := sha256.New()
		size, readErr := io.Copy(hash, existing)
		existing.Close()
		if readErr == nil && size == source.Size && hex.EncodeToString(hash.Sum(nil)) == source.SHA256 {
			return nil
		}
	}
	input, err := s.SourceStream(ctx, source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(s.upDir, target.ID+".convert-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	hash := sha256.New()
	size, copyErr := io.Copy(output, io.TeeReader(conversionReader{ctx: ctx, source: input}, hash))
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if size != source.Size || hex.EncodeToString(hash.Sum(nil)) != source.SHA256 {
		return errors.New("恢复原件与索引大小或哈希不一致")
	}
	if err := os.Remove(plain); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(output.Name(), plain)
}

type conversionReader struct {
	ctx    context.Context
	source io.Reader
}

func (r conversionReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

func (s *Service) RunConvertedCleanup(ctx context.Context, sourceID string) error {
	unlock, err := s.lockFile(ctx, sourceID)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := s.DB.GetConversion(sourceID)
	if err != nil {
		return err
	}
	if c.Phase != "done" {
		return dbx.ErrConverting
	}
	if !c.CleanupPending {
		return nil
	}
	source, err := s.DB.GetFile(sourceID)
	if err != nil {
		return err
	}
	if !source.Hidden {
		return errors.New("旧归档尚未退役，已停止残留清理")
	}
	if err := s.cleanupConversionSource(ctx, source); err != nil {
		failure := conversionFailure("清理旧副本", err)
		if c.Kind == dbx.TaskTransferOwner && ctx.Err() == nil {
			failure = errors.New("归属已转移，旧副本清理未完成，剩余清理定位已保留，请重试清理任务")
		}
		s.DB.ConversionCleanup(sourceID, true, failure.Error())
		return failure
	}
	return s.DB.ConversionCleanup(sourceID, false, "")
}

func (s *Service) cleanupConversionSource(ctx context.Context, source *dbx.File) error {
	s.ForgetMediaViews(source)
	if err := s.removePermanent(ctx, source); err != nil {
		return err
	}
	if err := s.DB.SetPublished(source.ID, false, "", ""); err != nil {
		return err
	}
	objects, err := s.DB.FileTempObjects(source.ID)
	if err != nil {
		return err
	}
	var failures []error
	for _, object := range objects {
		if err := s.deleteTemp(ctx, object); err != nil {
			failures = append(failures, err)
		} else if err := s.DB.MarkTempDeleted(object.ID); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	if source.MediaVersion == 2 {
		if err := s.purgeMedia(ctx, source, ""); err != nil {
			return err
		}
	} else {
		backend, err := s.cold(source)
		if err != nil {
			return err
		}
		for _, kind := range []string{"data", "thumb", "bthumb"} {
			chunks, err := s.DB.ListChunks(source.ID, kind)
			if err != nil {
				return err
			}
			for _, chunk := range chunks {
				if err := backend.Delete(ctx, chunkRef(chunk)); err != nil {
					failures = append(failures, err)
				} else if err := s.DB.DeleteChunk(chunk.ID); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	for _, path := range []string{s.plainPath(source.ID), s.ctPath(source.ID), s.ctDonePath(source.ID)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
