package store

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"strings"
)

const TaskLocalLayout = "local_layout"

func (s *Service) QueueLocalLayouts() error {
	if s.Cfg.Paths.LegacyDir == "" {
		return nil
	}
	rows, err := s.DB.Query(`SELECT DISTINCT f.id FROM files f JOIN archive_objects a ON a.file_id=f.id JOIN storage_instances i ON i.id=a.instance_id WHERE i.driver='local' AND f.archive_state='done' AND f.purge_state IS NULL AND json_extract(a.reference,'$.Key') LIKE 'albums/%' AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.type='local_layout' AND t.file_id=f.id AND t.state IN ('pending','running','failed')) LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []string
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
	return s.DB.EnqueueMaintenanceTasks(TaskLocalLayout, ids)
}

// Migration commits the new locator only after byte-preserving copy and full
// payload verification. The old object remains recorded for eventual cleanup.
func (s *Service) MigrateLocalArchiveFile(ctx context.Context, id string) error {
	unlock, err := s.lockFile(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.mutableFile(id)
	if err != nil {
		return err
	}
	if f.ArchiveState != "done" {
		return errors.New("原件归档未完成")
	}
	objects, err := s.DB.ArchiveObjects(id, "")
	if err != nil {
		return err
	}
	chunks, err := s.DB.ListChunks(id, "data")
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err = ctx.Err(); err != nil {
			return err
		}
		instance, e := s.instance(object.InstanceID)
		if e != nil {
			return e
		}
		local, ok := instance.cold.(storage.LocalCold)
		if !ok {
			continue
		}
		old := objectReference(object)
		if !strings.HasPrefix(old.Key, "albums/") {
			continue
		}
		var chunk *dbx.Chunk
		for _, c := range chunks {
			if c.Kind == object.Kind && c.Idx == object.Idx {
				chunk = c
				break
			}
		}
		if chunk == nil {
			return errors.New("旧归档缺少分片记录，保留原位置")
		}
		key := managedArchiveKey(f, object.Kind, object.Idx)
		// Open through the constrained storage root before using the migration copier.
		input, e := local.Open(ctx, old)
		if e != nil {
			return e
		}
		input.Close()
		if e = datalayout.CopyTreeContext(ctx, filepath.Join(local.Disk.Dir, filepath.FromSlash(old.Key)), filepath.Join(local.Disk.Dir, filepath.FromSlash(key))); e != nil {
			return e
		}
		wire, e := local.Open(ctx, storage.Reference{Key: key})
		if e != nil {
			return e
		}
		payload, e := s.archivePayload(f, chunk, object.InstanceID, wire)
		if e != nil {
			wire.Close()
			return e
		}
		hash := sha256New()
		n, e := io.Copy(hash, s.idleReader(ctx, payload))
		e = errors.Join(e, payload.Close())
		if e != nil {
			return e
		}
		if n != chunk.Size || hexOf(hash) != chunk.SHA256 {
			return errors.New("迁移目标原件校验失败，仍使用旧位置")
		}
		next := old
		next.Key = key
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		_, e = tx.Exec(`INSERT INTO archive_garbage(file_id,instance_id,reference) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, object.InstanceID, object.Reference)
		if e == nil {
			_, e = tx.Exec(`UPDATE archive_objects SET reference=?,album_id=? WHERE file_id=? AND instance_id=? AND kind=? AND idx=? AND reference=?`, referenceJSON(next), f.AlbumID.Int64, id, object.InstanceID, object.Kind, object.Idx, object.Reference)
		}
		if e == nil && f.ColdBackend == object.InstanceID {
			_, e = tx.Exec(`UPDATE file_chunks SET object_key=? WHERE file_id=? AND kind=? AND idx=? AND object_key=?`, key, id, object.Kind, object.Idx, old.Key)
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}
