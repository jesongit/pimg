package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pimg/internal/dbx"
	"pimg/internal/ids"
	"pimg/internal/store"
)

// fileStorageLocation describes a persisted copy, rather than a provider selected
// for a future upload. Permanent locations have no expires_at.
type fileStorageLocation struct {
	Name       string `json:"name,omitempty"`
	Encrypted  *bool  `json:"encrypted,omitempty"`
	PartCount  int    `json:"part_count,omitempty"`
	SavedParts int    `json:"saved_parts,omitempty"`
	Original   bool   `json:"original,omitempty"`
	State      string `json:"state,omitempty"`
	Role       string `json:"role"`
	Backend    string `json:"backend"`
	Kind       string `json:"kind,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Confirmed  *bool  `json:"confirmed,omitempty"`
}

// fileJSON 管理端文件视图。
func (s *Server) fileJSON(f *dbx.File, tempObjects ...*dbx.TempObject) map[string]any {
	var replicas []dbx.MediaReplica
	if f.MediaVersion == 2 {
		replicas, _ = s.DB.MediaReplicas(f.ID)
	}
	var legacy *dbx.FileArchiveMetadata
	if f.MediaVersion != 2 {
		if all, e := s.DB.FileArchiveMetadata([]string{f.ID}); e == nil {
			legacy = all[f.ID]
		}
	}
	_, accessErr := s.DB.MutationFile(f.ID)
	return s.fileJSONFrom(f, replicas, legacy, accessErr == nil && f.PurgeState.String == "", tempObjects...)
}

func (s *Server) fileJSONFrom(f *dbx.File, replicas []dbx.MediaReplica, legacy *dbx.FileArchiveMetadata, allowLinks bool, tempObjects ...*dbx.TempObject) map[string]any {
	var manifest dbx.MediaReplica
	var localReady bool
	var links *store.AvailableLinks
	if f.MediaVersion == 2 {
		manifest, localReady, links = s.Svc.MediaPageInfo(f, replicas, tempObjects)
	} else if allowLinks {
		if legacy != nil {
			links = s.Svc.RecordedPageLinks(context.Background(), f, tempObjects, legacy.Chunks)
		} else {
			links = s.Svc.RecordedFileLinks(context.Background(), f, tempObjects...)
		}
	}
	if !allowLinks {
		links = &store.AvailableLinks{Items: make([]store.AvailableLink, 0)}
	}
	locations := make([]fileStorageLocation, 0, 2+len(tempObjects))
	if f.MediaVersion == 2 {
		{
			for _, replica := range replicas {
				count, saved := 0, 0
				original := false
				for _, part := range replica.Parts {
					if part.Count > count {
						count = part.Count
					}
					if part.Verified {
						saved++
					}
					original = original || part.Original
				}
				confirmed := replica.State == "ready"
				encrypted := replica.Encrypted
				locations = append(locations, fileStorageLocation{Role: "archive", Backend: replica.InstanceID, Encrypted: &encrypted, PartCount: count, SavedParts: saved, Original: original, State: replica.State, Confirmed: &confirmed})
			}
		}
	} else if f.ArchiveState == "done" {
		if legacy != nil {
			for _, instance := range s.Svc.StorageInstances() {
				if count := legacy.Copies[instance.ID]; count > 0 {
					verified := count == len(legacy.Chunks["data"])
					locations = append(locations, fileStorageLocation{Role: "archive", Backend: instance.ID, Confirmed: &verified})
				}
			}
		}
		if len(locations) == 0 && f.ColdBackend != "" {
			locations = append(locations, fileStorageLocation{Role: "archive", Backend: f.ColdBackend})
		}
	}

	if f.Published && f.PublishedPath.Valid && f.PublishedPath.String != "" && f.HotBackend != "" {
		location := fileStorageLocation{Role: "published", Backend: f.HotBackend}
		if f.MediaVersion == 2 {
			var encrypted bool
			if s.DB.QueryRow(`SELECT encrypted FROM media_publications WHERE file_id=? AND backend=? AND object_key=? AND ready=1`, f.ID, f.HotBackend, f.PublishedPath.String).Scan(&encrypted) == nil {
				location.Encrypted = &encrypted
			}
		}
		locations = append(locations, location)
	}
	for _, obj := range tempObjects {
		confirmed := obj.ReadyAt.Valid
		location := fileStorageLocation{Role: "temporary", Backend: obj.Backend, Kind: obj.Kind, ExpiresAt: obj.ExpiresAt, Confirmed: &confirmed}
		if f.MediaVersion == 2 {
			var encrypted bool
			if s.DB.QueryRow(`SELECT encrypted FROM media_temporary WHERE object_id=?`, obj.ID).Scan(&encrypted) == nil {
				location.Encrypted = &encrypted
			}
		}
		locations = append(locations, location)
	}
	onHome := s.DB.FileExhibited(f.ID)
	if f.MediaVersion == 2 {
		for index := range locations {
			locations[index].Name = s.Svc.StorageInstanceName(locations[index].Backend)
		}
	}
	m := map[string]any{
		"id": f.ID, "owner_id": f.OwnerID, "name": f.Name, "orig_name": f.OrigName, "ext": f.Ext,
		"naming": f.Naming, "file_name": store.TelegramFilename(f, "data", 0),
		"mime": f.Mime, "size": f.Size, "sha256": f.SHA256, "kind": f.Kind,
		"width": f.Width, "height": f.Height, "duration": f.Duration,
		"album_id": nil, "archive_state": f.ArchiveState, "storage": f.Storage,
		"cold_storage": f.ColdBackend, "hot_storage": f.HotBackend,
		"encrypted":                 f.Storage != "direct",
		"permanent_publish_allowed": !f.Hidden && (f.Storage == "direct" || f.MediaVersion == 2),
		"storage_locations":         locations,
		"available_links":           links,
		"archive_error":             f.ArchiveError.String, "has_thumb": f.HasThumb,
		"thumb_w": f.ThumbW, "thumb_h": f.ThumbH, "has_bthumb": f.HasBThumb,
		"published": f.Published, "published_at": f.PublishedAt.Int64,
		"publish_error": f.PublishError.String, "purge_state": f.PurgeState.String,
		"show_home": onHome, "home_visibility": f.HomeVisibility, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt,
		"media_at": f.MediaAt, "media_time_source": f.MediaTimeSource(),
	}
	if f.AlbumID.Valid {
		m["album_id"] = f.AlbumID.Int64
	}
	if f.MediaVersion == 2 {
		m["media_version"] = 2
		m["encryption_requested"] = f.Storage != "direct"
		delete(m, "encrypted")
		m["local_ready"] = localReady
		if len(manifest.Parts) > 0 {
			parts := make([]map[string]any, 0, len(manifest.Parts))
			for _, part := range manifest.Parts {
				parts = append(parts, map[string]any{"index": part.Index, "count": part.Count, "start": part.Start, "duration": part.Duration, "size": part.Size, "mime": part.MIME, "ext": part.Ext, "original": part.Original})
			}
			m["media_parts"] = parts
		}
	}
	if f.Published {
		m["url"] = s.Svc.PublicURL(f)
		m["thumb_url"] = privateContentURL(f.ID, "thumb")
		if f.HasBThumb {
			m["blur_url"] = s.Svc.BThumbURL(f)
		}
	}
	return m
}

// homeAlbumID 返回固定游客相册 ID（无则 0）。
func (s *Server) homeAlbumID() int64 { return s.guestAlbumID }

// ---------- 上传 ----------

// handleUpload multipart 接收。查询参数：
//
//	encrypt=true 显式加密；缺省=明文私有。旧 mode=direct 保持公开直发语义。
//	album_id=   归入相册（可缺省=不进任何相册；首页只展示游客相册内已发布的文件）
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// 请求体上限 = 单文件上限 × 2 + 32MB（多文件顺序接收，每文件单独限流）
	maxMB := s.Svc.MaxSizeMB()
	if maxMB > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxMB)*2<<20+32<<20)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "请求必须是 multipart/form-data")
		return
	}
	var albumID sql.NullInt64
	if v := r.URL.Query().Get("album_id"); v != "" {
		a, err := strconv.ParseInt(v, 10, 64)
		if err != nil || a < 0 {
			fail(w, 400, "相册编号无效")
			return
		}
		albumID = sql.NullInt64{Int64: a, Valid: a > 0}
	}
	owner, err := s.uploadOwner(r, albumID.Int64)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	r = r.WithContext(dbx.WithOwner(r.Context(), owner))
	direct, private := true, true
	if mode := r.URL.Query().Get("mode"); mode != "" {
		direct, private = mode == "direct", false
	}
	if raw := r.URL.Query().Get("encrypt"); raw != "" {
		if raw != "true" && raw != "false" {
			fail(w, 400, "encrypt 须为 true 或 false")
			return
		}
		if r.URL.Query().Get("mode") != "" {
			fail(w, 400, "encrypt 不能与旧版 mode 同时指定")
			return
		}
		direct, private = raw == "false", true
	}
	wait := r.URL.Query().Get("wait") == "1"
	var operation *dbx.TaskOperation
	legacyOperation := r.URL.Query().Get("operation_id") == ""
	if raw := r.URL.Query().Get("operation_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "操作编号无效")
			return
		}
		operation, err = s.checkedOperation(r, id, "upload", operationScope("upload", albumID.Int64, !direct))
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
	} else {
		operation, err = s.DB.BeginTaskOperation(dbx.ContextOwner(r.Context()), "upload", ids.New(), operationScope("upload", albumID.Int64, !direct), 1)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		defer s.DB.SealTaskOperation(operation.ID, operation.OwnerID)
	}
	itemKey := r.URL.Query().Get("item_key")
	if len(itemKey) > 128 {
		fail(w, 400, "文件操作标识过长")
		return
	}
	partIndex := 0
	// 相册归属完全由 album_id 决定：未指定=不进任何相册。
	// 直发即发布，但首页只展示游客相册内的已发布文件——
	// 要上首页须显式指定游客相册（或事后移入并发布）。
	var results []map[string]any
	var sourceCreated time.Time
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, 400, "读取 multipart 失败: "+err.Error())
			return
		}
		if part.FormName() == "file_created_at" {
			value, err := io.ReadAll(io.LimitReader(part, 129))
			part.Close()
			if err != nil || len(value) > 128 {
				fail(w, 400, "原文件创建时间无效")
				return
			}
			sourceCreated, err = time.Parse(time.RFC3339Nano, string(value))
			if err != nil || sourceCreated.Year() < 1900 {
				fail(w, 400, "原文件创建时间须为带时区的 ISO 8601 时间")
				return
			}
			continue
		}
		if part.FormName() != "files" && part.FormName() != "file" {
			continue
		}
		fname := part.FileName()
		if fname == "" {
			part.Close()
			continue
		}
		ext := ""
		if i := strings.LastIndexByte(fname, '.'); i >= 0 {
			ext = fname[i+1:]
		}
		ctx := r.Context()
		key := itemKey
		if key == "" {
			key = ids.New()
		} else if partIndex > 0 {
			key = fmt.Sprintf("%s:%d", key, partIndex)
		}
		partIndex++
		if legacyOperation && partIndex > 1 {
			res, e := s.DB.Exec(`UPDATE task_operations SET expected_files=? WHERE id=? AND owner_id=? AND input_state='open'`, partIndex, operation.ID, operation.OwnerID)
			if e != nil {
				part.Close()
				fail(w, 500, "无法接收上传批次")
				return
			}
			if n, _ := res.RowsAffected(); n != 1 {
				part.Close()
				fail(w, 409, dbx.ErrTaskOperationClosed.Error())
				return
			}
		}
		ctx = store.WithUploadOperation(ctx, operation.ID, key)
		if private {
			ctx = store.WithPrivateUpload(ctx)
		}
		if !sourceCreated.IsZero() {
			ctx = store.WithFileCreatedTime(ctx, sourceCreated)
		}
		sourceCreated = time.Time{}
		files, err := s.Svc.ReceiveUploadBatch(ctx, part, fname, ext, albumID, nil, 0, direct)
		if err != nil {
			if errors.Is(err, dbx.ErrTaskOperationClosed) {
				part.Close()
				fail(w, 409, err.Error())
				return
			}
			body, _ := json.Marshal(map[string]any{"source": "pimg", "orig_name": fname, "requested_album_id": albumID.Int64, "operation_id": operation.ID})
			if logErr := s.DB.RecordIngestEvent(dbx.IngestEvent{SourceKey: "rejected:" + ids.New(), Type: "failed", Reason: err.Error(), Body: body}); logErr != nil {
				s.Log.Error("记录上传失败", "error", logErr)
			}
			results = append(results, map[string]any{"orig_name": fname, "error": err.Error()})
			if logErr := s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, key, "", fname, "failed", err.Error()); logErr != nil {
				part.Close()
				fail(w, 409, logErr.Error())
				return
			}
			part.Close()
			continue
		}
		part.Close()
		for _, f := range files {
			state := "queued"
			if f.UploadDuplicate {
				state = "skipped"
			}
			if err := s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, key, f.ID, fname, state, ""); err != nil {
				fail(w, 409, err.Error())
				return
			}
		}
		s.Eng.Wake()
		for _, f := range files {
			duplicate := f.UploadDuplicate
			if wait {
				deadline := time.Now().Add(180 * time.Second)
				for time.Now().Before(deadline) {
					cur, err := s.DB.GetVisibleFile(f.ID)
					if err != nil {
						break
					}
					if cur.ArchiveState == "done" || cur.ArchiveState == "failed" {
						f = cur
						break
					}
					time.Sleep(700 * time.Millisecond)
				}
			}
			s.Eng.Wake()
			result := s.fileJSON(f)
			result["duplicate"] = duplicate
			results = append(results, result)
		}
	}
	if partIndex == 0 {
		fail(w, 400, "请选择需要上传的文件")
		return
	}
	writeJSON(w, 201, map[string]any{"files": results, "operation_id": operation.ID, "task_id": operation.TaskID})
}

// ---------- 列表/详情 ----------

func (s *Server) listFileFilter(r *http.Request) (dbx.FileFilter, error) {
	q := r.URL.Query()
	albumID := int64(0)
	if v := q.Get("album_id"); v != "" && v != "0" {
		fmt.Sscanf(v, "%d", &albumID)
	}
	filter := dbx.FileFilter{
		Query: q.Get("q"), Kind: q.Get("kind"), State: q.Get("state"), Encryption: q.Get("encryption"),
		AlbumID: albumID, HomeID: s.homeAlbumID(),
		Page: atoiOr(q.Get("page"), 1), PerPage: atoiOr(q.Get("per_page"), 40),
		Sort: q.Get("sort"), Seed: q.Get("seed"),
	}
	if filter.Encryption != "" && filter.Encryption != "encrypted" && filter.Encryption != "plaintext" {
		return filter, fmt.Errorf("加密筛选参数无效")
	}
	if len(filter.Seed) > 128 {
		return filter, fmt.Errorf("随机排序参数过长")
	}
	for key, target := range map[string]*int64{"before": &filter.Before, "after": &filter.After} {
		if value := q.Get(key); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < -2208988800 || parsed > 253402300800 {
				return filter, fmt.Errorf("日期定位参数无效")
			}
			*target = parsed
			if key == "before" {
				filter.BeforeSet = true
			} else {
				filter.AfterSet = true
			}
		}
	}
	if filter.BeforeSet && filter.AfterSet && filter.After >= filter.Before {
		return filter, fmt.Errorf("日期范围无效")
	}
	owner, err := s.galleryOwner(r)
	if err != nil {
		return filter, err
	}
	filter.OwnerID = owner
	return filter, nil
}

func (s *Server) handleFileTimeline(w http.ResponseWriter, r *http.Request) {
	filter, err := s.listFileFilter(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "month"
	}
	if group != "day" && group != "week" && group != "month" && group != "year" {
		fail(w, 400, "日期分区无效")
		return
	}
	offset := 0
	if value := r.URL.Query().Get("tz_offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < -840 || offset > 840 {
			fail(w, 400, "时区偏移无效")
			return
		}
	}
	months, err := s.DB.FileTimelineGroups(filter, offset, group)
	if err != nil {
		fail(w, 500, "读取日期索引失败")
		return
	}
	writeJSON(w, 200, map[string]any{"months": months, "groups": months, "group": group})
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	filter, err := s.listFileFilter(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	files, total, err := s.DB.ListFiles(filter)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(files))
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.ID
	}
	operations, err := s.DB.LatestFileOperations(ids)
	if err != nil {
		fail(w, 500, "读取文件任务失败")
		return
	}
	conversions, err := s.DB.FileConversions(ids)
	if err != nil {
		fail(w, 500, "读取转换状态失败")
		return
	}
	tempObjects, err := s.DB.LiveTempObjects(ids, time.Now().Unix())
	if err != nil {
		fail(w, 500, "读取文件存储位置失败")
		return
	}
	var modernIDs, legacyIDs []string
	for _, file := range files {
		if file.MediaVersion == 2 {
			modernIDs = append(modernIDs, file.ID)
		} else {
			legacyIDs = append(legacyIDs, file.ID)
		}
	}
	replicas, err := s.pageMediaReplicas(modernIDs)
	if err != nil {
		fail(w, 500, "读取媒体存储位置失败")
		return
	}
	legacyMetadata, err := s.DB.FileArchiveMetadata(legacyIDs)
	if err != nil {
		fail(w, 500, "读取归档位置失败")
		return
	}
	for _, f := range files {
		conversion := conversions[f.ID]
		item := s.fileJSONFrom(f, replicas[f.ID], legacyMetadata[f.ID], f.PurgeState.String == "" && (conversion == nil || conversion.Phase == "done"), tempObjects[f.ID]...)
		if strings.HasPrefix(r.URL.Path, "/api/account/") && canPreview(f) {
			item["thumb_url"] = requestContentURL(r, f.ID, "thumb")
		}
		attachOperation(item, operations[f.ID])
		attachConversion(item, conversions[f.ID])
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) pageMediaReplicas(ids []string) (map[string][]dbx.MediaReplica, error) {
	all, err := s.DB.FileMediaReplicas(ids)
	var syntax *json.SyntaxError
	var kind *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &syntax) && !errors.As(err, &kind) {
		return all, err
	}
	// Preserve list availability when one historical record is malformed.
	// Archive reads and writes still use the strict database loader.
	all = make(map[string][]dbx.MediaReplica, len(ids))
	for _, id := range ids {
		replicas, e := s.DB.MediaReplicas(id)
		if e == nil {
			all[id] = replicas
		} else if !errors.As(e, &syntax) && !errors.As(e, &kind) {
			return nil, e
		}
	}
	return all, nil
}

func (s *Server) handleFileNavigation(w http.ResponseWriter, r *http.Request) {
	filter, err := s.listFileFilter(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	position, err := s.DB.FileNavigation(filter, r.URL.Query().Get("id"))
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			fail(w, 404, "文件不在当前筛选结果中")
		} else {
			fail(w, 500, "读取图库顺序失败")
		}
		return
	}
	writeJSON(w, 200, position)
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.GetFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	conversion, conversionErr := s.DB.GetConversion(f.ID)
	if conversionErr != nil && !errors.Is(conversionErr, dbx.ErrNotFound) {
		fail(w, 500, "读取转换状态失败")
		return
	}
	if f.Hidden {
		if conversionErr != nil || conversion.Phase != "done" {
			fileAccessFailure(w, dbx.ErrNotFound)
			return
		}
		if conversion.Kind == dbx.TaskTransferOwner && strings.HasPrefix(r.URL.Path, "/api/account/") {
			fileAccessFailure(w, dbx.ErrNotFound)
			return
		}
		receipt := map[string]any{"id": f.ID, "name": f.Name}
		attachConversion(receipt, conversion)
		writeJSON(w, 200, receipt)
		return
	}
	tempObjects, err := s.DB.LiveTempObjects([]string{f.ID}, time.Now().Unix())
	if err != nil {
		fail(w, 500, "读取文件存储位置失败")
		return
	}
	// 附带分片与任务状态（归档/发布透明度）
	out := s.fileJSON(f, tempObjects[f.ID]...)
	if strings.HasPrefix(r.URL.Path, "/api/account/") && canPreview(f) {
		out["thumb_url"] = requestContentURL(r, f.ID, "thumb")
	}
	refs, err := s.DB.FileTaskReferences(f.ID, 0)
	if err != nil {
		fail(w, 500, "读取任务引用失败")
		return
	}
	out["task_references"] = refs
	attachConversion(out, conversion)
	operations, err := s.DB.LatestFileOperations([]string{f.ID})
	if err != nil {
		fail(w, 500, "读取文件任务失败")
		return
	}
	attachOperation(out, operations[f.ID])
	if f.PurgeState.String != "" {
		var message string
		s.DB.QueryRow(`SELECT COALESCE(last_error,'') FROM tasks WHERE file_id=? AND type=? ORDER BY id DESC LIMIT 1`, f.ID, dbx.TaskPurgeFile).Scan(&message)
		out["purge_error"] = message
	}
	chunks := map[string]int{}
	for _, kind := range []string{"data", "thumb", "bthumb"} {
		cs, _ := s.DB.ListChunks(f.ID, kind)
		chunks[kind] = len(cs)
	}
	out["chunks"] = chunks
	if t, err := s.DB.FindActiveTask(dbx.TaskTempMaterialize, f.ID); err == nil {
		out["temp_task_id"] = t.ID
	}
	writeJSON(w, 200, out)
}

func attachOperation(item map[string]any, task *dbx.Task) {
	if task != nil {
		item["operation"] = map[string]any{"id": task.ID, "type": task.Type, "state": task.State, "error": task.LastError.String, "result": jsonOrNull(task.Result.String)}
	}
}

func attachConversion(item map[string]any, c *dbx.Conversion) {
	if c == nil {
		return
	}
	item["conversion"] = map[string]any{
		"kind": c.Kind, "target_owner_id": c.TargetOwnerID, "target_album_id": c.TargetAlbumID,
		"target_encrypted": c.TargetEncrypted, "phase": c.Phase, "state": c.State,
		"error": c.LastError, "target_file_id": c.TargetID, "task_id": c.TaskID,
		"cleanup_pending": c.CleanupPending, "cleanup_error": c.CleanupError,
	}
}

func fileAccessFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, dbx.ErrConverting) || errors.Is(err, dbx.ErrDeleting) || errors.Is(err, dbx.ErrTelegramMoving) || errors.Is(err, dbx.ErrTaskReferenced) {
		fail(w, 409, err.Error())
	} else if errors.Is(err, dbx.ErrNotFound) {
		fail(w, 404, "文件不存在")
	} else {
		fail(w, 500, "文件暂时无法读取")
	}
}

func (s *Server) handleConvertFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Encrypted *bool  `json:"encrypted"`
		Confirm   string `json:"confirm"`
	}
	if err := readJSONBody(r, &request, 1024); err != nil || request.Encrypted == nil || request.Confirm != "CONVERT" {
		fail(w, 400, "请指定 encrypted 布尔值并确认 confirm=CONVERT")
		return
	}
	conversion, err := s.Svc.QueueConversion(r.Context(), r.PathValue("id"), *request.Encrypted)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) || errors.Is(err, dbx.ErrConverting) || errors.Is(err, dbx.ErrDeleting) {
			fileAccessFailure(w, err)
		} else {
			fail(w, 409, err.Error())
		}
		return
	}
	s.Eng.Wake()
	writeJSON(w, 202, map[string]any{"task_id": conversion.TaskID})
}

// Only the administrator route exposes this operation. Account requests retain
// their existing owner/album restrictions, irrespective of confirmation text.
func (s *Server) handleTransferFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AlbumID int64  `json:"album_id"`
		Confirm string `json:"confirm"`
	}
	if err := readJSONBody(r, &request, 1024); err != nil || request.AlbumID <= 0 || request.Confirm != "TRANSFER_OWNER" {
		fail(w, 400, "请选择目标相册，并确认 confirm=TRANSFER_OWNER；新归档默认私有，旧链接失效")
		return
	}
	conversion, err := s.Svc.QueueOwnerTransfer(r.Context(), r.PathValue("id"), request.AlbumID, 0)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			fileAccessFailure(w, err)
		} else {
			fail(w, 409, err.Error())
		}
		return
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 202, map[string]any{"task_id": conversion.TaskID, "target_file_id": conversion.TargetID})
}

func (s *Server) handleFileLinks(w http.ResponseWriter, r *http.Request) {
	links, err := s.Svc.FileLinks(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, dbx.ErrNotFound):
			fileAccessFailure(w, err)
		case errors.Is(err, dbx.ErrDeleting):
			fail(w, 409, "正在删除，外链获取已停止")
		case errors.Is(err, dbx.ErrConverting):
			fail(w, 409, dbx.ErrConverting.Error())
		default:
			fail(w, 500, "可用链接暂时无法读取，请重试。")
		}
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, links)
}

// handleDownload serves the archived original as an authenticated attachment.
// This avoids confusing a thumbnail preview with a download and needs no CORS
// permission on the optional remote hot-storage domain.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.GetVisibleFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.ArchiveState != "done" || f.PurgeState.String != "" {
		fail(w, 409, "原件尚未就绪或正在删除")
		return
	}
	if f.MediaVersion == 2 {
		s.serveMediaOriginal(w, r, f, true)
		return
	}
	source, err := s.Svc.ViewOriginal(r.Context(), f)
	if err != nil {
		fail(w, 500, "无法读取归档原件，请稍后重试")
		return
	}
	defer source.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": store.MediaFilename(f)}))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, store.MediaFilename(f), time.Time{}, source)
}

type patchFileReq struct {
	Name     string `json:"name"`
	AlbumID  *int64 `json:"album_id"`
	ShowHome *bool  `json:"show_home"`
}

// Moving changes album membership. Public thumbnail access follows the target
// album's display grant; original publication remains an independent action.
func (s *Server) handlePatchFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.MutationFile(id); err != nil {
		fileAccessFailure(w, err)
		return
	}
	var req patchFileReq
	if err := readJSONBody(r, &req, 8192); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if req.ShowHome != nil {
		if req.Name != "" || req.AlbumID != nil {
			fail(w, 400, "公开设置请单独提交")
			return
		}
		if err := s.DB.SetFileHomeVisibility(id, *req.ShowHome, 0); err != nil {
			fileAccessFailure(w, err)
			return
		}
		fresh, err := s.DB.GetVisibleFile(id)
		if err != nil {
			fileAccessFailure(w, err)
			return
		}
		writeJSON(w, 200, s.fileJSON(fresh))
		return
	}
	var album sql.NullInt64
	if req.AlbumID != nil {
		if current, err := s.DB.GetVisibleFile(id); err == nil && s.Svc.HasTelegramMedia(current) {
			task, err := s.Svc.QueueTelegramMove(r.Context(), id, *req.AlbumID)
			if err != nil {
				s.telegramError(w, err)
				return
			}
			if req.Name != "" || req.ShowHome != nil {
				if err = s.DB.UpdateFileMeta(id, req.Name, sql.NullInt64{}, req.ShowHome); err != nil {
					s.telegramError(w, err)
					return
				}
			}
			s.Eng.Wake()
			writeJSON(w, 202, map[string]any{"task_id": task})
			return
		}
		if *req.AlbumID > 0 {
			album = sql.NullInt64{Int64: *req.AlbumID, Valid: true}
		} else {
			album = sql.NullInt64{Int64: 0, Valid: true} // 移出相册
		}
	}
	if err := s.DB.UpdateFileMeta(id, req.Name, album, req.ShowHome); err != nil {
		fileAccessFailure(w, err)
		return
	}
	fresh, err := s.DB.MutationFile(id)
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	writeJSON(w, 200, s.fileJSON(fresh))
}

// ---------- 发布/撤销 ----------

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.DB.MutationFile(id)
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.ArchiveState != "done" {
		fail(w, 409, "归档尚未完成，不能发布")
		return
	}
	if f.MediaVersion != 2 && f.Storage != "direct" {
		fail(w, 409, dbx.ErrPermanentEncrypted.Error())
		return
	}
	if f.PurgeState.String != "" {
		fail(w, 409, dbx.ErrDeleting.Error())
		return
	}
	if f.Published {
		fresh, err := s.DB.MutationFile(id)
		if err != nil {
			fileAccessFailure(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"task_id": nil, "file": s.fileJSON(fresh)})
		return
	}
	if _, err := s.DB.FindActiveTask(dbx.TaskPublish, id); err != nil {
		tid, err := s.DB.EnqueueTask(dbx.TaskPublish, id, "{}")
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.Eng.Wake()
		fresh, err := s.DB.MutationFile(id)
		if err != nil {
			fileAccessFailure(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"task_id": tid, "file": s.fileJSON(fresh)})
		return
	}
	writeJSON(w, 200, map[string]any{"task_id": nil, "file": s.fileJSON(f), "pending": true})
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.MutationFile(id); err != nil {
		fileAccessFailure(w, err)
		return
	}
	tid, err := s.DB.EnqueueTask(dbx.TaskUnpublish, id, "{}")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Eng.Wake()
	writeJSON(w, 200, map[string]any{"task_id": tid})
}

// ---------- 临时查看 ----------

type tempReq struct {
	Kind  string `json:"kind"`  // original|thumb，默认 original
	Share bool   `json:"share"` // explicit opt-in to a shareable temporary copy
}

func (s *Server) handleTempView(w http.ResponseWriter, r *http.Request) {
	f, err := s.DB.MutationFile(r.PathValue("id"))
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.ArchiveState != "done" {
		fail(w, 409, "归档尚未完成")
		return
	}
	kind := "original"
	share := false
	if r.Method == http.MethodPost {
		var req tempReq
		if err := readJSONBody(r, &req, 1024); err != nil && !errors.Is(err, io.EOF) {
			fail(w, 400, "临时链接参数必须是合法 JSON，share 必须为布尔值。")
			return
		} else if err == nil {
			if req.Kind == "thumb" {
				kind = "thumb"
			}
			share = req.Share
		}
	}
	if kind == "thumb" && !canPreview(f) {
		fail(w, 404, "该文件没有缩略图")
		return
	}
	if f.PurgeState.String != "" {
		fail(w, 409, "正在删除，预览已停止")
		return
	}
	// Only an explicit share creates a remote hot copy. Normal browsing uses
	// the local compressed preview and the authenticated original cache.
	if share {
		if kind != "original" {
			fail(w, 400, "缩略图由 Pimg 本地提供，不创建存储副本")
			return
		}
		res, err := s.Svc.GetTempView(r.Context(), f, kind)
		if err != nil {
			fail(w, 500, "临时预览暂时不可用，请重试。")
			return
		}
		s.Eng.Wake()
		writeJSON(w, 200, res)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "url": requestContentURL(r, f.ID, kind), "expires_at": 0})
}

// previews?ids=a,b,c 一次拿多个缩略图临时链接。
// 已发布文件用公开封面 URL；其余按配置使用临时 R2 外链或鉴权接口。
func (s *Server) handlePreviews(w http.ResponseWriter, r *http.Request) {
	owner, err := s.galleryOwner(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	idList := strings.Split(r.URL.Query().Get("ids"), ",")
	out := map[string]any{}
	for _, id := range idList {
		id = strings.TrimSpace(id)
		if id == "" || len(out) >= 120 {
			continue
		}
		f, err := s.DB.GetVisibleFile(id)
		if err != nil || owner != "" && f.OwnerID != owner || !canPreview(f) || f.ArchiveState != "done" || f.PurgeState.String != "" {
			continue
		}
		out[id] = map[string]any{"status": "ready", "url": requestContentURL(r, f.ID, "thumb"), "expires_at": 0}
	}
	writeJSON(w, 200, map[string]any{"previews": out})
}

// ---------- 校验/重试/删除 ----------

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.MutationFile(id); err != nil {
		fileAccessFailure(w, err)
		return
	}
	tid, err := s.DB.EnqueueTask(dbx.TaskVerify, id, "{}")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Eng.Wake()
	writeJSON(w, 200, map[string]any{"task_id": tid})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.DB.MutationFile(id)
	if err != nil {
		fileAccessFailure(w, err)
		return
	}
	if f.ArchiveState == "done" && f.PurgeState.String == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "noop": true})
		return
	}
	if f.PurgeState.String != "" {
		fail(w, 409, dbx.ErrDeleting.Error())
		return
	}
	tid, err := s.DB.RetryFileArchive(id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Eng.Wake()
	writeJSON(w, 200, map[string]any{"task_id": tid})
}

// handleDeleteFile 彻底删除（高风险）：需 confirm=PURGE。
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("confirm") != "PURGE" {
		fail(w, 400, "彻底删除是不可恢复的高风险操作：请传 confirm=PURGE。该操作会删除冷存储原件、热存储副本与索引记录；他人已保存的外链副本无法收回。")
		return
	}
	if _, err := s.DB.MutationFile(id); err != nil {
		fileAccessFailure(w, err)
		return
	}
	operation, err := s.DB.BeginTaskOperation(dbx.ContextOwner(r.Context()), "purge", ids.New(), "", 1)
	if err != nil {
		fail(w, 500, "无法创建删除操作")
		return
	}
	defer s.DB.SealTaskOperation(operation.ID, operation.OwnerID)
	tid, err := s.DB.EnqueuePurgeTask(id, operation.ID)
	if err != nil {
		_ = s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, id, id, "", "failed", err.Error())
		if errors.Is(err, dbx.ErrTaskReferenced) {
			refs, _ := s.DB.FileTaskReferences(id, 0)
			writeJSON(w, 409, map[string]any{"error": map[string]string{"message": err.Error()}, "task_ids": refs})
			return
		}
		fail(w, 500, err.Error())
		return
	}
	if err = s.interruptFileWork(id); err != nil {
		s.Log.Warn("停止待删除文件任务失败", "err", err)
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]any{"task_id": tid, "operation_id": operation.ID, "notice": "已自动取消相关任务，正在删除原件与副本；导入源文件保留"})
}

// ---------- 批量操作 ----------

type batchReq struct {
	Action      string   `json:"action"` // publish|unpublish|show_home|hide_home|move_album|purge|set_media_time|convert
	IDs         []string `json:"ids"`
	AlbumID     int64    `json:"album_id"`
	Confirm     string   `json:"confirm"`
	OperationID int64    `json:"operation_id"`
	MediaAt     *int64   `json:"media_at"`
	Encrypted   *bool    `json:"encrypted"`
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if err := readJSONBody(r, &req, 64<<10); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 200 {
		fail(w, 400, "ids 数量须在 1..200")
		return
	}
	if req.Action == "purge" && req.Confirm != "PURGE" {
		fail(w, 400, "批量彻底删除需要 confirm=PURGE；将删除归档、发布副本和索引，保留服务器导入源文件")
		return
	}
	if req.Action != "publish" && req.Action != "unpublish" && req.Action != "move_album" && req.Action != "show_home" && req.Action != "hide_home" && req.Action != "purge" && req.Action != "set_media_time" && req.Action != "convert" {
		fail(w, 400, "未知批量操作")
		return
	}
	if req.Action == "convert" && (req.Encrypted == nil || req.Confirm != "CONVERT") {
		fail(w, 400, "请指定 encrypted 布尔值并确认 confirm=CONVERT")
		return
	}
	operationScopeValue := operationScope(req.Action, req.AlbumID, req.Encrypted != nil && *req.Encrypted)
	if req.Action == "set_media_time" {
		if req.MediaAt == nil || !dbx.ValidMediaTime(*req.MediaAt) {
			fail(w, 400, "请指定有效的文件日期（精确到秒）")
			return
		}
		operationScopeValue = dbx.MediaDateScope(*req.MediaAt)
	}
	if req.Action == "move_album" && req.AlbumID > 0 {
		var exists int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM albums WHERE id=?`, req.AlbumID).Scan(&exists); err != nil || exists == 0 {
			fail(w, 400, "目标相册不存在")
			return
		}
	}
	var operation *dbx.TaskOperation
	{
		var err error
		if req.OperationID != 0 {
			operation, err = s.checkedOperation(r, req.OperationID, req.Action, operationScopeValue)
		} else {
			operation, err = s.DB.BeginTaskOperation(dbx.ContextOwner(r.Context()), req.Action, ids.New(), operationScopeValue, len(req.IDs))
			if err == nil {
				defer s.DB.SealTaskOperation(operation.ID, operation.OwnerID)
			}
		}
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
	}
	done, skipped := 0, 0
	results := make([]map[string]any, 0, len(req.IDs))
	seen := make(map[string]bool)
	operationID := int64(0)
	if operation != nil {
		operationID = operation.ID
	}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		result := map[string]any{"id": id, "status": "skipped"}
		results = append(results, result)
		f, err := s.DB.MutationFile(id)
		if req.Action == "move_album" || req.Action == "convert" {
			// A repeated chunk may already own a staged transfer. The enqueue
			// transaction verifies its destination and operation membership.
			f, err = s.DB.GetVisibleFile(id)
		}
		if err != nil {
			result["reason"] = "文件不存在或已删除"
			skipped++
			if operation != nil {
				_ = s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, id, id, "", "skipped", "文件不存在或已删除")
			}
			continue
		}
		result["name"] = f.Name
		if f.PurgeState.String != "" {
			result["reason"] = "正在删除或等待清理"
			skipped++
			if operation != nil {
				_ = s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, id, id, f.OrigName, "skipped", "正在删除或等待清理")
			}
			continue
		}
		var actionErr error
		receiptCommitted := false
		ok := func() bool {
			switch req.Action {
			case "convert":
				var conversion *dbx.Conversion
				conversion, actionErr = s.Svc.QueueConversion(r.Context(), id, *req.Encrypted, operationID)
				if errors.Is(actionErr, dbx.ErrConversionNotNeeded) {
					result["reason"] = actionErr.Error()
					actionErr = nil
					return false
				}
				if actionErr == nil {
					receiptCommitted = true
					result["task_id"] = conversion.TaskID
					result["target_file_id"] = conversion.TargetID
				}
			case "set_media_time":
				actionErr = s.DB.SetMediaTimeInOperation(id, *req.MediaAt, operationID)
				receiptCommitted = actionErr == nil
			case "purge":
				var task int64
				task, actionErr = s.DB.EnqueuePurgeTask(id, operationID)
				if actionErr == nil {
					receiptCommitted = true
					result["task_id"] = task
					if e := s.interruptFileWork(id); e != nil {
						s.Log.Warn("停止待删除文件任务失败", "err", e)
					}
				}
			case "publish":
				if f.MediaVersion != 2 && f.Storage != "direct" {
					result["reason"] = dbx.ErrPermanentEncrypted.Error()
					return false
				}
				if f.ArchiveState != "done" || f.Published || f.PurgeState.String != "" {
					result["reason"] = "尚未归档完成或已经发布"
					return false
				}
				if _, err := s.DB.FindActiveTask(dbx.TaskPublish, id); err == nil {
					result["reason"] = "发布任务正在执行"
					return false
				}
				_, actionErr = s.DB.EnqueueOperationTask(dbx.TaskPublish, id, "{}", operationID)
				receiptCommitted = actionErr == nil
			case "unpublish":
				if !f.Published {
					result["reason"] = "文件尚未发布"
					return false
				}
				if _, err := s.DB.FindActiveTask(dbx.TaskUnpublish, id); err == nil {
					result["reason"] = "撤销任务正在执行"
					return false
				}
				_, actionErr = s.DB.EnqueueOperationTask(dbx.TaskUnpublish, id, "{}", operationID)
				receiptCommitted = actionErr == nil
			case "show_home":
				actionErr = s.DB.SetFileHomeVisibility(id, true, operationID)
				receiptCommitted = actionErr == nil
			case "hide_home":
				actionErr = s.DB.SetFileHomeVisibility(id, false, operationID)
				receiptCommitted = actionErr == nil
			case "move_album":
				if req.AlbumID > 0 {
					target, e := s.DB.GetAlbum(req.AlbumID)
					if e != nil {
						actionErr = e
						break
					}
					if target.OwnerID != f.OwnerID {
						if dbx.ContextOwner(r.Context()) != dbx.AdminOwner || !strings.HasPrefix(r.URL.Path, "/api/admin/") || req.Confirm != "TRANSFER_OWNER" {
							actionErr = errors.New("跨用户移动需要管理员确认转移归属（confirm=TRANSFER_OWNER）")
							break
						}
						var transfer *dbx.Conversion
						transfer, actionErr = s.Svc.QueueOwnerTransfer(r.Context(), id, req.AlbumID, operationID)
						if actionErr == nil {
							receiptCommitted = true
							result["task_id"] = transfer.TaskID
							result["target_file_id"] = transfer.TargetID
						}
						break
					}
				}
				if s.Svc.HasTelegramMedia(f) {
					var task int64
					task, actionErr = s.Svc.QueueTelegramMove(r.Context(), id, req.AlbumID, operationID)
					if actionErr == nil {
						receiptCommitted = true
						result["task_id"] = task
						result["status"] = "submitted"
					}
					break
				}
				actionErr = s.DB.MoveFileInOperation(id, req.AlbumID, operationID)
				receiptCommitted = actionErr == nil
			default:
				return false
			}
			return actionErr == nil
		}()
		if ok {
			result["status"] = "done"
			if req.Action == "publish" || req.Action == "unpublish" || result["task_id"] != nil {
				result["status"] = "submitted"
			}
			done++
		} else {
			if actionErr != nil {
				result["status"] = "failed"
				result["reason"] = actionErr.Error()
			}
			skipped++
		}
		if operation != nil && !receiptCommitted {
			state := "queued"
			if result["status"] == "done" {
				state = "done"
			} else if result["status"] == "skipped" {
				state = "skipped"
			} else if result["status"] == "failed" {
				state = "failed"
			}
			reason, _ := result["reason"].(string)
			if err := s.DB.RecordTaskOperationItem(operation.ID, operation.OwnerID, id, id, f.OrigName, state, reason); err != nil {
				if errors.Is(err, dbx.ErrTaskOperationClosed) {
					fail(w, 409, err.Error())
					return
				}
				s.Log.Error("保存批量结果失败", "err", err)
				fail(w, 500, "保存批量操作结果失败，请刷新查看操作状态")
				return
			}
		}
	}
	s.Eng.Wake()
	response := map[string]any{"done": done, "skipped": skipped, "results": results}
	if operation != nil {
		response["operation_id"] = operation.ID
		response["task_id"] = operation.TaskID
	}
	writeJSON(w, 200, response)
}

