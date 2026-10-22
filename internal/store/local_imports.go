package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"pimg/internal/dbx"
	"pimg/internal/mediax"
)

type privateUploadKey struct{}

func WithPrivateUpload(ctx context.Context) context.Context {
	return context.WithValue(ctx, privateUploadKey{}, true)
}

var ErrLocalImportPending = errors.New("导入批次仍有待处理项目")
var ErrLocalImportWaiting = errors.New("等待当前归档或批次恢复")

type LocalImportBatch struct {
	ID        int64            `json:"id"`
	OwnerID   string           `json:"owner_id"`
	TaskID    int64            `json:"task_id"`
	Root      string           `json:"source_root"`
	Paths     []string         `json:"paths"`
	AlbumID   int64            `json:"album_id"`
	Encrypted bool             `json:"encrypted"`
	Recursive bool             `json:"recursive"`
	Schedule  string           `json:"schedule"`
	State     string           `json:"state"`
	Error     string           `json:"error"`
	CreatedAt int64            `json:"created_at"`
	Counts    map[string]int64 `json:"counts"`
	Bytes     int64            `json:"bytes"`
}

type LocalImportItem struct {
	ID            int64    `json:"id"`
	Path          string   `json:"path"`
	Size          int64    `json:"size"`
	Version       string   `json:"-"`
	State         string   `json:"state"`
	FileIDs       []string `json:"file_ids"`
	Error         string   `json:"error"`
	ExistingAlbum string   `json:"existing_album,omitempty"`
}

type ImportPath struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
}

func (s *Service) ImportRoot() string {
	root := s.Cfg.Paths.ImportDir
	if root == "" {
		root = "./imports"
	}
	abs, _ := filepath.Abs(root)
	return abs
}

// Used only as a convenient opening location and for older relative-path API
// clients. Each new batch records its own source root, independent of settings.
func (s *Service) InitialImportPath() string {
	for _, candidate := range []string{s.ImportRoot(), "/imports"} {
		if info, err := s.ImportPathInfo(candidate); err == nil && info.Directory {
			return info.Path
		}
	}
	working, _ := os.Getwd()
	return filepath.VolumeName(working) + string(filepath.Separator)
}

