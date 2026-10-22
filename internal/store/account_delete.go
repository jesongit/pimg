package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"pimg/internal/dbx"
)

var ErrAccountDeleteWaiting = errors.New("等待用户文件任务停止后继续删除")

func (s *Service) RunAccountDelete(ctx context.Context, owner string) error {
	if owner == dbx.AdminOwner || owner == "" {
		return errors.New("不能删除管理员账号")
	}
	var deleting bool
	err := s.DB.QueryRow(`SELECT deleting FROM accounts WHERE id=?`, owner).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !deleting {
		return errors.New("用户没有提交删除")
	}
	if err = s.DB.CancelDeletingAccountFileTasks(owner); err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT id FROM files WHERE owner_id=? ORDER BY hidden,id LIMIT 10`, owner)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.RunPurgeFile(ctx, id); err != nil {
			if errors.Is(err, dbx.ErrTaskReferenced) {
				var running int
				if e := s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE file_id=? AND state IN ('pending','running')`, id).Scan(&running); e == nil && running > 0 {
					return ErrAccountDeleteWaiting
				}
			}
			return fmt.Errorf("删除文件 %s 失败，账号与清理进度已保留: %w", id, err)
		}
	}
	if len(ids) > 0 {
		return ErrAccountDeleteWaiting
	}
	return s.DB.FinishAccountDelete(owner)
}