// ---------- 相册 ----------

func (s *Server) handleListAlbums(w http.ResponseWriter, r *http.Request) {
	owner, err := s.galleryOwner(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	albums, err := s.DB.ListOwnerAlbums(owner)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(albums))
	for _, a := range albums {
		item := map[string]any{
			"id": a.ID, "owner_id": a.OwnerID, "name": a.Name, "count": a.Count,
			"created_at": a.CreatedAt, "is_home": a.IsHome,
			"is_unclassified": a.IsDefault, "displayed": a.Displayed,
		}
		if b, err := s.DB.GetTopicBinding(a.ID); err == nil {
			item["telegram"] = map[string]any{"chat_id": b.ChatID, "topic_id": b.TopicID, "state": b.State, "error": b.LastError, "scan_date": b.ScanDate}
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

type albumReq struct {
	Displayed *bool  `json:"displayed,omitempty"`
	Name      string `json:"name"`
	TopicID   *int   `json:"topic_id,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
}

func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	var req albumReq
	if err := readJSONBody(r, &req, 2048); err != nil || !validAccountAlbumName(req.Name) {
		fail(w, 400, "请填写相册名称")
		return
	}
	var id int64
	var err error
	owner := dbx.AdminOwner
	if req.OwnerID != "" && req.OwnerID != "self" {
		owner = req.OwnerID
	}
	if !s.DB.AccountActive(owner) {
		fail(w, 400, "请选择已启用的用户")
		return
	}
	if owner != dbx.AdminOwner {
		if req.TopicID != nil {
			fail(w, 400, "用户相册的 TG 话题由系统自动分配")
			return
		}
		id, err = s.createAccountAlbum(owner, strings.TrimSpace(req.Name))
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"id": id, "name": strings.TrimSpace(req.Name), "owner_id": owner})
		return
	}
	if req.TopicID != nil && (!s.Svc.UsesStorageDriver("telegram") || !s.Svc.UserTelegramEnabled()) {
		fail(w, 400, "请先启用 Telegram 冷存储，再关联已有话题")
		return
	}
	if s.Svc.UsesStorageDriver("telegram") && s.Svc.UserTelegramEnabled() {
		if req.TopicID != nil {
			id, err = s.Svc.CreateLinkedAlbumFromTopic(r.Context(), strings.TrimSpace(req.Name), *req.TopicID)
		} else {
			id, err = s.Svc.CreateLinkedAlbum(r.Context(), strings.TrimSpace(req.Name))
		}
	} else {
		id, err = s.DB.InsertAlbum(strings.TrimSpace(req.Name))
	}
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	response := map[string]any{"id": id, "name": req.Name, "owner_id": owner}
	if album, e := s.DB.GetAlbum(id); e == nil {
		response["name"] = album.Name
	}
	if task, e := s.DB.LatestTelegramAlbumTask(id, dbx.TaskTelegramRenameAlbum); e == nil {
		response["task_id"] = task.ID
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 201, response)
}

func (s *Server) handlePatchAlbum(w http.ResponseWriter, r *http.Request) {
	var id int64
	fmt.Sscanf(r.PathValue("id"), "%d", &id)
	var req albumReq
	if err := readJSONBody(r, &req, 2048); err != nil {
		fail(w, 400, "无效的相册设置")
		return
	}
	if req.Displayed != nil {
		if req.Name != "" || req.TopicID != nil || req.OwnerID != "" {
			fail(w, 400, "请分别修改展示和相册信息")
			return
		}
		if err := s.DB.SetAlbumDisplayed(id, *req.Displayed); err != nil {
			if errors.Is(err, dbx.ErrNotFound) {
				fail(w, 404, "相册不存在")
			} else {
				fail(w, 500, "展示设置未保存")
			}
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "displayed": *req.Displayed})
		return
	}
	if !validAccountAlbumName(req.Name) {
		fail(w, 400, "请填写相册名称")
		return
	}

	var renameErr error
	linked := false
	if _, err := s.DB.GetTopicBinding(id); err == nil && s.Svc.UserTelegramEnabled() {
		linked = true
		renameErr = s.Svc.RenameLinkedAlbum(r.Context(), id, strings.TrimSpace(req.Name))
	} else {
		renameErr = s.DB.RenameAlbum(id, strings.TrimSpace(req.Name))
	}
	if err := renameErr; err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			fail(w, 404, "相册不存在")
			return
		}
		fail(w, 400, err.Error())
		return
	}
	s.Eng.Wake()
	if linked {
		if task, err := s.DB.LatestTelegramAlbumTask(id, dbx.TaskTelegramRenameAlbum); err == nil {
			writeJSON(w, 202, map[string]any{"ok": true, "task_id": task.ID})
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	var id int64
	fmt.Sscanf(r.PathValue("id"), "%d", &id)
	if _, err := s.DB.GetTopicBinding(id); err == nil && s.Svc.UserTelegramEnabled() {
		task, err := s.Svc.QueueDeleteLinkedAlbum(r.Context(), id)
		if err != nil {
			s.telegramError(w, err)
			return
		}
		s.Eng.Wake()
		writeJSON(w, 202, map[string]any{"task_id": task})
		return
	}
	if err := s.DB.DeleteAlbum(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- 任务 ----------

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("view") != "" {
		s.handleActivityTasks(w, r)
		return
	}
	tasks, err := s.DB.ListFileTasks(r.URL.Query().Get("file_id"), r.URL.Query().Get("state"), atoiOr(r.URL.Query().Get("limit"), 60))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		cancelRequested := false
		if t.State == "running" {
			cancelRequested, _ = s.DB.TaskCancelRequested(t.ID)
		}
		repositoryName := ""
		if t.Type == dbx.TaskColdSync || t.Type == dbx.TaskColdDelete {
			var p struct {
				RepositoryID int64 `json:"repository_id"`
			}
			if json.Unmarshal([]byte(t.Payload), &p) == nil {
				if repo, err := s.DB.GetColdRepository(p.RepositoryID); err == nil {
					repositoryName = repo.Name
				}
			}
		}
		items = append(items, map[string]any{
			"id": t.ID, "type": t.Type, "file_id": t.FileID.String, "state": t.State,
			"attempts": t.Attempts, "max_attempts": t.MaxAttempts,
			"last_error": t.LastError.String, "result": jsonOrNull(t.Result.String),
			"created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
			"cancel_requested": cancelRequested,
			"idle_only":        dbx.IdleTask(t.Type),
			"repository_name":  repositoryName,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func jsonOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	if s.handleGroupedTaskAction(w, r, "retry") {
		return
	}
	var id int64
	fmt.Sscanf(r.PathValue("id"), "%d", &id)
	if err := s.DB.RetryTask(id); err != nil {
		if errors.Is(err, dbx.ErrSystemScheduleManaged) || errors.Is(err, dbx.ErrDeleting) || errors.Is(err, dbx.ErrConverting) || errors.Is(err, dbx.ErrTelegramMoving) || errors.Is(err, dbx.ErrPermanentEncrypted) {
			fail(w, 409, err.Error())
			return
		}
		if errors.Is(err, dbx.ErrNotFound) {
			fail(w, 404, "任务或文件不存在")
			return
		}
		fail(w, 500, err.Error())
		return
	}
	s.Eng.Wake()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var _ = ids.NewShort
