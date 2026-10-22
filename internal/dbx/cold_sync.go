package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

const (
	TaskColdSync     = "cold_sync"
	TaskColdDelete   = "cold_delete"
	idleTaskTypesSQL = "'account_delete','local_import_idle','local_layout','storage_probe','archive_sync','source_scan','source_cleanup','cold_sync','cold_delete','purge_file','cleanup_converted','tg_delete_album'"
)

func IdleTask(typ string) bool {
	return typ == TaskAccountDelete || typ == TaskLocalImportIdle || typ == "local_layout" || typ == TaskStorageProbe || typ == TaskArchiveSync || typ == TaskSourceScan || typ == TaskSourceCleanup || typ == TaskColdSync || typ == TaskColdDelete || typ == TaskPurgeFile || typ == TaskCleanupConverted || typ == TaskTelegramDeleteAlbum
}

// PassiveTask identifies automatic Telegram history probes. A probe becomes
// foreground activity only if it actually needs to ingest new media.
func PassiveTask(t *Task) bool {
	if t.Type == TaskSystemSchedule {
		return true
	}
	if t.Type != TaskTelegramScan && t.Type != TaskTelegramReceive {
		return false
	}
	var p struct {
		Automatic bool `json:"automatic"`
	}
	return json.Unmarshal([]byte(t.Payload), &p) == nil && p.Automatic
}

func passiveTaskSQL(alias string) string {
	return `(` + alias + `.type='system_schedule' OR (` + alias + `.type IN ('tg_scan','tg_receive') AND COALESCE(CASE WHEN json_valid(` + alias + `.payload) THEN json_type(` + alias + `.payload,'$.automatic')='true' ELSE 0 END,0)=1))`
}

var ErrTaskReferenced = errors.New("相关操作正在停止，清理任务将自动继续")

