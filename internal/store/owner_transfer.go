package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/ids"
)

func (s *Service) QueueOwnerTransfer(ctx context.Context, sourceID string, albumID, operationID int64) (*dbx.Conversion, error) {
	unlock, err := s.lockFile(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	wrapped, nonce, err := s.Master.WrapFileKey(cryptox.GenerateFileKey())
	if err != nil {
		return nil, err
	}
	c, err := s.DB.BeginOwnerTransfer(sourceID, ids.New(), albumID, operationID, wrapped, nonce, cryptox.CryptoVer)
	if err == nil {
		if f, e := s.DB.GetFile(sourceID); e == nil {
			s.ForgetMediaViews(f)
		}
	}
	return c, err
}

func transferFailure(stage string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("转移归属%s未完成，原文件与恢复进度已保留，请重试", stage)
}

func (s *Service) RunOwnerTransfer(ctx context.Context, sourceID string) (targetID string, retErr error) {
	c, err := s.DB.GetConversion(sourceID)
	if err != nil {
		return "", err
	}
	if c.Kind != dbx.TaskTransferOwner {
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
	c, err = s.DB.GetConversion(sourceID)
	if err != nil {
		return "", err
	}
	if c.Phase == "done" {
		return c.TargetID, nil
	}
	defer func() {
		if retErr != nil {
			_ = s.DB.ConversionError(sourceID, retErr.Error())
		}
	}()
	source, err := s.DB.GetVisibleFile(sourceID)
	if err != nil {
		return "", err
	}
	target, err := s.DB.GetFile(c.TargetID)
	if err != nil {
		return "", err
	}
	if source.PurgeState.String != "" || !target.Hidden || target.PurgeState.String != "" {
		return "", dbx.ErrConverting
	}
	if !s.DB.AccountActive(target.OwnerID) || !s.DB.AccountActive(source.OwnerID) {
		return "", errors.New("相关用户已停用，原文件归属保持不变")
	}
	if source.MediaVersion == 2 {
		// Preserve each independently stored representation. Reconstructing a
		// split video would remux its container and could change its bytes/hash.
		if err = s.DB.ConversionPhase(sourceID, "archive"); err != nil {
			return "", err
		}
		if err = s.copyMediaReplacement(ctx, source, target); err != nil {
			return "", transferFailure("写入并校验副本", err)
		}
	} else {
		if c.Phase == "prepare" {
			if err = s.prepareConversionOriginal(ctx, source, target); err != nil {
				return "", transferFailure("读取原件", err)
			}
			if err = s.DB.ConversionPhase(sourceID, "archive"); err != nil {
				return "", err
			}
		}
		if c.TargetEncrypted {
			err = s.runEncryptedArchive(ctx, target)
		} else {
			err = s.runDirectArchive(ctx, target, false)
		}
		if err != nil {
			return "", transferFailure("写入副本", err)
		}
	}
	if _, err = s.Verify(ctx, target.ID); err != nil {
		return "", transferFailure("校验新归档", err)
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = s.DB.CompleteConversion(sourceID); err != nil {
		return "", err
	}
	s.ForgetMediaViews(source)
	return target.ID, nil
}

func (s *Service) copyMediaReplacement(ctx context.Context, source, target *dbx.File) error {
	if err := s.DB.SetArchiveState(target.ID, "running", ""); err != nil {
		return err
	}
	if preview, err := os.ReadFile(s.previewPath(source)); err == nil {
		if err = s.saveLocalPreview(target, preview); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	replicas, err := s.DB.MediaReplicas(source.ID)
	if err != nil {
		return err
	}
	persisted, err := s.DB.MediaReplicas(target.ID)
	if err != nil {
		return err
	}
	copied := 0
	for _, r := range replicas {
		if r.State != "ready" {
			continue
		}
		if len(r.Parts) == 0 {
			return errors.New("来源归档缺少分片")
		}
		destination := r
		destination.FileID = target.ID
		destination.Parts = nil
		for _, policy := range persisted {
			if policy.InstanceID == r.InstanceID {
				destination.Encrypted = policy.Encrypted
				destination.Split = policy.Split
				destination.PartMB = policy.PartMB
				break
			}
		}
		if err = s.DB.EnsureMediaReplica(destination); err != nil {
			return err
		}
		if err = s.DB.SetMediaReplicaState(target.ID, r.InstanceID, "running", ""); err != nil {
			return err
		}
		existing, err := s.DB.MediaReplicas(target.ID)
		if err != nil {
			return err
		}
		for index, p := range r.Parts {
			if !p.Verified || p.Index != index || p.Count != len(r.Parts) || p.Size < 0 || p.SHA256 == "" {
				return errors.New("来源分片尚未校验或布局不完整")
			}
			kept := false
			for _, oldReplica := range existing {
				if oldReplica.InstanceID == r.InstanceID {
					for _, old := range oldReplica.Parts {
						if old.Index == p.Index && old.Verified && old.SHA256 == p.SHA256 && old.Size == p.Size && old.Count == p.Count && s.verifyMediaPart(ctx, target, destination, old) == nil {
							kept = true
							break
						}
					}
				}
			}
			if kept {
				continue
			}
			if err = s.copyOwnerPart(ctx, source, target, r, destination, p); err != nil {
				_ = s.DB.SetMediaReplicaState(target.ID, r.InstanceID, "failed", "副本写入未完成，恢复定位已保留")
				return err
			}
		}
		if err = s.DB.SetMediaReplicaState(target.ID, r.InstanceID, "ready", ""); err != nil {
			return err
		}
		copied++
	}
	if copied == 0 {
		return errors.New("来源没有已就绪的归档")
	}
	return s.DB.SetArchiveState(target.ID, "done", "")
}

func (s *Service) copyOwnerPart(ctx context.Context, source, target *dbx.File, from, to dbx.MediaReplica, p dbx.MediaPart) error {
	input, err := s.openMediaPart(ctx, source, from, p)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(s.upDir, target.ID+".transfer-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(conversionReader{ctx: ctx, source: input}, p.Size+1))
	if err != nil {
		return err
	}
	if n != p.Size || hex.EncodeToString(hash.Sum(nil)) != p.SHA256 {
		return errors.New("来源分片大小或哈希不一致")
	}
	if err = output.Close(); err != nil {
		return err
	}
	// Storage references and carrier checksums belong exclusively to the new
	// account/file/key. Never reuse a source deletion locator.
	p.Reference = ""
	p.Verified = false
	p.LocalModNS = 0
	p.PayloadSize = 0
	p.StoredSize = 0
	p.PayloadSHA256 = ""
	_, err = s.writeMediaPart(ctx, target, to, p, output.Name())
	return err
}
