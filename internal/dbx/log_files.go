package dbx

func (db *DB) LogTaskFiles(id int64, page, limit int) ([]TaskOperationItem, int, error) {
	var key string
	if err := db.QueryRow(taskGroupsCTE+` SELECT group_key FROM grouped_tasks WHERE id=?`, id).Scan(&key); err != nil {
		return nil, 0, err
	}
	base := taskGroupsCTE + `, log_files AS (SELECT m.file_id,
 COALESCE(NULLIF(MAX(f.orig_name),''),MAX(f.name),MAX(i.name),m.file_id) name,
 CASE WHEN SUM(m.state='running')>0 THEN 'running' WHEN SUM(m.state='pending')>0 THEN 'pending' WHEN SUM(m.state='failed')>0 THEN 'failed' WHEN SUM(m.state='canceled')>0 THEN 'canceled' ELSE 'done' END state,
 COALESCE(MAX(CASE WHEN m.state='failed' THEN m.last_error END),'') error,MAX(f.id IS NOT NULL AND f.hidden=0) present,COALESCE(MAX(f.owner_id),'') owner_id
 FROM ranked_members m LEFT JOIN files f ON f.id=m.file_id LEFT JOIN task_operation_jobs j ON j.task_id=m.id
 LEFT JOIN task_operation_items i ON i.operation_id=j.operation_id AND i.item_key=j.item_key
 WHERE m.group_key=? AND m.relevance=1 AND COALESCE(m.file_id,'')!='' GROUP BY m.file_id) `
	var total int
	if err := db.QueryRow(base+`SELECT COUNT(*) FROM log_files`, key).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(base+`SELECT file_id,name,state,error,present,owner_id FROM log_files ORDER BY (state='failed') DESC,file_id LIMIT ? OFFSET ?`, key, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []TaskOperationItem{}
	for rows.Next() {
		var item TaskOperationItem
		if err = rows.Scan(&item.FileID, &item.FileName, &item.State, &item.Error, &item.FileExists, &item.OwnerID); err != nil {
			return nil, 0, err
		}
		item.Key = item.FileID
		items = append(items, item)
	}
	return items, total, rows.Err()
}
