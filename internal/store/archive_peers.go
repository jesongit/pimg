package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"pimg/internal/dbx"
	"pimg/internal/storage"
	"time"
)

func managedArchiveKey(f *dbx.File, kind string, idx int) string {
	if kind == "data" && idx == 0 {
		return originalArchiveKey(f)
	}
	return fmt.Sprintf("accounts/%s/albums/%d/%s/%s-%04d.bin", dbx.OwnerDirectory(f.OwnerID), f.AlbumID.Int64, f.ID, kind, idx)
}
func referenceJSON(r storage.Reference) string { return dbx.JSONValue(r) }
func objectReference(o dbx.ArchiveObject) storage.Reference {
	var r storage.Reference
	_ = json.Unmarshal([]byte(o.Reference), &r)
	return r
}
func (s *Service) allChunks(id string) ([]*dbx.Chunk, error) {
	return s.DB.ListChunks(id, "data")
}
func (s *Service) completeArchive(ctx context.Context, f *dbx.File) error {
	if f.MediaVersion == 2 {
		return s.queueMediaSync(f)
	}
	var adopted bool
	if e := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM uploader_files WHERE canonical_id=?)`, f.ID).Scan(&adopted); e != nil {
		return e
	}

	cs, e := s.allChunks(f.ID)
	if e != nil {
		return e
	}
	b, e := s.cold(f)
	if e != nil {
		return e
	}
	objects, e := s.DB.ArchiveObjects(f.ID, f.ColdBackend)
	if e != nil {
		return e
	}
	if e = s.repairCanonicalChunks(f, cs, objects); e != nil {
		return e
	}
	for _, c := range cs {
		if c.Kind != "data" && !adopted {
			r, err := b.Open(ctx, chunkRef(c))
			if err != nil {
				return err
			}
			h := sha256New()
			n, err := io.Copy(h, r)
			r.Close()
			if err != nil {
				return err
			}
			if n != c.Size || hexOf(h) != c.SHA256 {
				return errors.New("归档缩略图校验失败")
			}
		}
		if e = s.DB.SaveArchiveObject(dbx.ArchiveObject{AlbumID: f.AlbumID.Int64, FileID: f.ID, InstanceID: f.ColdBackend, Kind: c.Kind, Idx: c.Idx, Reference: referenceJSON(chunkRef(c)), Size: c.Size, SHA256: c.SHA256, Verified: true}); e != nil {
			return e
		}
	}
	var revision string
	if e = s.DB.QueryRow(`SELECT CAST(revision AS TEXT) FROM cold_sync_revisions WHERE file_id=?`, f.ID).Scan(&revision); e != nil {
		return e
	}
	fresh, e := s.DB.GetFile(f.ID)
	if e != nil {
		return e
	}
	if e = s.saveArchiveManifest(ctx, fresh, f.ColdBackend, revision, len(cs)); e != nil {
		return e
	}
	for _, target := range s.StoragePolicy().Archives {
		if target != f.ColdBackend {
			if _, e = s.DB.EnqueueUniqueTask(dbx.TaskArchiveSync, f.ID, "{}"); e != nil {
				return e
			}
			break
		}
	}

	return nil
}

func (s *Service) ReconcileStorage() error {
	rowsV2, errV2 := s.DB.Query(`SELECT f.id FROM files f WHERE f.media_version=2 AND f.archive_state='done' AND f.hidden=0 AND f.purge_state IS NULL AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.file_id=f.id AND t.type='archive_sync' AND t.state IN ('pending','running','failed','canceled')) AND EXISTS(SELECT 1 FROM json_each(?) target WHERE NOT EXISTS(SELECT 1 FROM media_replicas r WHERE r.file_id=f.id AND r.instance_id=target.value AND r.state='ready')) LIMIT 100`, dbx.JSONValue(s.StoragePolicy().Archives))
	if errV2 != nil {
		return errV2
	}
	var mediaIDs []string
	for rowsV2.Next() {
		var id string
		if errV2 = rowsV2.Scan(&id); errV2 != nil {
			rowsV2.Close()
			return errV2
		}
		mediaIDs = append(mediaIDs, id)
	}
	errV2 = rowsV2.Err()
	rowsV2.Close()
	if errV2 != nil {
		return errV2
	}
	for _, id := range mediaIDs {
		if _, errV2 = s.DB.EnqueueUniqueTask(dbx.TaskArchiveSync, id, "{}"); errV2 != nil {
			return errV2
		}
	}
	if err := s.QueueLocalLayouts(); err != nil {
		return err
	}
	p := s.StoragePolicy()
	rows, e := s.DB.Query(`SELECT f.id FROM files f JOIN cold_sync_revisions r ON r.file_id=f.id WHERE f.media_version=0 AND f.archive_state='done' AND f.hidden=0 AND f.purge_state IS NULL AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.file_id=f.id AND t.type='archive_sync' AND t.state IN ('pending','running','failed','canceled')) AND EXISTS(SELECT 1 FROM json_each(?) target WHERE NOT EXISTS(SELECT 1 FROM archive_manifests m WHERE m.file_id=f.id AND m.instance_id=target.value AND m.revision=CAST(r.revision AS TEXT))) ORDER BY f.id LIMIT 100`, dbx.JSONValue(p.Archives))
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = s.DB.EnqueueUniqueTask(dbx.TaskArchiveSync, id, "{}"); e != nil {
			return e
		}
	}
	return nil
}

// One object is spooled, read from the source once, then sent to each missing
// target in sequence. Verified progress is durable; completed objects are not
// downloaded again after interruption. The shared idle worker bounds concurrency.
func (s *Service) RunArchiveSync(ctx context.Context, id string) error {
	unlock, e := s.lockFile(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	f, e := s.mutableFile(id)
	if e != nil {
		return e
	}
	if f.ArchiveState != "done" {
		return errors.New("等待归档完成")
	}
	if f.MediaVersion == 2 {
		return s.runMediaSync(ctx, f)
	}
	cs, e := s.allChunks(id)
	if e != nil {
		return e
	}
	objects, e := s.DB.ArchiveObjects(id, "")
	if e != nil {
		return e
	}
	if e = s.repairCanonicalChunks(f, cs, objects); e != nil {
		return e
	}
	p := s.StoragePolicy()
	failed := map[string]bool{}
	var errs []error
	known := map[string]dbx.ArchiveObject{}
	for _, o := range objects {
		known[fmt.Sprintf("%s/%s/%d", o.InstanceID, o.Kind, o.Idx)] = o
	}
	for _, c := range cs {
		missing := []string{}
		for _, target := range p.Archives {
			if failed[target] {
				continue
			}
			key := fmt.Sprintf("%s/%s/%d", target, c.Kind, c.Idx)
			o, ok := known[key]
			if !ok || !o.Verified || o.SHA256 != c.SHA256 || o.Size != c.Size || o.AlbumID != f.AlbumID.Int64 {
				missing = append(missing, target)
			}
		}
		if len(missing) == 0 {
			continue
		}
		if c.Size > int64(p.SyncCacheMB)<<20 {
			// Larger originals stream one target at a time without exceeding disk cache.
			for _, target := range missing {
				err := s.syncStreamingOriginal(ctx, f, c, target, objects)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					failed[target] = true
					errs = append(errs, fmt.Errorf("归档 %s: %w", target, err))
				}
			}
			continue
		}
		spool, e := os.CreateTemp(s.ctDir, "peer-*.part")
		if e != nil {
			return e
		}
		e = s.spoolArchiveChunk(ctx, f, c, objects, spool)
		if e == nil {
			for _, target := range missing {
				if _, e = spool.Seek(0, io.SeekStart); e != nil {
					break
				}
				if e = s.syncObject(ctx, f, c, target, spool); e != nil {
					if ctx.Err() != nil {
						break
					}
					failed[target] = true
					errs = append(errs, fmt.Errorf("归档 %s: %w", target, e))
				}
			}
			if ctx.Err() == nil {
				e = nil
			}
		}
		spool.Close()
		os.Remove(spool.Name())
		if e != nil {
			return e
		}
	}
	var revision string
	if e = s.DB.QueryRow(`SELECT CAST(revision AS TEXT) FROM cold_sync_revisions WHERE file_id=?`, id).Scan(&revision); e != nil {
		return e
	}
	fresh, e := s.DB.GetFile(id)
	if e != nil {
		return e
	}
	for _, target := range p.Archives {
		if failed[target] {
			continue
		}
		if e = s.saveArchiveManifest(ctx, fresh, target, revision, len(cs)); e != nil {
			errs = append(errs, fmt.Errorf("归档 %s 清单: %w", target, e))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) syncStreamingOriginal(ctx context.Context, f *dbx.File, c *dbx.Chunk, target string, objects []dbx.ArchiveObject) error {
	ids := append(s.rankedArchives(), f.ColdBackend)
	for _, o := range objects {
		if o.Verified {
			ids = append(ids, o.InstanceID)
		}
	}
	seen := map[string]bool{}
	var last error
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		input, err := s.openPeerChunk(context.WithValue(ctx, archiveSourceContext{}, id), f, c)
		if err == nil {
			err = s.syncObject(ctx, f, c, target, input)
			input.Close()
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有可用的同步源")
	}
	return last
}

func (s *Service) repairCanonicalChunks(f *dbx.File, chunks []*dbx.Chunk, objects []dbx.ArchiveObject) error {
	for _, o := range objects {
		if o.InstanceID != f.ColdBackend || !o.Verified || o.AlbumID != f.AlbumID.Int64 {
			continue
		}
		for _, c := range chunks {
			if c.Kind != o.Kind || c.Idx != o.Idx || c.SHA256 != o.SHA256 || c.Size != o.Size || referenceJSON(chunkRef(c)) == o.Reference {
				continue
			}
			ref := objectReference(o)
			clone := *c
			clone.ObjectKey = ref.Key
			clone.MessageID = sql.NullInt64{Int64: ref.MessageID, Valid: ref.MessageID != 0}
			clone.TgFileID = sql.NullString{String: ref.FileID, Valid: ref.FileID != ""}
			if err := s.DB.UpsertChunk(&clone); err != nil {
				return err
			}
			*c = clone
		}
	}
	return nil
}

// A damaged preferred source must not strand replication while another complete
// copy is available. Retry sources sequentially, reusing the same bounded spool.
func (s *Service) spoolArchiveChunk(ctx context.Context, f *dbx.File, c *dbx.Chunk, objects []dbx.ArchiveObject, spool *os.File) error {
	available := map[string]bool{f.ColdBackend: true}
	targets := append(s.rankedArchives(), f.ColdBackend)
	for _, o := range objects {
		if o.Verified && o.Kind == c.Kind && o.Idx == c.Idx && o.SHA256 == c.SHA256 {
			available[o.InstanceID] = true
			targets = append(targets, o.InstanceID)
		}
	}
	seen := map[string]bool{}
	var last error
	for _, target := range targets {
		if seen[target] || !available[target] {
			continue
		}
		seen[target] = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := spool.Truncate(0); err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		started := time.Now()
		input, err := s.openPeerChunk(context.WithValue(ctx, archiveSourceContext{}, target), f, c)
		if err != nil {
			last = err
			continue
		}
		h := sha256New()
		n, err := io.Copy(io.MultiWriter(spool, h), s.idleReader(ctx, io.LimitReader(input, c.Size+1)))
		input.Close()
		if err == nil && (n != c.Size || hexOf(h) != c.SHA256) {
			err = errors.New("同步源分片校验失败")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.recordStorageResult(target, n, started, err)
		if err == nil {
			return nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有可用的同步源")
	}
	return last
}
func (s *Service) saveArchiveManifest(ctx context.Context, f *dbx.File, target, revision string, chunkCount int) error {
	objects, e := s.DB.ArchiveObjects(f.ID, target)
	if e != nil {
		return e
	}
	chunks, e := s.allChunks(f.ID)
	if e != nil {
		return e
	}
	if len(chunks) != chunkCount {
		return errors.New("归档副本尚不完整")
	}
	for _, c := range chunks {
		matched := false
		for _, o := range objects {
			if o.Kind == c.Kind && o.Idx == c.Idx && o.Verified && o.Size == c.Size && o.SHA256 == c.SHA256 && o.AlbumID == f.AlbumID.Int64 {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("归档副本定位或校验版本已变化，等待重新同步")
		}
	}
	// Completion is a database receipt only, never a remote metadata file.
	_, e = s.DB.Exec(`INSERT INTO archive_manifests(file_id,instance_id,reference,revision) VALUES(?,?,'',?) ON CONFLICT(file_id,instance_id) DO UPDATE SET revision=excluded.revision`, f.ID, target, revision)
	if e != nil {
		return e
	}
	return s.cleanupArchiveGarbage(ctx, f.ID)
}

func (s *Service) archiveBackend(id string) (storage.Cold, error) {
	if id == "local" && s.coldBackends[id] != nil {
		return s.coldBackends[id], nil
	}
	if id == "telegram" {
		if s.UserTG != nil {
			return topicCold{s: s, instance: id}, nil
		}
		if s.coldBackends[id] != nil {
			return s.coldBackends[id], nil
		}
	}

	i, e := s.instance(id)
	if e != nil {
		return nil, e
	}
	if i.cold == nil {
		return nil, errors.New("归档存储未配置")
	}
	return i.cold, nil
}
func (s *Service) putArchiveReader(ctx context.Context, b storage.Cold, key string, r io.Reader, n int64) (storage.Reference, error) {
	if stream, ok := b.(storage.StreamCold); ok {
		return stream.PutReader(ctx, key, r, n, "pimg-managed")
	}
	if n > 32<<20 {
		return storage.Reference{}, errors.New("该适配器不支持大对象流式写入")
	}
	data, e := io.ReadAll(io.LimitReader(r, n+1))
	if e != nil {
		return storage.Reference{}, e
	}
	if int64(len(data)) != n {
		return storage.Reference{}, errors.New("归档对象长度不符")
	}
	return b.Put(ctx, key, data, "pimg-managed")
}
func (s *Service) syncObject(ctx context.Context, f *dbx.File, c *dbx.Chunk, target string, source io.Reader) (ret error) {
	start := time.Now()
	defer func() { s.recordStorageResult(target, c.Size, start, ret) }()
	b, e := s.archiveBackend(target)
	if e != nil {
		ret = e
		return e
	}

	// Deterministic object stores persist the cleanup locator before PUT. A lost
	// acknowledgment therefore remains both resumable and removable.
	key := managedArchiveKey(f, c.Kind, c.Idx)
	if oldObjects, err := s.DB.ArchiveObjects(f.ID, target); err != nil {
		return err
	} else {
		for _, old := range oldObjects {
			if old.Kind == c.Kind && old.Idx == c.Idx && old.AlbumID != f.AlbumID.Int64 {
				if _, err = s.DB.Exec(`INSERT OR IGNORE INTO archive_garbage(file_id,instance_id,reference) VALUES(?,?,?)`, f.ID, target, old.Reference); err != nil {
					return err
				}
			}
		}
	}

	if planner, ok := b.(storage.PlannedCold); ok {
		planned := planner.PlanReference(key)
		if e = s.DB.SaveArchiveObject(dbx.ArchiveObject{AlbumID: f.AlbumID.Int64, FileID: f.ID, InstanceID: target, Kind: c.Kind, Idx: c.Idx, Reference: referenceJSON(planned), Size: c.Size, SHA256: c.SHA256}); e != nil {
			ret = e
			return e
		}
	}

	var ref storage.Reference
	ref, e = s.writeArchiveOriginal(ctx, b, f, c, s.idleReader(ctx, source))
	if e != nil {
		ret = e
		return e
	}
	o := dbx.ArchiveObject{AlbumID: f.AlbumID.Int64, FileID: f.ID, InstanceID: target, Kind: c.Kind, Idx: c.Idx, Reference: referenceJSON(ref), Size: c.Size, SHA256: c.SHA256}
	if e = s.DB.SaveArchiveObject(o); e != nil {
		ret = e
		return e
	}
	r, e := b.Open(ctx, ref)
	if e == nil {
		r, e = s.archivePayload(f, c, target, r)
	}
	if e != nil {
		ret = e
		return e
	}
	h := sha256New()
	n, e := io.Copy(h, s.idleReader(ctx, io.LimitReader(r, c.Size+1)))
	r.Close()
	if e == nil && (n != c.Size || hexOf(h) != c.SHA256) {
		e = errors.New("目标归档分片校验失败")
	}
	if e != nil {
		ret = e
		return e
	}
	o.Verified = true
	ret = s.DB.SaveArchiveObject(o)
	if ret != nil {
		return ret
	}
	if target == f.ColdBackend {
		clone := *c
		clone.ObjectKey = ref.Key
		clone.MessageID = sql.NullInt64{Int64: ref.MessageID, Valid: ref.MessageID != 0}
		clone.TgFileID = sql.NullString{String: ref.FileID, Valid: ref.FileID != ""}
		ret = s.DB.UpsertChunk(&clone)
		*c = clone
	}
	return ret
}

type archiveSourceContext struct{}

// Verification must succeed from one complete instance. Do not authorize source
// deletion using a stream assembled from incomplete replicas on different stores.
func (s *Service) verifyArchivedOriginal(ctx context.Context, f *dbx.File, paced bool) (string, error) {
	if f.MediaVersion == 2 {
		return s.verifyMedia(ctx, f)
	}
	started := time.Now()
	ids := append(s.rankedArchives(), f.ColdBackend)
	objects, err := s.DB.ArchiveObjects(f.ID, "")
	if err != nil {
		return "", err
	}
	for _, o := range objects {
		if o.Verified {
			ids = append(ids, o.InstanceID)
		}
	}
	if forced, _ := ctx.Value(archiveSourceContext{}).(string); forced != "" {
		ids = []string{forced}
	}
	seen := map[string]bool{}
	var last error
	var result string
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		attempt := time.Now()
		rc, err := s.SourceStream(context.WithValue(ctx, archiveSourceContext{}, id), f)
		if err != nil {
			last = err
			continue
		}
		var reader io.Reader = io.LimitReader(contextReaderForCache{ctx, rc}, f.Size+1)
		if paced {
			reader = s.idleReader(ctx, reader)
		}
		h := sha256New()
		n, err := io.Copy(h, reader)
		err = errors.Join(err, rc.Close())
		got := hexOf(h)
		if err == nil && (n != f.Size || got != f.SHA256) {
			err = errors.New("恢复校验不一致（大小或哈希不匹配）")
		}
		res, _ := json.Marshal(map[string]any{"ok": err == nil, "size": n, "sha256": got, "want_sha256": f.SHA256, "want_size": f.Size, "source": id, "seconds": time.Since(started).Round(time.Millisecond).Seconds()})
		result = string(res)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		s.recordStorageResult(id, n, attempt, err)
		if err == nil {
			return result, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有可完整恢复的归档副本")
	}
	return result, last
}

func (s *Service) openPeerChunk(ctx context.Context, f *dbx.File, c *dbx.Chunk) (io.ReadCloser, error) {
	forced, _ := ctx.Value(archiveSourceContext{}).(string)
	ids := s.rankedArchives()
	if forced != "" {
		ids = []string{forced}
	} else {
		ids = append(ids, f.ColdBackend)
	}
	seen := map[string]bool{}
	var last error
	objects, e := s.DB.ArchiveObjects(f.ID, "")
	if e != nil {
		return nil, e
	}
	for _, o := range objects {
		if forced == "" {
			ids = append(ids, o.InstanceID)
		}
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		b, e := s.archiveBackend(id)
		if e != nil {
			last = e
			continue
		}
		var ref storage.Reference
		found, current := false, false
		for _, o := range objects {
			if o.InstanceID == id && o.Kind == c.Kind && o.Idx == c.Idx && o.Verified && o.SHA256 == c.SHA256 {
				ref = objectReference(o)
				found = true
				current = o.AlbumID == f.AlbumID.Int64 && o.Size == c.Size
				break
			}
		}
		if id == f.ColdBackend && !current {
			ref = chunkRef(c)
			found = true
		}
		if !found {
			continue
		}
		start := time.Now()
		r, e := b.Open(ctx, ref)
		s.recordStorageResult(id, 0, start, e)
		if e == nil {
			r, e = s.archivePayload(f, c, id, r)
			if e == nil {
				return r, nil
			}
		}
		last = e
	}
	if last == nil {
		last = errors.New("没有可用的归档副本")
	}
	return nil, last
}

type pacedReader struct {
	ctx context.Context
	svc *Service
	r   io.Reader
}

func (s *Service) idleReader(ctx context.Context, r io.Reader) io.Reader {
	return &pacedReader{ctx: ctx, svc: s, r: r}
}
func (r *pacedReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	start := time.Now()
	n, e := r.r.Read(p)
	if n > 0 {
		policy := r.svc.StoragePolicy()
		rate := policy.NormalMB
		now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("15:04")
		if policy.Start != policy.End && ((policy.Start < policy.End && now >= policy.Start && now < policy.End) || (policy.Start > policy.End && (now >= policy.Start || now < policy.End))) {
			rate = policy.AcceleratedMB
		}
		if rate < 1 {
			rate = 1
		}
		wait := time.Duration(float64(n)/float64(rate<<20)*float64(time.Second)) - time.Since(start)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-r.ctx.Done():
				return n, r.ctx.Err()
			case <-timer.C:
			}
		}
	}
	return n, e
}
func (s *Service) purgeArchivePeers(ctx context.Context, f *dbx.File) error {
	if e := s.purgeManifestJobs(ctx, f); e != nil {
		return e
	}
	if e := s.cleanupArchiveGarbage(ctx, f.ID); e != nil {
		return e
	}
	objects, e := s.DB.ArchiveObjects(f.ID, "")
	if e != nil {
		return e
	}
	for _, o := range objects {
		b, e := s.archiveBackend(o.InstanceID)
		if e != nil {
			return e
		}
		if e = b.Delete(ctx, objectReference(o)); e != nil {
			return e
		}
		if _, e = s.DB.Exec(`DELETE FROM archive_objects WHERE file_id=? AND instance_id=? AND kind=? AND idx=?`, o.FileID, o.InstanceID, o.Kind, o.Idx); e != nil {
			return e
		}
	}
	rows, e := s.DB.Query(`SELECT instance_id,reference FROM archive_manifests WHERE file_id=?`, f.ID)
	if e != nil {
		return e
	}
	type manifest struct{ id, ref string }
	ms := []manifest{}
	for rows.Next() {
		var m manifest
		if e = rows.Scan(&m.id, &m.ref); e != nil {
			rows.Close()
			return e
		}
		ms = append(ms, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, m := range ms {
		if m.ref == "" {
			continue
		}
		b, e := s.archiveBackend(m.id)
		if e != nil {
			return e
		}
		var r storage.Reference
		if e = json.Unmarshal([]byte(m.ref), &r); e != nil {
			return e
		}
		if e = b.Delete(ctx, r); e != nil {
			return e
		}
		if _, e = s.DB.Exec(`DELETE FROM archive_manifests WHERE file_id=? AND instance_id=?`, f.ID, m.id); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) purgeManifestJobs(ctx context.Context, f *dbx.File) error {
	rows, err := s.DB.Query(`SELECT instance_id,revision,reference,body FROM archive_manifest_jobs WHERE file_id=?`, f.ID)
	if err != nil {
		return err
	}
	type job struct {
		instance, revision, reference string
		body                          []byte
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.instance, &j.revision, &j.reference, &j.body); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, err := s.archiveBackend(j.instance)
		if err != nil {
			return err
		}
		var ref storage.Reference
		if j.reference != "" {
			if err = json.Unmarshal([]byte(j.reference), &ref); err != nil {
				return err
			}
		} else {
			// A message-backed store may lose its response before returning a
			// locator. Replay its durable bytes/key to recover the same object.
			g := s.configCipher()
			if len(j.body) < g.NonceSize() {
				return errors.New("待清理归档清单损坏")
			}
			plain, err := g.Open(nil, j.body[:g.NonceSize()], j.body[g.NonceSize():], []byte("pimg/archive-manifest/v1"))
			if err != nil {
				return err
			}
			var saved struct {
				File dbx.File `json:"file"`
			}
			if err = json.Unmarshal(plain, &saved); err != nil || saved.File.ID != f.ID {
				return errors.New("待清理清单的文件标识不匹配")
			}
			key := managedArchiveKey(&saved.File, "manifest", 0) + "." + j.revision + ".enc"
			ref, err = s.putArchiveReader(ctx, b, key, bytes.NewReader(j.body), int64(len(j.body)))
			if err != nil {
				return err
			}
			if _, err = s.DB.Exec(`UPDATE archive_manifest_jobs SET reference=? WHERE file_id=? AND instance_id=? AND revision=?`, referenceJSON(ref), f.ID, j.instance, j.revision); err != nil {
				return err
			}
		}
		if err = b.Delete(ctx, ref); err != nil {
			return err
		}
		if _, err = s.DB.Exec(`DELETE FROM archive_manifest_jobs WHERE file_id=? AND instance_id=? AND revision=?`, f.ID, j.instance, j.revision); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cleanupArchiveGarbage(ctx context.Context, id string) error {
	rows, e := s.DB.Query(`SELECT instance_id,reference FROM archive_garbage WHERE file_id=?`, id)
	if e != nil {
		return e
	}
	type entry struct{ id, ref string }
	entries := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.id, &v.ref); e != nil {
			rows.Close()
			return e
		}
		entries = append(entries, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(entries) == 0 {
		return nil
	}
	chunks, e := s.allChunks(id)
	if e != nil {
		return e
	}
	f, e := s.DB.GetFile(id)
	if e != nil {
		return e
	}
	for _, v := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var active int
		if e = s.DB.QueryRow(`SELECT (SELECT count(*) FROM archive_objects WHERE file_id=? AND instance_id=? AND reference=?)+(SELECT count(*) FROM archive_manifests WHERE file_id=? AND instance_id=? AND reference=?)+(SELECT count(*) FROM archive_manifest_jobs WHERE file_id=? AND instance_id=? AND reference=?)+(SELECT count(*) FROM media_parts WHERE file_id=? AND instance_id=? AND json_extract(metadata,'$.reference')=?)`, id, v.id, v.ref, id, v.id, v.ref, id, v.id, v.ref, id, v.id, v.ref).Scan(&active); e != nil {
			return e
		}
		if active > 0 {
			if _, e = s.DB.Exec(`DELETE FROM archive_garbage WHERE file_id=? AND instance_id=? AND reference=?`, id, v.id, v.ref); e != nil {
				return e
			}
			continue
		}
		stillCanonical := false
		if v.id == f.ColdBackend {
			for _, c := range chunks {
				if referenceJSON(chunkRef(c)) == v.ref {
					stillCanonical = true
					break
				}
			}
		}
		if stillCanonical {
			continue
		}
		b, e := s.archiveBackend(v.id)
		if e != nil {
			return e
		}
		var ref storage.Reference
		if e = json.Unmarshal([]byte(v.ref), &ref); e != nil {
			return e
		}
		if e = b.Delete(ctx, ref); e != nil {
			return e
		}
		if _, e = s.DB.Exec(`DELETE FROM archive_garbage WHERE file_id=? AND instance_id=? AND reference=?`, id, v.id, v.ref); e != nil {
			return e
		}
	}
	return nil
}

func (r *pacedReader) Seek(offset int64, whence int) (int64, error) {
	if seeker, ok := r.r.(io.Seeker); ok {
		return seeker.Seek(offset, whence)
	}
	return 0, errors.New("该流不支持定位")
}
