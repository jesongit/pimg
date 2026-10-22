package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pimg/internal/dbx"
	"pimg/internal/storage"
)

type ReplicaField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
}
type ReplicaDriver struct {
	ID       string                                                 `json:"id"`
	Name     string                                                 `json:"name"`
	Fields   []ReplicaField                                         `json:"fields"`
	Validate func(json.RawMessage) (string, error)                  `json:"-"`
	Open     func(context.Context, string) (storage.Replica, error) `json:"-"`
}
type localReplicaConfig struct {
	Directory string `json:"directory"`
}

func (s *Service) initReplicaDrivers() {
	s.replicaDrivers = map[string]ReplicaDriver{}
	s.replicaDrivers["local"] = ReplicaDriver{ID: "local", Name: "本地目录", Fields: []ReplicaField{{Key: "directory", Label: "存储目录", Hint: "相对于数据目录，例如 replicas/backup-a；请使用独立目录。"}}, Validate: s.validateLocalReplica, Open: func(ctx context.Context, config string) (storage.Replica, error) {
		var c localReplicaConfig
		if err := json.Unmarshal([]byte(config), &c); err != nil {
			return nil, err
		}
		return storage.LocalReplica{DataDir: s.Cfg.Paths.DataDir, Directory: c.Directory}, ctx.Err()
	}}
}

// New integrations register a native album-aware driver here. Normal cold/hot
// provider selection and foreground operations do not depend on this registry.
func (s *Service) RegisterReplicaDriver(d ReplicaDriver) error {
	if d.ID == "" || d.Validate == nil || d.Open == nil {
		return errors.New("副仓适配器不完整")
	}
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	if _, ok := s.replicaDrivers[d.ID]; ok {
		return errors.New("副仓适配器已注册")
	}
	s.replicaDrivers[d.ID] = d
	return nil
}
func (s *Service) ReplicaDrivers() []ReplicaDriver {
	s.replicaMu.RLock()
	defer s.replicaMu.RUnlock()
	out := []ReplicaDriver{}
	for _, d := range s.replicaDrivers {
		out = append(out, d)
	}
	return out
}
func (s *Service) replicaDriver(id string) (ReplicaDriver, error) {
	s.replicaMu.RLock()
	defer s.replicaMu.RUnlock()
	d, ok := s.replicaDrivers[id]
	if !ok {
		return d, errors.New("该存储尚未接入副仓适配器")
	}
	return d, nil
}
func pathsOverlap(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	a = strings.ToLower(filepath.Clean(a))
	b = strings.ToLower(filepath.Clean(b))
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
func (s *Service) validateLocalReplica(raw json.RawMessage) (string, error) {
	var c localReplicaConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return "", errors.New("本地副仓配置无效")
	}
	c.Directory = filepath.ToSlash(filepath.Clean(strings.TrimSpace(c.Directory)))
	if c.Directory == "." || !filepath.IsLocal(c.Directory) || strings.Contains(c.Directory, "\\") || len(c.Directory) > 240 {
		return "", errors.New("副仓目录必须是数据目录内的相对路径，不能使用绝对路径或上级目录")
	}
	full := filepath.Join(s.Cfg.Paths.DataDir, filepath.FromSlash(c.Directory))
	cold, hot := s.Cfg.Storage.LocalColdDir, s.Cfg.Storage.LocalHotDir
	if cold == "" {
		cold = filepath.Join(s.Cfg.Paths.DataDir, "cold")
	}
	if hot == "" {
		hot = filepath.Join(s.Cfg.Paths.DataDir, "hot")
	}
	for _, i := range s.StorageInstances() {
		if i.Driver != "local" {
			continue
		}
		for _, dir := range []string{i.Config.Directory, i.Config.ColdDirectory} {
			if dir != "" && pathsOverlap(full, dir) {
				return "", errors.New("副仓不能与已配置的本地存储目录重叠")
			}
		}
	}
	for _, reserved := range []string{cold, hot, s.upDir, s.ctDir, filepath.Join(s.Cfg.Paths.DataDir, "telegram"), filepath.Join(s.Cfg.Paths.DataDir, "diagnostics")} {
		if reserved != "" && pathsOverlap(full, reserved) {
			return "", errors.New("副仓不能与主仓、热存储或工作目录重叠")
		}
	}
	data, err := json.Marshal(c)
	return string(data), err
}
func (s *Service) AddColdRepository(name, driver string, config json.RawMessage) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return 0, errors.New("副仓名称须为 1–80 个字符")
	}
	d, err := s.replicaDriver(driver)
	if err != nil {
		return 0, err
	}
	canonical, err := d.Validate(config)
	if err != nil {
		return 0, err
	}
	if driver == "local" {
		var c localReplicaConfig
		_ = json.Unmarshal([]byte(canonical), &c)
		repos, err := s.DB.ColdRepositories()
		if err != nil {
			return 0, err
		}
		for _, r := range repos {
			if r.Driver == "local" {
				var other localReplicaConfig
				_ = json.Unmarshal([]byte(r.Config), &other)
				if pathsOverlap(c.Directory, other.Directory) {
					return 0, errors.New("副仓目录不能与其他副仓重叠")
				}
			}
		}
	}
	return s.DB.AddColdRepository(name, driver, canonical)
}

