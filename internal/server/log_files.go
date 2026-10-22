package server

import (
	"net/http"
	"path"
	"pimg/internal/dbx"
	"strconv"
)

// The dialog lists sources, including rejected inputs with no gallery ID.
// Worker steps are not a substitute for the submitted file list.
func (s *Server) handleLogFiles(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	page := atoiOr(r.URL.Query().Get("page"), 1)
	if err != nil || id < 1 || page < 1 || page > 1000000 {
		fail(w, 400, "操作编号或页码无效")
		return
	}
	group, err := s.DB.GetTaskGroup(id)
	if err != nil {
		fail(w, 404, "操作不存在")
		return
	}
	context, err := s.DB.ActivityContext(group)
	if err != nil {
		fail(w, 500, "读取操作内容失败")
		return
	}
	items := []dbx.TaskOperationItem{}
	total := 0
	if group.Group != nil && group.Group.BatchID > 0 {
		batch, e := s.Svc.LocalImportBatch(group.Group.BatchID)
		if e != nil {
			fail(w, 500, "读取导入记录失败")
			return
		}
		for _, n := range batch.Counts {
			total += int(n)
		}
		sources, e := s.Svc.LocalImportItems(batch.ID, (page-1)*100)
		if e != nil {
			fail(w, 500, "读取导入文件失败")
			return
		}
		for _, source := range sources {
			item := dbx.TaskOperationItem{Key: strconv.FormatInt(source.ID, 10), FileName: source.Path, State: source.State, Error: source.Error}
			if len(source.FileIDs) > 0 {
				item.FileID = source.FileIDs[0]
				if _, e := s.DB.GetVisibleFile(item.FileID); e == nil {
					item.FileExists = true
				}
			}
			if item.FileName == "" {
				item.FileName = path.Base(source.Path)
			}
			items = append(items, item)
		}
	} else if group.Group != nil && group.Group.OperationID > 0 {
		items, total, err = s.DB.TaskOperationItems(group.Group.OperationID, group.OwnerID, "all", page, 100)
	} else {
		items, total, err = s.DB.LogTaskFiles(id, page, 100)
	}
	if err != nil {
		fail(w, 500, "读取文件列表失败，请重试")
		return
	}
	ids := []string{}
	for _, item := range items {
		if item.FileID != "" && !item.FileExists {
			ids = append(ids, item.FileID)
		}
	}
	successors, e := s.DB.FileSuccessors(ids)
	if e != nil {
		fail(w, 500, "读取文件新位置失败，请重试")
		return
	}
	for i := range items {
		items[i].FileSuccessor = successors[items[i].FileID]
	}
	next := 0
	if page*100 < total {
		next = page + 1
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "next_page": next, "context": context, "owner_id": group.OwnerID})
}