func (s *Service) absoluteImportPath(p string) (string, error) {
	if filepath.IsAbs(p) {
		if strings.ContainsRune(p, '\x00') || strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, `\\?\`) {
			return "", errors.New("请选择本机或容器内的文件路径")
		}
		return filepath.Clean(p), nil
	}
	p, err := cleanImportPath(p)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.ImportRoot(), filepath.FromSlash(p)), nil
}

// Check ancestors as well as entries below the selected source; choosing a
// different absolute path must not make symlinks an implicit import feature.
func openImportRoot(p string) (*os.Root, error) {
	if !filepath.IsAbs(p) {
		return nil, errors.New("导入来源必须是绝对路径")
	}
	volume := filepath.VolumeName(p) + string(filepath.Separator)
	outer, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	defer outer.Close()
	relative, err := filepath.Rel(volume, p)
	if err != nil {
		return nil, err
	}
	if _, err = importInfo(outer, filepath.ToSlash(relative)); err != nil {
		return nil, err
	}
	return outer.OpenRoot(relative)
}

func (s *Service) ImportPathInfo(p string) (ImportPath, error) {
	abs, err := s.absoluteImportPath(p)
	if err != nil {
		return ImportPath{}, err
	}
	root, err := openImportRoot(filepath.Dir(abs))
	if err != nil {
		return ImportPath{}, err
	}
	defer root.Close()
	entry := filepath.Base(abs)
	if abs == filepath.Dir(abs) {
		entry = "."
	}
	info, err := importInfo(root, entry)
	if err != nil {
		return ImportPath{}, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return ImportPath{}, errors.New("请选择普通文件或目录")
	}
	return ImportPath{Path: abs, Name: filepath.Base(abs), Directory: info.IsDir(), Size: info.Size()}, nil
}

func cleanImportPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if p != "." && (!fs.ValidPath(p) || strings.ContainsAny(p, "\\:\x00")) {
		return "", errors.New("请选择导入根目录内的相对路径")
	}
	return p, nil
}

// os.Root enforces confinement even across filesystem races. Reject symlinks
// within the root as well, so browsing and importing have identical semantics.
func importInfo(root *os.Root, p string) (fs.FileInfo, error) {
	if _, err := cleanImportPath(p); err != nil {
		return nil, err
	}
	current := ""
	for _, part := range strings.Split(p, "/") {
		current = path.Join(current, part)
		st, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("不导入符号链接")
		}
	}
	return root.Stat(p)
}

func (s *Service) BrowseImports(p string, offset int) ([]ImportPath, int, error) {
	node, err := s.ImportPathInfo(p)
	if err != nil {
		return nil, 0, err
	}
	if !node.Directory {
		return []ImportPath{node}, 0, nil
	}
	root, err := openImportRoot(node.Path)
	if err != nil {
		return nil, 0, errors.New("无法读取此路径，请检查目录、挂载和读取权限")
	}
	defer root.Close()
	d, err := root.Open(".")
	if err != nil {
		return nil, 0, err
	}
	defer d.Close()
	if offset < 0 || offset > 10_000_000 {
		return nil, 0, errors.New("分页位置无效")
	}
	for n := 0; n < offset; {
		entries, e := d.ReadDir(min(200, offset-n))
		n += len(entries)
		if e != nil {
			return []ImportPath{}, 0, nil
		}
	}
	entries, err := d.ReadDir(201)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	next := 0
	if len(entries) > 200 {
		next = offset + 200
		entries = entries[:200]
	}
	out := []ImportPath{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		out = append(out, ImportPath{Path: filepath.Join(node.Path, entry.Name()), Name: entry.Name(), Directory: entry.IsDir(), Size: info.Size()})
	}
	return out, next, nil
}

func (s *Service) CreateLocalImport(paths []string, album int64, encrypted, recursive bool, schedule string) (int64, error) {
	owner := dbx.AdminOwner
	if len(paths) == 0 || len(paths) > 200 {
		return 0, errors.New("请选择 1–200 个文件或目录")
	}
	if album != 0 {
		target, err := s.DB.GetAlbum(album)
		if err != nil {
			return 0, errors.New("请选择有效的目标相册")
		}
		owner = target.OwnerID
	}
	if !s.DB.AccountActive(owner) {
		return 0, errors.New("目标相册的账号已停用或不存在")
	}
	if schedule != "idle" && schedule != "now" {
		return 0, errors.New("请选择空闲时或立即导入")
	}
	sourceRoot := s.ImportRoot()
	absolute := false
	for _, p := range paths {
		absolute = absolute || filepath.IsAbs(p)
	}
	paths = append([]string(nil), paths...)
	if absolute {
		for i, p := range paths {
			var err error
			paths[i], err = s.absoluteImportPath(p)
			if err != nil {
				return 0, err
			}
		}
		sourceRoot = filepath.Dir(paths[0])
		for _, p := range paths {
			for {
				relative, err := filepath.Rel(sourceRoot, p)
				if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					break
				}
				parent := filepath.Dir(sourceRoot)
				if parent == sourceRoot {
					return 0, errors.New("请在同一磁盘内选择文件或目录，不同磁盘请分批导入")
				}
				sourceRoot = parent
			}
		}
		for i, p := range paths {
			relative, err := filepath.Rel(sourceRoot, p)
			if err != nil {
				return 0, err
			}
			paths[i] = filepath.ToSlash(relative)
		}
	}
	root, err := openImportRoot(sourceRoot)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	for i, p := range paths {
		p, err = cleanImportPath(p)
		if err != nil {
			return 0, err
		}
		if _, err = importInfo(root, p); err != nil {
			return 0, err
		}
		paths[i] = p
	}
	raw, _ := json.Marshal(paths)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO local_import_batches(root,paths,album_id,encrypted,recursive,schedule) VALUES(?,?,?,?,?,?)`, sourceRoot, string(raw), album, encrypted, recursive, schedule)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = dbx.SnapshotLocalImport(tx, id, album, sourceRoot, owner); err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO tasks(type,payload,state,run_after,created_at,updated_at) VALUES(?,?,'pending',unixepoch(),unixepoch(),unixepoch())`, dbx.TaskLocalImportScan, dbx.JSONValue(map[string]any{"batch_id": id}))
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Service) LocalImportBatch(id int64) (*LocalImportBatch, error) {
	return s.localImportBatch(id, true)
}

func (s *Service) localImportBatch(id int64, counts bool) (*LocalImportBatch, error) {
	if counts {
		if err := s.refreshLocalImportItems(id); err != nil {
			return nil, err
		}
	}
	b := &LocalImportBatch{Counts: map[string]int64{}}
	owner, err := s.DB.LocalImportOwner(id)
	if err != nil {
		return nil, err
	}
	b.OwnerID = owner
	var raw string
	err = s.DB.QueryRow(`SELECT id,root,paths,album_id,encrypted,recursive,schedule,state,error,created_at FROM local_import_batches WHERE id=?`, id).Scan(&b.ID, &b.Root, &raw, &b.AlbumID, &b.Encrypted, &b.Recursive, &b.Schedule, &b.State, &b.Error, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err = s.DB.QueryRow(`SELECT COALESCE(MIN(id),0) FROM tasks WHERE type IN ('local_import_scan','local_import','local_import_idle') AND json_valid(payload) AND json_extract(payload,'$.batch_id')=?`, id).Scan(&b.TaskID); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &b.Paths); err != nil {
		return nil, err
	}
	if !counts {
		return b, nil
	}
	rows, err := s.DB.ReadQuery(`SELECT state,COUNT(*),COALESCE(SUM(size),0) FROM local_import_items WHERE batch_id=? GROUP BY state`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count, size int64
		if err = rows.Scan(&state, &count, &size); err != nil {
			return nil, err
		}
		b.Counts[state] = count
		b.Bytes += size
	}
	return b, rows.Err()
}

// File tasks continue independently while the intake batch is paused/canceled.
// Derive their final receipt from every part, rather than reporting acceptance
// as successful archival or leaving completed files labeled as archiving.
func (s *Service) refreshLocalImportItems(id int64) error {
	// Resolve all parts in one read, then commit only completed receipts in a
	// short transaction. A page poll never performs a query per imported file.
	rows, err := s.DB.ReadQuery(`SELECT i.id,
    CASE WHEN SUM(j.value IS NOT NULL AND (f.id IS NULL OR f.hidden=1 OR f.archive_state='failed'))>0 THEN 'failed' ELSE 'done' END state,
    COALESCE(MAX(CASE WHEN f.archive_state='failed' THEN COALESCE(f.archive_error,'') END),'导入文件记录不可用') reason
    FROM local_import_items i LEFT JOIN json_each(i.file_ids) j LEFT JOIN files f ON f.id=j.value
    WHERE i.batch_id=? AND i.state='archiving' GROUP BY i.id
    HAVING SUM(j.value IS NOT NULL AND (f.id IS NULL OR f.hidden=1 OR f.archive_state='failed'))>0
    OR SUM(j.value IS NOT NULL AND f.id IS NOT NULL AND f.hidden=0 AND f.archive_state!='done')=0
    ORDER BY i.id LIMIT 200`, id)
	if err != nil {
		return err
	}
	type result struct {
		id            int64
		state, reason string
	}
	list := []result{}
	for rows.Next() {
		var v result
		if err = rows.Scan(&v.id, &v.state, &v.reason); err != nil {
			rows.Close()
			return err
		}
		if v.state == "done" {
			v.reason = ""
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(list) == 0 {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range list {
		if _, err = tx.Exec(`UPDATE local_import_items SET state=?,error=? WHERE id=? AND state='archiving'`, v.state, v.reason, v.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) LocalImportItems(id int64, offset int, states ...string) ([]LocalImportItem, error) {
	owner, err := s.DB.LocalImportOwner(id)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("分页位置无效")
	}
	state := ""
	if len(states) > 0 {
		state = states[0]
	}
	if state != "" && state != "all" && state != "failed" {
		return nil, errors.New("导入明细范围无效")
	}
	rows, err := s.DB.Query(`SELECT id,path,size,version,state,file_ids,error FROM local_import_items WHERE batch_id=? AND (?!='failed' OR state='failed') ORDER BY (state='failed') DESC,id LIMIT 100 OFFSET ?`, id, state, offset)
	if err != nil {
		return nil, err
	}
	out := []LocalImportItem{}
	for rows.Next() {
		var v LocalImportItem
		var raw string
		if err = rows.Scan(&v.ID, &v.Path, &v.Size, &v.Version, &v.State, &raw, &v.Error); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &v.FileIDs); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].State == "duplicate" && len(out[i].FileIDs) > 0 {
			_ = s.DB.QueryRow(`SELECT a.name FROM files f JOIN albums a ON a.id=f.album_id WHERE f.id=? AND f.owner_id=?`, out[i].FileIDs[0], owner).Scan(&out[i].ExistingAlbum)
		}
	}
	return out, nil
}

func (s *Service) RunLocalImportScan(ctx context.Context, id int64) (retErr error) {
	span := s.DB.Performance().Start(ctx, "stage", "import_scan")
	defer func() { span.Finish(retErr) }()
	b, err := s.LocalImportBatch(id)
	if err != nil {
		return err
	}
	if b.State != "scanning" {
		return nil
	}
	defer func() {
		if retErr != nil && ctx.Err() == nil {
			_, _ = s.DB.Exec(`UPDATE local_import_batches SET state='failed',error=? WHERE id=? AND state='scanning'`, retErr.Error(), id)
		}
	}()
	if !s.DB.AccountActive(b.OwnerID) {
		return errors.New("目标相册的账号已停用或不存在")
	}
	root, err := openImportRoot(b.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	allowed := map[string]bool{}
	for _, ext := range s.AllowedExts() {
		allowed[strings.ToLower(ext)] = true
	}
	for _, source := range b.Paths {
		if _, err = importInfo(root, source); err != nil {
			return err
		}
		err = fs.WalkDir(root.FS(), source, func(p string, d fs.DirEntry, walkErr error) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			var state string
			if e := s.DB.QueryRow(`SELECT state FROM local_import_batches WHERE id=?`, id).Scan(&state); e != nil {
				return e
			}
			if state != "scanning" {
				return fs.SkipAll
			}
			if walkErr != nil {
				_, e := s.DB.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,error) VALUES(?,?,0,'','skipped',?) ON CONFLICT DO NOTHING`, id, p, walkErr.Error())
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if p != source && !b.Recursive {
					return fs.SkipDir
				}
				return nil
			}
			st, e := d.Info()
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() {
				return nil
			}
			itemState, reason := "pending", ""
			if !sourceMedia(SourceEntry{Name: d.Name()}) || !allowed[strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")] {
				itemState, reason = "skipped", "格式不支持或未在上传策略中启用"
			}
			if st.Size() == 0 {
				itemState, reason = "skipped", "空文件"
			}
			if limit := s.MaxSizeMB(); itemState == "pending" && limit > 0 && st.Size() > int64(limit)<<20 {
				itemState, reason = "failed", fmt.Sprintf("文件超过大小限制 %d MB，请调整网页设置后重试", limit)
			}
			_, e = s.DB.Exec(`INSERT INTO local_import_items(batch_id,path,size,version,state,error) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, id, p, st.Size(), localVersion(st), itemState, reason)
			return e
		})
		if err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(`UPDATE local_import_batches SET state='ready',error='' WHERE id=? AND state='scanning'`, id)
	return err
}

func (s *Service) ChangeLocalImport(id int64, action string) error {
	b, err := s.LocalImportBatch(id)
	if err != nil {
		return err
	}
	switch action {
	case "cancel":
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`UPDATE local_import_batches SET state='canceled' WHERE id=? AND state NOT IN ('done','done_errors','canceled')`, id); e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE tasks SET cancel_requested=1,state=CASE WHEN state='running' THEN state ELSE 'canceled' END,updated_at=unixepoch()
 WHERE type IN('local_import_scan','local_import','local_import_idle') AND state IN('pending','running','failed')
 AND CASE WHEN json_valid(payload) THEN CAST(json_extract(payload,'$.batch_id') AS INTEGER) END=?`, id); e != nil {
			return e
		}
		return tx.Commit()
	case "pause":
		_, err = s.DB.Exec(`UPDATE local_import_batches SET state='paused' WHERE id=? AND state='running'`, id)
		return err
	case "start", "resume", "retry":
		if action == "start" && b.State != "ready" || action == "resume" && b.State != "paused" || action == "retry" && b.State != "done_errors" && b.State != "failed" && b.State != "canceled" {
			return errors.New("当前批次不能执行此操作")
		}
		if action == "retry" && b.State == "failed" {
			if _, err = s.DB.Exec(`UPDATE local_import_batches SET state='scanning',error='' WHERE id=?`, id); err != nil {
				return err
			}
			_, err = s.DB.EnqueueUniqueTask(dbx.TaskLocalImportScan, "", dbx.JSONValue(map[string]any{"batch_id": id}))
			return err
		}
		if action == "retry" {
			items, e := s.failedImportFiles(id)
			if e != nil {
				return e
			}
			for _, f := range items {
				file, e := s.DB.GetVisibleFile(f)
				if e != nil {
					return e
				}
				if file.ArchiveState == "failed" {
					if _, err = s.DB.RetryFileArchive(f); err != nil {
						return err
					}
				}
			}
			if _, err = s.DB.Exec(`UPDATE local_import_items SET state=CASE WHEN file_ids='[]' THEN 'pending' ELSE 'archiving' END,error='' WHERE batch_id=? AND state='failed'`, id); err != nil {
				return err
			}
		}
		typ := dbx.TaskLocalImport
		if b.Schedule == "idle" {
			typ = dbx.TaskLocalImportIdle
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`UPDATE local_import_batches SET state='running',error='' WHERE id=?`, id); err != nil {
			return err
		}
		payload := dbx.JSONValue(map[string]any{"batch_id": id})
		if _, err = tx.Exec(`INSERT INTO tasks(type,payload,state,run_after,created_at,updated_at) SELECT ?,?,'pending',unixepoch(),unixepoch(),unixepoch() WHERE NOT EXISTS(SELECT 1 FROM tasks WHERE type=? AND payload=? AND state IN ('pending','running'))`, typ, payload, typ, payload); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE tasks SET run_after=unixepoch() WHERE type=? AND payload=? AND state='pending'`, typ, payload); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("未知导入操作")
	}
}

func (s *Service) failedImportFiles(id int64) ([]string, error) {
	rows, err := s.DB.Query(`SELECT file_ids FROM local_import_items WHERE batch_id=? AND state='failed'`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var ids []string
		if err = json.Unmarshal([]byte(raw), &ids); err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return out, rows.Err()
}

type importGuard struct {
	ctx           context.Context
	file          *os.File
	root          *os.Root
	path, version string
}

func (g importGuard) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := g.file.Read(p)
	if err == io.EOF {
		st, e := g.file.Stat()
		current, e2 := importInfo(g.root, g.path)
		if e != nil || e2 != nil || localVersion(st) != g.version || !os.SameFile(st, current) {
			return n, ErrSourceChanged
		}
	}
	return n, err
}

// One durable step at a time bounds disk usage. Archive tasks finish the
// accepted source before this batch admits another source (including videos).
func (s *Service) RunLocalImport(ctx context.Context, id int64) (retErr error) {
	span := s.DB.Performance().Start(ctx, "stage", "import_step")
	defer func() {
		err := retErr
		if errors.Is(err, ErrLocalImportPending) || errors.Is(err, ErrLocalImportWaiting) {
			err = nil
		}
		span.Finish(err)
	}()
	if !s.localImportMu.TryLock() {
		return ErrLocalImportWaiting
	}
	defer s.localImportMu.Unlock()
	b, err := s.localImportBatch(id, false)
	if err != nil {
		return err
	}
	if b.State == "paused" {
		return ErrLocalImportWaiting
	}
	if b.State != "running" {
		return nil
	}
	if !s.DB.AccountActive(b.OwnerID) {
		message := "目标相册的账号已停用或不存在"
		_, err := s.DB.Exec(`UPDATE local_import_batches SET state='failed',error=? WHERE id=? AND state='running'`, message, id)
		return errors.Join(errors.New(message), err)
	}
	var item LocalImportItem
	var raw string
	err = s.DB.QueryRow(`SELECT id,path,size,version,state,file_ids FROM local_import_items WHERE batch_id=? AND state='archiving' ORDER BY id LIMIT 1`, id).Scan(&item.ID, &item.Path, &item.Size, &item.Version, &item.State, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.DB.QueryRow(`SELECT id,path,size,version,state,file_ids FROM local_import_items WHERE batch_id=? AND state='pending' ORDER BY id LIMIT 1`, id).Scan(&item.ID, &item.Path, &item.Size, &item.Version, &item.State, &raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		state := "done"
		var failed bool
		if err = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM local_import_items WHERE batch_id=? AND state='failed')`, id).Scan(&failed); err != nil {
			return err
		}
		if failed {
			state = "done_errors"
		}
		_, err = s.DB.Exec(`UPDATE local_import_batches SET state=? WHERE id=? AND state='running'`, state, id)
		return err
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(raw), &item.FileIDs); err != nil {
		return err
	}
	finish := func(state, reason string, ids []string) error {
		if ids == nil {
			ids = []string{}
		}
		_, e := s.DB.Exec(`UPDATE local_import_items SET state=?,error=?,file_ids=? WHERE id=?`, state, reason, dbx.JSONValue(ids), item.ID)
		if e != nil {
			return e
		}
		return ErrLocalImportPending
	}
	if item.State == "archiving" {
		for _, fileID := range item.FileIDs {
			f, e := s.DB.GetVisibleFile(fileID)
			if e != nil {
				if !errors.Is(e, dbx.ErrNotFound) {
					return e
				}
				return finish("failed", "导入文件记录不可用", item.FileIDs)
			}
			if f.ArchiveState == "failed" {
				return finish("failed", f.ArchiveError.String, item.FileIDs)
			}
			if f.ArchiveState != "done" {
				return ErrLocalImportWaiting
			}
		}
		return finish("done", "", item.FileIDs)
	}
	sourceKey := fmt.Sprintf("local-import:%d:%d", id, item.ID)
	parts, err := s.DB.TelegramSourceParts(sourceKey)
	if err != nil {
		return err
	}
	if len(parts) > 0 {
		ids := []string{}
		for _, p := range parts {
			if p.PurgedAt > 0 {
				return finish("skipped", "之前接收的文件已删除，未自动重新导入", nil)
			}
			ids = append(ids, p.FileID)
		}
		return finish("archiving", "", ids)
	}
	root, err := openImportRoot(b.Root)
	if err != nil {
		return finish("failed", err.Error(), nil)
	}
	defer root.Close()
	st, err := importInfo(root, item.Path)
	if err != nil {
		return finish("skipped", err.Error(), nil)
	}
	if localVersion(st) != item.Version {
		return finish("skipped", "来源已变化，请重新扫描后导入", nil)
	}
	f, err := root.Open(item.Path)
	if err != nil {
		return finish("failed", err.Error(), nil)
	}
	defer f.Close()
	ctx = dbx.WithOwner(WithPrivateUpload(ctx), b.OwnerID)
	if created, ok := mediax.FileCreatedTime(f); ok {
		ctx = WithFileCreatedTime(ctx, created)
	}
	ctx = context.WithValue(ctx, telegramSourceKey{}, sourceKey)
	ctx = context.WithValue(ctx, idleIntakeContext{}, true)
	var input io.Reader = importGuard{ctx, f, root, item.Path, item.Version}
	if b.Schedule == "idle" {
		input = s.idleReader(ctx, input)
	}
	files, err := s.ReceiveUploadBatch(ctx, input, path.Base(item.Path), strings.TrimPrefix(path.Ext(item.Path), "."), sql.NullInt64{Int64: b.AlbumID, Valid: b.AlbumID > 0}, nil, 0, !b.Encrypted)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, dbx.ErrImagePending) {
			return ErrLocalImportWaiting
		}
		if errors.Is(err, ErrSourceChanged) {
			return finish("skipped", err.Error(), nil)
		}
		return finish("failed", err.Error(), nil)
	}
	ids := []string{}
	duplicate := len(files) > 0
	for _, file := range files {
		ids = append(ids, file.ID)
		duplicate = duplicate && file.UploadDuplicate
	}
	if duplicate {
		return finish("duplicate", "相同内容已存在，保留原相册", ids)
	}
	return finish("archiving", "", ids)
}

func (s *Service) QueueLocalImports() error {
	rows, err := s.DB.Query(`SELECT id,state,schedule FROM local_import_batches WHERE state IN ('scanning','running','paused')`)
	if err != nil {
		return err
	}
	type pending struct {
		id              int64
		state, schedule string
	}
	list := []pending{}
	for rows.Next() {
		var v pending
		if err = rows.Scan(&v.id, &v.state, &v.schedule); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		typ := dbx.TaskLocalImport
		if v.state == "scanning" {
			typ = dbx.TaskLocalImportScan
		} else if v.schedule == "idle" {
			typ = dbx.TaskLocalImportIdle
		}
		if _, err = s.DB.EnqueueUniqueTask(typ, "", dbx.JSONValue(map[string]any{"batch_id": v.id})); err != nil {
			return err
		}
	}
	return nil
}