type syncActivity struct {
	mu         sync.Mutex
	foreground int
	idleCancel context.CancelFunc
	yieldTimer *time.Timer
	idleEpoch  uint64
}

func (s *Service) InterruptIdle() {
	s.activity.mu.Lock()
	if s.activity.idleCancel != nil {
		s.activity.idleCancel()
	}
	s.activity.mu.Unlock()
}
func (s *Service) BeginForeground() func() {
	return s.beginForeground(false)
}

// Small, independent reads and uploads often finish before an idle transfer can
// even acknowledge cancellation. Allow a bounded overlap at the existing sync
// rate; sustained work still preempts. Destructive operations use BeginForeground.
func (s *Service) BeginForegroundTransfer() func() {
	return s.beginForeground(true)
}

const foregroundYieldGrace = 200 * time.Millisecond

func (s *Service) beginForeground(grace bool) func() {
	s.activity.mu.Lock()
	s.activity.foreground++
	if s.activity.idleCancel != nil {
		if !grace {
			s.activity.idleCancel()
		} else if s.activity.yieldTimer == nil {
			epoch := s.activity.idleEpoch
			var timer *time.Timer
			timer = time.AfterFunc(foregroundYieldGrace, func() {
				s.activity.mu.Lock()
				defer s.activity.mu.Unlock()
				if s.activity.idleEpoch == epoch && s.activity.yieldTimer == timer {
					s.activity.yieldTimer = nil
					if s.activity.foreground > 0 && s.activity.idleCancel != nil {
						s.Log.Debug("闲时任务让位", "reason", "foreground_transfer", "active", s.activity.foreground)
						s.activity.idleCancel()
					}
				}
			})
			s.activity.yieldTimer = timer
		}
	}
	s.activity.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activity.mu.Lock()
			defer s.activity.mu.Unlock()
			s.activity.foreground--
			if s.activity.foreground == 0 && s.activity.yieldTimer != nil {
				s.activity.yieldTimer.Stop()
				s.activity.yieldTimer = nil
			}
		})
	}
}
func (s *Service) BeginIdle(ctx context.Context) (context.Context, func(), error) {
	s.activity.mu.Lock()
	defer s.activity.mu.Unlock()
	if s.activity.foreground > 0 || s.activity.idleCancel != nil {
		return nil, nil, context.Canceled
	}
	busy, err := s.DB.ColdSyncForegroundBusy(time.Now().Unix())
	if err != nil {
		return nil, nil, err
	}
	if busy {
		return nil, nil, context.Canceled
	}
	child, cancel := context.WithCancel(ctx)
	s.activity.idleEpoch++
	s.activity.idleCancel = cancel
	var once sync.Once
	return child, func() {
		once.Do(func() {
			cancel()
			s.activity.mu.Lock()
			defer s.activity.mu.Unlock()
			s.activity.idleCancel = nil
			if s.activity.yieldTimer != nil {
				s.activity.yieldTimer.Stop()
				s.activity.yieldTimer = nil
			}
		})
	}, nil
}

type taskContextKey struct{}

func WithTaskID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, taskContextKey{}, id)
}
func currentTaskID(ctx context.Context) int64 {
	id, _ := ctx.Value(taskContextKey{}).(int64)
	return id
}

type replicaCopy struct {
	storage.ReplicaObject
	Reference string `json:"reference"`
	AlbumID   int64  `json:"album_id"`
	Retired   bool   `json:"retired,omitempty"`
}
type replicaSource struct {
	storage.ReplicaObject
	open func(context.Context) (io.ReadCloser, error)
}

