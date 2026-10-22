package store

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/mediax"
)

type MediaView struct {
	OriginalView
	Part       dbx.MediaPart `json:"part"`
	Compatible string        `json:"compatible,omitempty"`
}

func (s *Service) MediaManifest(f *dbx.File) (dbx.MediaReplica, error) {
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return dbx.MediaReplica{}, err
	}
	return s.mediaManifest(all)
}

func (s *Service) mediaManifest(all []dbx.MediaReplica) (dbx.MediaReplica, error) {
	for pass := 0; pass < 3; pass++ {
		for _, r := range all {
			if r.State != "ready" || len(r.Parts) == 0 {
				continue
			}
			i, e := s.instance(r.InstanceID)
			local := e == nil && i.row.Driver == "local"
			original := len(r.Parts) == 1 && r.Parts[0].Original
			if pass == 0 && !local || pass == 1 && !original {
				continue
			}
			return r, nil
		}
	}
	return dbx.MediaReplica{}, errors.New("没有完整就绪的媒体副本")
}

func (s *Service) localMediaPart(f *dbx.File, p dbx.MediaPart) (dbx.MediaReplica, dbx.MediaPart, bool) {
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return dbx.MediaReplica{}, dbx.MediaPart{}, false
	}
	return s.localMediaPartFrom(all, p)
}

func (s *Service) localMediaPartFrom(all []dbx.MediaReplica, p dbx.MediaPart) (dbx.MediaReplica, dbx.MediaPart, bool) {
	for _, r := range all {
		instance, e := s.instance(r.InstanceID)
		if r.State == "removing" {
			continue
		}
		if e != nil || instance.row.Driver != "local" {
			continue
		}
		for _, local := range r.Parts {
			if local.Verified && local.SHA256 == p.SHA256 && local.Size == p.Size {
				return r, local, true
			}
		}
	}
	return dbx.MediaReplica{}, dbx.MediaPart{}, false
}

func (s *Service) MediaLocalReady(f *dbx.File) bool {
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return false
	}
	return s.mediaLocalReady(f, all)
}

func (s *Service) mediaLocalReady(f *dbx.File, all []dbx.MediaReplica) bool {
	manifest, err := s.mediaManifest(all)
	if err != nil || len(manifest.Parts) == 0 {
		return false
	}
	r, p, ok := s.localMediaPartFrom(all, manifest.Parts[0])
	if !ok {
		return false
	}
	if r.Encrypted {
		return true
	}
	input, err := s.openMediaPart(context.Background(), f, r, p)
	if err != nil {
		return false
	}
	defer input.Close()
	if file, ok := input.(*os.File); ok {
		st, e := file.Stat()
		return e == nil && st.Size() == p.Size
	}
	return false
}

// MediaPageInfo shares the page snapshot across status, manifest and links.
// The caller has already authorized and selected these visible file records.
func (s *Service) MediaPageInfo(f *dbx.File, all []dbx.MediaReplica, objects []*dbx.TempObject) (dbx.MediaReplica, bool, *AvailableLinks) {
	manifest, _ := s.mediaManifest(all)
	return manifest, s.mediaLocalReady(f, all), s.recordedMediaLinksFrom(f, objects, all)
}

func (s *Service) mediaViewFile(f *dbx.File, p dbx.MediaPart, compatible string) (*os.File, error) {
	if compatible != "" {
		path := s.mediaCompatiblePath(f, p, compatible)
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		stat, err := file.Stat()
		if err != nil || time.Since(stat.ModTime()) >= time.Duration(s.OriginalCacheHours())*time.Hour {
			file.Close()
			return nil, os.ErrNotExist
		}
		return file, nil
	}
	r, local, ok := s.localMediaPart(f, p)
	if !ok {
		return nil, os.ErrNotExist
	}
	if r.Encrypted {
		copy := mediaPartFile(f, r, p)
		file, _, err := s.openCachedOriginal(copy)
		return file, err
	}
	input, err := s.openMediaPart(context.Background(), f, r, local)
	if err != nil {
		return nil, err
	}
	file, ok := input.(*os.File)
	if !ok {
		input.Close()
		return nil, errors.New("本地存储未提供可定位文件")
	}
	stat, err := file.Stat()
	if err != nil || stat.Size() != p.Size {
		file.Close()
		return nil, os.ErrNotExist
	}
	if local.LocalModNS == 0 || stat.ModTime().UnixNano() != local.LocalModNS {
		n, sum, e := hashMediaPath(context.Background(), file.Name())
		if e != nil || n != p.Size || sum != p.SHA256 {
			file.Close()
			return nil, errors.New("本地原件校验失败，请从其他副本重新取回")
		}
		local.LocalModNS = stat.ModTime().UnixNano()
		if e = s.DB.SaveMediaPart(f.ID, r.InstanceID, local); e != nil {
			file.Close()
			return nil, e
		}
	}
	return file, nil
}

func (s *Service) mediaCompatiblePath(f *dbx.File, p dbx.MediaPart, kind string) string {
	copy := *f
	copy.Size = p.Size
	copy.SHA256 = p.SHA256
	ext := "mp4"
	if f.Kind == "image" {
		ext = "png"
	}
	return s.originalCachePath(&copy) + "." + kind + "." + ext
}