// The primary is always the existing cold_storage choice. Only secondary
// instances live here, so replication never changes normal object routing.
type ColdRepository struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Config     string `json:"-"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	Pending    int    `json:"pending"`
	Synced     int    `json:"synced"`
	Failed     int    `json:"failed"`
	Running    int    `json:"running"`
	LastSynced int64  `json:"last_synced"`
	LastError  string `json:"last_error"`
}

type ColdSyncJob struct {
	RepositoryID int64
	FileID       string
	Revision     int64
	Objects      string
}

func (db *DB) AddColdRepository(name, driver, config string) (int64, error) {
	r, err := db.Exec(`INSERT INTO cold_repositories(name,driver,config,created_at) VALUES(?,?,?,?)`, name, driver, config, now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}
func (db *DB) ColdRepositories() ([]ColdRepository, error) {
	rows, err := db.Query(`SELECT r.id,r.name,r.driver,r.config,r.enabled,r.created_at,
 (SELECT COUNT(*) FROM tasks t WHERE t.type IN ('cold_sync','cold_delete') AND t.state='pending' AND json_extract(t.payload,'$.repository_id')=r.id),
 (SELECT COUNT(*) FROM cold_sync_files s WHERE s.repository_id=r.id AND s.done_revision=s.wanted_revision AND s.done_revision>0 AND s.objects!='[]'),
 (SELECT COUNT(*) FROM tasks t WHERE t.type IN ('cold_sync','cold_delete') AND t.state='failed' AND json_extract(t.payload,'$.repository_id')=r.id),
 (SELECT COUNT(*) FROM tasks t WHERE t.type IN ('cold_sync','cold_delete') AND t.state='running' AND json_extract(t.payload,'$.repository_id')=r.id),
 COALESCE((SELECT MAX(s.synced_at) FROM cold_sync_files s WHERE s.repository_id=r.id),0),
 COALESCE((SELECT t.last_error FROM tasks t WHERE t.type IN ('cold_sync','cold_delete') AND json_extract(t.payload,'$.repository_id')=r.id AND t.state IN ('failed','pending') AND t.last_error IS NOT NULL ORDER BY t.updated_at DESC LIMIT 1),'')
 FROM cold_repositories r WHERE r.deleted_at IS NULL ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ColdRepository{}
	for rows.Next() {
		var r ColdRepository
		if err := rows.Scan(&r.ID, &r.Name, &r.Driver, &r.Config, &r.Enabled, &r.CreatedAt, &r.Pending, &r.Synced, &r.Failed, &r.Running, &r.LastSynced, &r.LastError); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (db *DB) GetColdRepository(id int64) (*ColdRepository, error) {
	r := &ColdRepository{}
	err := db.QueryRow(`SELECT id,name,driver,config,enabled,created_at FROM cold_repositories WHERE id=? AND deleted_at IS NULL`, id).Scan(&r.ID, &r.Name, &r.Driver, &r.Config, &r.Enabled, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (db *DB) SetColdRepositoryEnabled(id int64, enabled bool) error {
	r, err := db.Exec(`UPDATE cold_repositories SET enabled=? WHERE id=? AND deleted_at IS NULL`, enabled, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}
func (db *DB) RemoveColdRepository(id int64) error {
	r, err := db.Exec(`UPDATE cold_repositories SET enabled=0,deleted_at=? WHERE id=? AND deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM cold_sync_files s WHERE s.repository_id=cold_repositories.id AND (s.objects!='[]' OR EXISTS(SELECT 1 FROM tasks t WHERE t.id=s.task_id AND t.state IN ('pending','running','failed'))))`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return errors.New("副仓仍有文件或未结束任务，请先清理文件并取消任务，或取消勾选以暂停同步")
	}
	return nil
}

// A changed source coalesces into its existing pending task. A user's cancelled
// task stays cancelled until the source changes or they explicitly retry it.
func (db *DB) ReconcileColdSync() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO cold_sync_files(repository_id,file_id,wanted_revision)
 SELECT r.id,f.id,v.revision FROM cold_repositories r CROSS JOIN files f JOIN cold_sync_revisions v ON v.file_id=f.id
 WHERE r.enabled=1 AND r.deleted_at IS NULL AND f.hidden=0 AND f.purge_state IS NULL AND f.archive_state='done'
 AND NOT EXISTS(SELECT 1 FROM file_conversions c WHERE c.phase!='done' AND (c.source_id=f.id OR c.target_id=f.id))
 AND NOT EXISTS(SELECT 1 FROM cold_sync_files s WHERE s.repository_id=r.id AND s.file_id=f.id AND s.wanted_revision=v.revision)
 ON CONFLICT(repository_id,file_id) DO UPDATE SET wanted_revision=excluded.wanted_revision,
 task_id=CASE WHEN EXISTS(SELECT 1 FROM tasks t WHERE t.id=cold_sync_files.task_id AND t.state IN ('pending','running')) THEN cold_sync_files.task_id ELSE 0 END`)
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT s.repository_id,s.file_id FROM cold_sync_files s JOIN cold_repositories r ON r.id=s.repository_id JOIN files f ON f.id=s.file_id
 WHERE r.enabled=1 AND r.deleted_at IS NULL AND s.task_id=0 AND f.hidden=0 AND f.purge_state IS NULL AND f.archive_state='done' LIMIT 100`)
	if err != nil {
		return err
	}
	var jobs []ColdSyncJob
	for rows.Next() {
		var j ColdSyncJob
		if err = rows.Scan(&j.RepositoryID, &j.FileID); err != nil {
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
		r, e := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskColdSync, j.FileID, TaskPayloadJSON(map[string]any{"repository_id": j.RepositoryID}), now(), now(), now())
		if e != nil {
			return e
		}
		id, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if e = attachTaskOperation(tx, id, TaskColdSync, j.FileID, TaskPayloadJSON(map[string]any{"repository_id": j.RepositoryID})); e != nil {
			return e
		}
		if _, err = tx.Exec(`UPDATE cold_sync_files SET task_id=? WHERE repository_id=? AND file_id=?`, id, j.RepositoryID, j.FileID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Completed conversions retire the old file identity. Secondary-copy
	// cleanup keeps its own resumable row inside the conversion operation.
	rows, err = db.Query(`SELECT s.repository_id,s.file_id FROM cold_sync_files s JOIN files f ON f.id=s.file_id WHERE f.hidden=1 AND s.objects!='[]' AND EXISTS(SELECT 1 FROM file_conversions c WHERE c.source_id=f.id AND c.phase='done')`)
	if err != nil {
		return err
	}
	jobs = nil
	for rows.Next() {
		var j ColdSyncJob
		if err = rows.Scan(&j.RepositoryID, &j.FileID); err != nil {
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
		if _, err = db.EnqueueColdDelete(j.RepositoryID, j.FileID, nil); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) EnqueueColdDelete(repoID int64, fileID string, refs []string) (int64, error) {
	sort.Strings(refs)
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRow(`SELECT wanted_revision FROM cold_sync_files WHERE repository_id=? AND file_id=?`, repoID, fileID).Scan(&revision); err != nil {
		return 0, err
	}
	// The same folder can become obsolete again after moving A -> B -> A -> B.
	// Deduplicate retries within one source revision, never across generations.
	payload := TaskPayloadJSON(map[string]any{"repository_id": repoID, "references": refs, "revision": revision})
	var id int64
	err = tx.QueryRow(`SELECT id FROM tasks WHERE type=? AND file_id=? AND payload=? ORDER BY id DESC LIMIT 1`, TaskColdDelete, fileID, payload).Scan(&id)
	if err == nil {
		if err = attachColdDeleteFollowup(tx, id, repoID, fileID); err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?)`, TaskColdDelete, fileID, payload, now(), now(), now())
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = attachColdDeleteFollowup(tx, id, repoID, fileID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Parse only stored driver configurations in callers; credentials added by a
// future driver must never be returned through the general settings endpoint.
func ColdRepositoryConfig(raw string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
func (db *DB) GetColdSyncJob(repoID int64, fileID string) (*ColdSyncJob, error) {
	j := &ColdSyncJob{RepositoryID: repoID, FileID: fileID}
	err := db.QueryRow(`SELECT wanted_revision,objects FROM cold_sync_files WHERE repository_id=? AND file_id=?`, repoID, fileID).Scan(&j.Revision, &j.Objects)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}
func (db *DB) SaveColdSyncObjects(j *ColdSyncJob, objects string) error {
	_, err := db.Exec(`UPDATE cold_sync_files SET objects=? WHERE repository_id=? AND file_id=?`, objects, j.RepositoryID, j.FileID)
	return err
}
func (db *DB) FinishColdSync(j *ColdSyncJob) error {
	_, err := db.Exec(`UPDATE cold_sync_files SET done_revision=?,synced_at=?,task_id=CASE WHEN wanted_revision=? THEN task_id ELSE 0 END WHERE repository_id=? AND file_id=?`, j.Revision, now(), j.Revision, j.RepositoryID, j.FileID)
	return err
}
func (db *DB) ColdSyncAlbum(repoID, albumID int64) (name, reference string, err error) {
	err = db.QueryRow(`SELECT name,reference FROM cold_sync_albums WHERE repository_id=? AND album_id=?`, repoID, albumID).Scan(&name, &reference)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
func (db *DB) SaveColdSyncAlbum(repoID, albumID int64, name, reference string) error {
	_, err := db.Exec(`INSERT INTO cold_sync_albums(repository_id,album_id,name,reference) VALUES(?,?,?,?) ON CONFLICT(repository_id,album_id) DO UPDATE SET name=excluded.name,reference=excluded.reference`, repoID, albumID, name, reference)
	return err
}
func (db *DB) FileColdSyncJobs(fileID string) ([]ColdSyncJob, error) {
	rows, err := db.Query(`SELECT repository_id,wanted_revision,objects FROM cold_sync_files WHERE file_id=?`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColdSyncJob
	for rows.Next() {
		j := ColdSyncJob{FileID: fileID}
		if err = rows.Scan(&j.RepositoryID, &j.Revision, &j.Objects); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (db *DB) ColdSyncForegroundBusy(at int64) (bool, error) {
	var busy bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks a WHERE a.type NOT IN (`+idleTaskTypesSQL+`) AND NOT (`+passiveTaskSQL("a")+`) AND (a.state='running' OR (a.state='pending' AND a.run_after<=?)))`, at).Scan(&busy)
	return busy, err
}

// ClaimTask cannot claim work for a file already owned by a running idle task.
// Detect that wait before claiming, otherwise the foreground hook is not reached.
func (db *DB) IdleBlocksForeground(at int64) (bool, error) {
	var blocked bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks p JOIN tasks a ON a.file_id=p.file_id
 WHERE p.state='pending' AND p.cancel_requested=0 AND p.run_after<=? AND p.file_id IS NOT NULL AND p.file_id!=''
 AND p.type NOT IN (`+idleTaskTypesSQL+`) AND NOT (`+passiveTaskSQL("p")+`)
 AND a.state='running' AND a.type IN (`+idleTaskTypesSQL+`))`, at).Scan(&blocked)
	return blocked, err
}

// Idle work has one dedicated worker and never takes a normal worker slot.
func (db *DB) ClaimIdleTask(at, leaseSec int64) (*Task, error) {
	return scanTaskRow(db.QueryRow(`UPDATE tasks SET state='running',lease_until=?,attempts=attempts+1,updated_at=? WHERE id=(SELECT c.id FROM tasks c
 WHERE c.type IN (`+idleTaskTypesSQL+`) AND c.state='pending' AND c.cancel_requested=0 AND c.run_after<=?
 AND NOT EXISTS(SELECT 1 FROM tasks a WHERE a.state='running' AND NOT (`+passiveTaskSQL("a")+`))
 AND NOT EXISTS(SELECT 1 FROM tasks a WHERE a.type NOT IN (`+idleTaskTypesSQL+`) AND NOT (`+passiveTaskSQL("a")+`) AND a.state='pending' AND a.run_after<=?)
 AND (c.type!='cold_sync' OR EXISTS(SELECT 1 FROM cold_repositories r WHERE r.id=json_extract(c.payload,'$.repository_id') AND r.enabled=1 AND r.deleted_at IS NULL))
 ORDER BY CASE WHEN c.type='purge_file' THEN 0 WHEN c.type='source_cleanup' THEN 1 WHEN c.type='cold_delete' THEN 2 WHEN c.type='archive_sync' THEN 4 ELSE 3 END,
 CASE WHEN c.type='archive_sync' THEN c.created_at ELSE c.run_after END,c.id LIMIT 1)
 RETURNING id,type,file_id,payload,state,attempts,max_attempts,last_error,result,run_after,lease_until,created_at,updated_at,done_at`, at+leaseSec, at, at, at))
}

func (db *DB) FileTaskReferences(fileID string, exceptTask int64) ([]int64, error) {
	rows, err := db.Query(`SELECT id FROM tasks WHERE file_id=? AND id!=? AND state IN ('pending','running','failed') ORDER BY id`, fileID, exceptTask)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (db *DB) RequestTaskCancel(id int64) error {
	var reserved bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=? AND type=?)`, id, TaskSystemSchedule).Scan(&reserved); err != nil {
		return err
	}
	if reserved {
		return ErrSystemScheduleManaged
	}
	r, err := db.Exec(`UPDATE tasks SET cancel_requested=1,state=CASE WHEN state='running' THEN 'running' ELSE 'canceled' END,updated_at=? WHERE id=? AND state IN ('pending','running','failed')`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return errors.New("任务已结束或不存在")
	}
	return nil
}
func (db *DB) TaskCancelRequested(id int64) (bool, error) {
	var v bool
	err := db.QueryRow(`SELECT cancel_requested FROM tasks WHERE id=?`, id).Scan(&v)
	return v, err
}
func (db *DB) FinishTaskCanceled(id int64) error {
	_, err := db.Exec(`UPDATE tasks SET state='canceled',lease_until=NULL,updated_at=?,done_at=? WHERE id=?`, now(), now(), id)
	return err
}
func (db *DB) PostponeIdleTask(id int64) error {
	_, err := db.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'pending' END,attempts=MAX(attempts-1,0),run_after=?,lease_until=NULL,updated_at=? WHERE id=?`, now()+2, now(), id)
	return err
}
