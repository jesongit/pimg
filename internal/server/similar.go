package server

import (
	"errors"
	"net/http"
	"pimg/internal/dbx"
	"pimg/internal/ids"
)

func (s *Server) handleSimilarPurge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AlbumID       int64  `json:"album_id"`
		KeepAlbumID   *int64 `json:"keep_album_id"`
		RemoveAlbumID *int64 `json:"remove_album_id"`
		KeepID        string `json:"keep_id"`
		RemoveID      string `json:"remove_id"`
		KeepSHA       string `json:"keep_sha256"`
		RemoveSHA     string `json:"remove_sha256"`
		Confirm       string `json:"confirm"`
		Comparison    string `json:"comparison"`
	}
	if readJSONBody(r, &req, 4096) != nil || req.Confirm != "PURGE" || req.KeepID == "" || req.RemoveID == "" {
		fail(w, 400, "请选择保留文件，并确认永久删除另一个文件")
		return
	}
	var snapshots []int64
	if req.KeepAlbumID != nil || req.RemoveAlbumID != nil {
		if req.KeepAlbumID == nil || req.RemoveAlbumID == nil || *req.KeepAlbumID < 0 || *req.RemoveAlbumID < 0 || req.AlbumID != 0 {
			fail(w, 400, "缺少对比文件的相册信息，请重新扫描")
			return
		}
		snapshots = []int64{*req.KeepAlbumID, *req.RemoveAlbumID}
	} else if req.AlbumID <= 0 {
		fail(w, 400, "缺少对比文件的相册信息，请重新扫描")
		return
	}
	if req.Comparison != "" && req.Comparison != "image" && req.Comparison != "video_size" {
		fail(w, 400, "不支持的对比方式")
		return
	}
	owner, err := s.galleryOwner(r)
	if err != nil || owner == "" {
		fail(w, 400, "请选择一个用户的相册")
		return
	}
	actor := dbx.ContextOwner(r.Context())
	for _, id := range []string{req.KeepID, req.RemoveID} {
		file, accessErr := s.DB.GetVisibleFile(id)
		if accessErr != nil || file.OwnerID != owner {
			fail(w, 404, "文件不存在或不属于当前相册的用户")
			return
		}
	}
	operation, err := s.DB.BeginTaskOperation(actor, "purge", ids.New(), "", 1)
	if err != nil {
		fail(w, 500, "无法创建删除操作")
		return
	}
	defer s.DB.SealTaskOperation(operation.ID, actor)
	tid, err := s.DB.EnqueueSimilarPurge(actor, owner, req.AlbumID, req.KeepID, req.RemoveID, req.KeepSHA, req.RemoveSHA, req.Comparison, operation.ID, snapshots...)
	if err != nil {
		_ = s.DB.RecordTaskOperationItem(operation.ID, actor, req.RemoveID, req.RemoveID, "", "failed", err.Error())
		reason := "enqueue_failed"
		var conflict *dbx.SimilarPurgeConflict
		if errors.As(err, &conflict) {
			reason = conflict.Reason
		}
		s.Log.Warn("对比文件删除未提交", "operation", "similar_purge", "comparison", req.Comparison, "reason", reason, "album_id", req.AlbumID, "keep_id", req.KeepID, "remove_id", req.RemoveID, "err", err)
		if errors.Is(err, dbx.ErrSimilarChanged) || errors.Is(err, dbx.ErrConverting) || errors.Is(err, dbx.ErrTelegramMoving) {
			fail(w, 409, err.Error())
		} else {
			fail(w, 500, "无法提交删除，请稍后重试")
		}
		return
	}
	if err = s.interruptFileWork(req.RemoveID); err != nil {
		s.Log.Warn("停止待删除文件任务失败", "err", err)
	}
	if s.Eng != nil {
		s.Eng.Wake()
	}
	writeJSON(w, 200, map[string]any{"task_id": tid, "operation_id": operation.ID})
}