func (s *Service) replicaSources(ctx context.Context, f *dbx.File) ([]replicaSource, error) {
	if f.MediaVersion == 2 {
		return s.mediaReplicaSources(ctx, f)
	}
	var out []replicaSource
	for _, kind := range []string{"data"} {
		chunks, err := s.DB.ListChunks(f.ID, kind)
		if err != nil {
			return nil, err
		}
		if kind == "data" && len(chunks) == 0 {
			return nil, errors.New("主仓原件分片不存在")
		}
		for _, c := range chunks {
			ext, mime := "bin", "application/octet-stream"
			size, sum := c.Size, c.SHA256
			chunk := c
			var open func(context.Context) (io.ReadCloser, error)
			if f.ColdBackend == "telegram" && s.UserTG != nil {
				obj, err := s.telegramObject(c.ObjectKey)
				if err != nil {
					return nil, err
				}
				size, sum = obj.Size, obj.WireSHA256
				if obj.Carrier {
					ext, mime = "png", "image/png"
				} else if kind == "data" {
					ext, mime = f.Ext, f.Mime
				}
				open = func(ctx context.Context) (io.ReadCloser, error) { return s.openTelegramWire(ctx, obj) }
			} else {
				backend, e := s.archiveBackend(f.ColdBackend)
				if e != nil {
					return nil, e
				}
				raw, e := backend.Open(context.Background(), chunkRef(chunk))
				if e != nil {
					return nil, e
				}
				hash := sha256New()
				size, e = io.Copy(hash, raw)
				raw.Close()
				if e != nil {
					return nil, e
				}
				sum = hexOf(hash)
				if f.Storage != "direct" {
					ext, mime = "png", "image/png"
				}
				open = func(ctx context.Context) (io.ReadCloser, error) { return backend.Open(ctx, chunkRef(chunk)) }
				if f.Storage == "direct" && kind == "data" {
					ext, mime = f.Ext, f.Mime
				}
			}
			name := fmt.Sprintf("%s-%04d.%s", kind, c.Idx, ext)
			if named := TelegramFilename(f, kind, c.Idx); named != "" {
				name = strings.TrimSuffix(named, filepath.Ext(named)) + "." + ext
			}
			out = append(out, replicaSource{ReplicaObject: storage.ReplicaObject{Name: name, Size: size, SHA256: sum, MIME: mime}, open: open})
		}
	}
	return out, nil
}
func verifyReplica(ctx context.Context, driver storage.Replica, copy replicaCopy) error {
	r, err := driver.Open(ctx, copy.Reference)
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(syncContextReader{ctx, r}, copy.Size+1))
	if err != nil {
		return err
	}
	if n != copy.Size || hex.EncodeToString(h.Sum(nil)) != copy.SHA256 {
		return errors.New("副仓回读校验失败")
	}
	return ctx.Err()
}

type syncContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r syncContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *Service) RunColdSync(ctx context.Context, fileID string, repoID int64) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	unlockRepo, err := s.lockFile(ctx, fmt.Sprintf("replica:%d", repoID))
	if err != nil {
		return err
	}
	defer unlockRepo()
	repo, err := s.DB.GetColdRepository(repoID)
	if err != nil {
		return err
	}
	if !repo.Enabled {
		return errors.New("副仓已暂停，请勾选后重试")
	}
	f, err := s.DB.GetVisibleFile(fileID)
	if err != nil {
		return err
	}
	if f.ArchiveState != "done" || f.PurgeState.Valid {
		return errors.New("文件尚未归档完成或正在删除")
	}
	if active, err := s.DB.ConversionActive(fileID); err != nil {
		return err
	} else if active {
		return dbx.ErrConverting
	}
	j, err := s.DB.GetColdSyncJob(repoID, fileID)
	if err != nil {
		return err
	}
	d, err := s.replicaDriver(repo.Driver)
	if err != nil {
		return err
	}
	driver, err := d.Open(ctx, repo.Config)
	if err != nil {
		return err
	}
	albumID := f.AlbumID.Int64
	if albumID <= 0 {
		albumID, err = s.DB.EnsureOwnerDefaultAlbum(f.OwnerID)
		if err != nil {
			return err
		}
	}
	album, err := s.DB.GetAlbum(albumID)
	if err != nil {
		return err
	}
	albumRef, err := driver.EnsureAlbum(ctx, storage.ReplicaAlbum{ID: albumID, Name: album.Name, OwnerID: f.OwnerID})
	if err != nil {
		return err
	}
	if err = s.DB.SaveColdSyncAlbum(repoID, albumID, album.Name, albumRef); err != nil {
		return err
	}
	var copies []replicaCopy
	if err = json.Unmarshal([]byte(j.Objects), &copies); err != nil {
		return err
	}
	sources, err := s.replicaSources(ctx, f)
	if err != nil {
		return err
	}
	save := func() error {
		b, err := json.Marshal(copies)
		if err != nil {
			return err
		}
		return s.DB.SaveColdSyncObjects(j, string(b))
	}
	used := map[int]bool{}
	for _, src := range sources {
		if err = ctx.Err(); err != nil {
			return err
		}
		found := -1
		for i, c := range copies {
			if c.Name == src.Name && c.Size == src.Size && c.SHA256 == src.SHA256 && !c.Retired && c.AlbumID == albumID {
				if verifyReplica(ctx, driver, c) == nil {
					found = i
					break
				}
			}
		}
		if found >= 0 {
			used[found] = true
			continue
		}
		// A durable operation locator precedes the remote write, so cancellation
		// and lost acknowledgments cannot leave an untracked partial copy.
		planned, err := driver.Prepare(ctx, albumRef, fileID, src.ReplicaObject)
		if err != nil {
			return err
		}
		fresh := replicaCopy{ReplicaObject: src.ReplicaObject, Reference: planned, AlbumID: albumID}
		copies = append(copies, fresh)
		index := len(copies) - 1
		used[index] = true
		if err = save(); err != nil {
			return err
		}
		r, err := src.open(ctx)
		if err != nil {
			return err
		}
		ref, putErr := driver.Put(ctx, albumRef, fileID, src.ReplicaObject, r)
		closeErr := r.Close()
		if putErr != nil {
			return putErr
		}
		copies[index].Reference = ref
		fresh = copies[index]
		if err = save(); err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = verifyReplica(ctx, driver, fresh); err != nil {
			return err
		}
	}
	activeRefs := map[string]bool{}
	for i := range used {
		activeRefs[copies[i].Reference] = true
	}
	var obsolete []string
	kept := make([]replicaCopy, 0, len(copies))
	for i, c := range copies {
		if !used[i] {
			if activeRefs[c.Reference] {
				continue
			}
			c.Retired = true
			obsolete = append(obsolete, c.Reference)
		}
		kept = append(kept, c)
	}
	copies = kept
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = save(); err != nil {
		return err
	}
	if err = s.DB.FinishColdSync(j); err != nil {
		return err
	}
	if len(obsolete) > 0 {
		_, err = s.DB.EnqueueColdDelete(repoID, fileID, obsolete)
	}
	return err
}

func (s *Service) RunColdDelete(ctx context.Context, fileID string, repoID int64, refs []string) error {
	unlock, err := s.lockFile(ctx, fileID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.deleteReplicaCopies(ctx, fileID, repoID, refs)
}
func (s *Service) deleteReplicaCopies(ctx context.Context, fileID string, repoID int64, refs []string) error {
	unlock, err := s.lockFile(ctx, fmt.Sprintf("replica:%d", repoID))
	if err != nil {
		return err
	}
	defer unlock()
	repo, err := s.DB.GetColdRepository(repoID)
	if err != nil {
		return err
	}
	d, err := s.replicaDriver(repo.Driver)
	if err != nil {
		return err
	}
	driver, err := d.Open(ctx, repo.Config)
	if err != nil {
		return err
	}
	j, err := s.DB.GetColdSyncJob(repoID, fileID)
	if err != nil {
		return err
	}
	var copies []replicaCopy
	if err = json.Unmarshal([]byte(j.Objects), &copies); err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, ref := range refs {
		selected[ref] = true
	}
	for i := 0; i < len(copies); {
		c := copies[i]
		if refs != nil && (!selected[c.Reference] || !c.Retired) {
			i++
			continue
		}
		if err = driver.Delete(ctx, c.Reference); err != nil {
			return err
		}
		copies = append(copies[:i], copies[i+1:]...)
		data, _ := json.Marshal(copies)
		if err = s.DB.SaveColdSyncObjects(j, string(data)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) purgeReplicaCopies(ctx context.Context, fileID string) error {
	jobs, err := s.DB.FileColdSyncJobs(fileID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err = s.deleteReplicaCopies(ctx, fileID, j.RepositoryID, nil); err != nil {
			return fmt.Errorf("副仓 %d 清理失败：%w", j.RepositoryID, err)
		}
	}
	return nil
}