func (s *Service) PrepareMediaView(ctx context.Context, f *dbx.File, index int, compatible string, retry bool) (MediaView, error) {
	if f.ArchiveState != "done" || f.PurgeState.String != "" {
		return MediaView{}, errors.New("原件尚未就绪或正在删除")
	}
	if compatible != "" && compatible != "image" && compatible != "remux" && compatible != "transcode" {
		return MediaView{}, errors.New("无效的兼容查看方式")
	}
	if f.Kind == "image" && compatible != "" && compatible != "image" || f.Kind == "video" && compatible == "image" {
		return MediaView{}, errors.New("查看方式与媒体类型不匹配")
	}
	manifest, err := s.MediaManifest(f)
	if err != nil {
		return MediaView{}, err
	}
	if index < 0 || index >= len(manifest.Parts) {
		return MediaView{}, errors.New("视频片段编号无效")
	}
	part := manifest.Parts[index]
	view := MediaView{OriginalView: OriginalView{Total: part.Size}, Part: part, Compatible: compatible}
	// Never put provider references, hashes of ciphertext or keys in view JSON.
	view.Part.Reference = ""
	view.Part.PayloadSHA256 = ""
	view.Part.PayloadSize = 0
	if file, e := s.mediaViewFile(f, part, compatible); e == nil {
		file.Close()
		view.Status = "ready"
		view.Loaded = part.Size
		view.Percent = 100
		view.Source = "local"
		return view, nil
	}
	copy := mediaPartFile(f, manifest, part)
	key := mediaKey(copy) + "/" + compatible
	cache := s.media
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return view, errors.New("服务正在停止")
	}
	if job := cache.jobs[key]; job != nil {
		if job.view.Status == "loading" || job.view.Status == "failed" && !retry {
			view.OriginalView = job.view
			return view, nil
		}
		delete(cache.jobs, key)
	}
	for k, j := range cache.jobs {
		if j.view.Status != "loading" {
			delete(cache.jobs, k)
		}
	}
	if len(cache.jobs) >= 8 {
		return view, errors.New("正在准备的原件较多，请稍后重试")
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	job := &originalJob{view: OriginalView{Status: "loading", Total: part.Size}, cancel: cancel, done: make(chan struct{}), fileID: f.ID}
	cache.jobs[key] = job
	cache.wg.Add(1)
	go func() {
		defer cache.wg.Done()
		defer close(job.done)
		defer cancel()
		var err error
		select {
		case cache.slots <- struct{}{}:
			err = s.loadMediaView(jobCtx, f, manifest, part, compatible, job)
			<-cache.slots
		case <-jobCtx.Done():
			err = jobCtx.Err()
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if err != nil {
			job.view.Status = "failed"
			job.view.Error = err.Error()
		} else {
			job.view.Status = "ready"
			job.view.Percent = 100
			job.view.Loaded = part.Size
			job.view.Source = "local"
		}
	}()
	view.OriginalView = job.view
	return view, nil
}

func (s *Service) loadMediaView(ctx context.Context, f *dbx.File, source dbx.MediaReplica, p dbx.MediaPart, compatible string, job *originalJob) error {
	unlock, err := s.lockFile(ctx, f.ID)
	if err != nil {
		return err
	}
	defer unlock()
	fresh, err := s.mutableFile(f.ID)
	if err != nil {
		return err
	}
	if fresh.PurgeState.String != "" {
		return dbx.ErrDeleting
	}
	finish := s.BeginForegroundTransfer()
	defer finish()
	if err = os.MkdirAll(s.originalCacheDir(), 0700); err != nil {
		return err
	}
	if local, stored, exists := s.localMediaPart(f, p); exists && !local.Encrypted {
		if file, e := s.mediaViewFile(f, p, ""); e == nil {
			file.Close()
		} else if e = s.markMediaPartUnavailable(f, local, stored, e); e != nil {
			return e
		}
	}
	if _, _, exists := s.localMediaPart(f, p); !exists {
		target := s.localMediaTarget(f)
		local, err := s.mediaReplica(f, target)
		if err != nil {
			return err
		}
		input, err := s.openMatchingMediaPart(ctx, f, p)
		if err != nil {
			return err
		}
		out, err := os.CreateTemp(s.originalCacheDir(), "local-load-*.part")
		if err != nil {
			input.Close()
			return err
		}
		defer os.Remove(out.Name())
		_ = out.Chmod(0600)
		h := sha256New()
		writer := progressWriter{io.MultiWriter(out, h), func(n int) {
			s.media.mu.Lock()
			job.view.Loaded += int64(n)
			job.view.Percent = int(min(90, job.view.Loaded*90/max(1, p.Size)))
			s.media.mu.Unlock()
		}}
		n, err := io.Copy(writer, io.LimitReader(contextReaderForCache{ctx, input}, p.Size+1))
		err = errors.Join(err, input.Close(), out.Close())
		if err != nil {
			return err
		}
		if n != p.Size || hexOf(h) != p.SHA256 {
			return errors.New("原件取回校验失败，未登记本地副本")
		}
		if fresh, err = s.mutableFile(f.ID); err != nil {
			return err
		}
		if fresh.PurgeState.String != "" {
			return dbx.ErrDeleting
		}
		// Preserve an existing remote segment as a segment. A local 'do not split'
		// policy never means concatenating independent video files into fake originals.
		if _, err = s.writeMediaPart(ctx, f, local, p, out.Name()); err != nil {
			return err
		}
		all, err := s.DB.MediaReplicas(f.ID)
		if err != nil {
			return err
		}
		for _, r := range all {
			if r.InstanceID == target {
				count := 0
				for _, part := range r.Parts {
					if part.Verified && part.Count == p.Count {
						count++
					}
				}
				state := "partial"
				if count == p.Count {
					state = "ready"
				}
				if err = s.DB.SetMediaReplicaState(f.ID, target, state, ""); err != nil {
					return err
				}
			}
		}
	}
	local, stored, exists := s.localMediaPart(f, p)
	if !exists {
		return errors.New("本地副本未完成")
	}
	native, err := s.mediaViewFile(f, p, "")
	if err != nil && local.Encrypted {
		input, e := s.openMediaPart(ctx, f, local, stored)
		if e != nil {
			return e
		}
		out, e := os.CreateTemp(s.originalCacheDir(), "decrypt-*.part")
		if e != nil {
			input.Close()
			return e
		}
		defer os.Remove(out.Name())
		_ = out.Chmod(0600)
		h := sha256New()
		n, e := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReaderForCache{ctx, input}, p.Size+1))
		e = errors.Join(e, input.Close(), out.Close())
		if e != nil {
			return e
		}
		if n != p.Size || hexOf(h) != p.SHA256 {
			return errors.New("本地加密副本恢复校验失败")
		}
		copy := mediaPartFile(f, local, p)
		if e = os.Rename(out.Name(), s.originalCachePath(copy)); e != nil {
			return e
		}
		native, err = s.mediaViewFile(f, p, "")
	}
	if err != nil {
		return err
	}
	defer native.Close()
	if compatible == "" {
		return nil
	}
	path := s.mediaCompatiblePath(f, p, compatible)
	temp := path + ".part"
	defer os.Remove(temp)
	if err = mediax.MakeCompatible(ctx, native.Name(), temp, f.Kind, compatible); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func (s *Service) OpenMediaView(ctx context.Context, f *dbx.File, index int, compatible string, prepare bool) (*os.File, dbx.MediaPart, error) {
	manifest, err := s.MediaManifest(f)
	if err != nil {
		return nil, dbx.MediaPart{}, err
	}
	if index < 0 || index >= len(manifest.Parts) {
		return nil, dbx.MediaPart{}, errors.New("片段编号无效")
	}
	p := manifest.Parts[index]
	if file, err := s.mediaViewFile(f, p, compatible); err == nil {
		return file, p, nil
	}
	if !prepare {
		return nil, p, os.ErrNotExist
	}
	for {
		view, err := s.PrepareMediaView(ctx, f, index, compatible, true)
		if err != nil {
			return nil, p, err
		}
		if view.Status == "ready" {
			file, err := s.mediaViewFile(f, p, compatible)
			return file, p, err
		}
		if view.Status == "failed" {
			return nil, p, errors.New(view.Error)
		}
		// Wait on the actual job, not polling the provider or reissuing downloads.
		copy := mediaPartFile(f, manifest, p)
		key := mediaKey(copy) + "/" + compatible
		s.media.mu.Lock()
		job := s.media.jobs[key]
		s.media.mu.Unlock()
		if job == nil {
			return nil, p, errors.New("加载任务已停止")
		}
		select {
		case <-ctx.Done():
			return nil, p, ctx.Err()
		case <-job.done:
			if job.view.Status == "failed" {
				return nil, p, errors.New(job.view.Error)
			}
		}
	}
}

func (s *Service) RemoveLocalMedia(ctx context.Context, f *dbx.File, instance string) error {
	unlock, err := s.lockFile(ctx, f.ID)
	if err != nil {
		return err
	}
	defer unlock()
	i, err := s.instance(instance)
	if err != nil {
		return err
	}
	if i.row.Driver != "local" {
		return errors.New("此操作仅移除本地副本")
	}
	fresh, err := s.mutableFile(f.ID)
	if err != nil {
		return err
	}
	if fresh.PurgeState.String != "" {
		return dbx.ErrDeleting
	}
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		return err
	}
	other := false
	for _, replica := range all {
		if replica.InstanceID != instance && replica.State == "ready" && len(replica.Parts) > 0 {
			other = true
			break
		}
	}
	if !other {
		return errors.New("这是唯一可恢复的副本，请使用彻底删除操作")
	}
	s.ForgetMediaViews(f)
	return s.purgeMedia(ctx, f, instance)
}

func mediaViewMIME(f *dbx.File, p dbx.MediaPart, compatible string) string {
	if compatible != "" {
		if f.Kind == "image" {
			return "image/png"
		}
		return "video/mp4"
	}
	return p.MIME
}
