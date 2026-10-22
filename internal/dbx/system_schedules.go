package dbx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

const TaskSystemSchedule = "system_schedule"

var ErrScheduleLeaseLost = errors.New("系统计划执行租约已失效")
var ErrInvalidSystemSchedule = errors.New("系统计划规则无效")
var ErrSystemScheduleManaged = errors.New("请在系统定时任务中修改规则；当前执行完成后应用新计划")

const systemScheduleSchema = `
CREATE TABLE system_schedules(
 id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1,cron TEXT NOT NULL,timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai',
 next_run INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'scheduled',
 last_run INTEGER NOT NULL DEFAULT 0,last_status TEXT NOT NULL DEFAULT '',last_error TEXT NOT NULL DEFAULT '',last_result TEXT NOT NULL DEFAULT '{}',
 run_task_id INTEGER NOT NULL DEFAULT 0,lease_token TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO system_schedules(id,name,description,cron) VALUES
 ('log_cleanup','日志过期清理','按保留天数清理过期日志，保留运行和恢复所需数据','0 6 * * *'),
 ('storage_probe','存储健康检查','检测可用状态与延迟，不传输测速文件','*/5 * * * *'),
 ('temp_cleanup','临时副本清理','清理到期临时发布副本和过期登录记录','* * * * *'),
 ('media_cleanup','媒体缓存清理','清理到期的本地原件与媒体预览缓存','* * * * *');
`

type SystemSchedule struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Cron        string          `json:"cron"`
	Timezone    string          `json:"timezone"`
	NextRun     int64           `json:"next_run"`
	State       string          `json:"state"`
	LastRun     int64           `json:"last_run"`
	LastStatus  string          `json:"last_status"`
	LastError   string          `json:"last_error"`
	LastResult  json.RawMessage `json:"last_result"`
	TaskID      int64           `json:"-"`
	LeaseToken  string          `json:"-"`
	LeaseUntil  int64           `json:"-"`
}

const scheduleColumns = `id,name,description,enabled,cron,timezone,next_run,state,last_run,last_status,last_error,last_result,run_task_id,lease_token,lease_until`

func scanSystemSchedule(row interface{ Scan(...any) error }) (*SystemSchedule, error) {
	s := &SystemSchedule{}
	var result string
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Enabled, &s.Cron, &s.Timezone, &s.NextRun, &s.State, &s.LastRun, &s.LastStatus, &s.LastError, &result, &s.TaskID, &s.LeaseToken, &s.LeaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	s.LastResult = json.RawMessage(result)
	return s, err
}

func NextSystemSchedule(expression, zone string, after int64) (int64, error) {
	if len(expression) > 256 || len(strings.Fields(expression)) != 5 || len(zone) > 128 || zone == "Local" || strings.TrimSpace(zone) != zone || zone == "" {
		return 0, fmt.Errorf("%w: 请填写标准五字段 Cron 和 IANA 时区", ErrInvalidSystemSchedule)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return 0, fmt.Errorf("%w: 时区无效，请使用 Asia/Shanghai 等 IANA 时区", ErrInvalidSystemSchedule)
	}
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := p.Parse("CRON_TZ=" + zone + " " + expression)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidSystemSchedule, err)
	}
	next := schedule.Next(time.Unix(after, 0))
	if next.IsZero() || next.Unix() <= after {
		return 0, fmt.Errorf("%w: Cron 规则没有有效的下一次执行时间", ErrInvalidSystemSchedule)
	}
	return next.Unix(), nil
}

func (db *DB) SystemSchedules() ([]SystemSchedule, error) {
	rows, err := db.Query(`SELECT ` + scheduleColumns + ` FROM system_schedules ORDER BY CASE id WHEN 'log_cleanup' THEN 0 WHEN 'storage_probe' THEN 1 WHEN 'temp_cleanup' THEN 2 ELSE 3 END`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SystemSchedule{}
	for rows.Next() {
		s, e := scanSystemSchedule(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (db *DB) SystemSchedule(id string) (*SystemSchedule, error) {
	return scanSystemSchedule(db.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE id=?`, id))
}

// Startup skips missed occurrences and schedules only a future run. Ordinary
// initialization does not move an already due row away from the scheduler.
func (db *DB) PrepareSystemSchedules(at int64, restart bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE system_schedules SET next_run=next_run WHERE 0`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT ` + scheduleColumns + ` FROM system_schedules`)
	if err != nil {
		return err
	}
	items := []*SystemSchedule{}
	for rows.Next() {
		s, e := scanSystemSchedule(rows)
		if e != nil {
			rows.Close()
			return e
		}
		items = append(items, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, s := range items {
		if !s.Enabled && s.State == "disabled" && s.NextRun == 0 {
			continue
		}
		if s.State == "running" && s.LeaseUntil > at {
			continue
		}
		interrupted := s.State == "running"
		if interrupted {
			if _, err = tx.Exec(`UPDATE tasks SET state='failed',last_error='系统维护被中断，将按下一次计划继续',done_at=?,updated_at=?,lease_until=NULL WHERE id=? AND type=? AND state='running'`, at, at, s.TaskID, TaskSystemSchedule); err != nil {
				return err
			}
			if _, err = tx.Exec(`UPDATE system_schedules SET last_status='interrupted',last_error='上次执行中断，已重新安排后续计划',run_task_id=0,lease_token='',lease_until=0 WHERE id=?`, s.ID); err != nil {
				return err
			}
		}
		if s.NextRun != 0 && !interrupted && (!restart || s.NextRun > at) && s.Enabled {
			continue
		}
		next := int64(0)
		state := "disabled"
		if s.Enabled {
			next, err = NextSystemSchedule(s.Cron, s.Timezone, at)
			if err != nil {
				return err
			}
			state = "scheduled"
		}
		if _, err = tx.Exec(`UPDATE system_schedules SET next_run=?,state=?,updated_at=? WHERE id=?`, next, state, at, s.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type SystemSchedulePatch struct {
	Enabled  *bool   `json:"enabled"`
	Cron     *string `json:"cron"`
	Timezone *string `json:"timezone"`
}

func (db *DB) PatchSystemSchedule(id string, p SystemSchedulePatch, at int64) (*SystemSchedule, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE system_schedules SET next_run=next_run WHERE 0`); err != nil {
		return nil, err
	}
	s, err := scanSystemSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if p.Enabled != nil {
		s.Enabled = *p.Enabled
	}
	if p.Cron != nil {
		s.Cron = strings.Join(strings.Fields(*p.Cron), " ")
	}
	if p.Timezone != nil {
		s.Timezone = *p.Timezone
	}
	next, err := NextSystemSchedule(s.Cron, s.Timezone, at)
	if err != nil {
		return nil, err
	}
	if !s.Enabled {
		next = 0
	}
	state := "scheduled"
	if !s.Enabled {
		state = "disabled"
	}
	if s.State == "running" {
		state = "running"
	}
	if _, err = tx.Exec(`UPDATE system_schedules SET enabled=?,cron=?,timezone=?,next_run=?,state=?,updated_at=? WHERE id=?`, s.Enabled, s.Cron, s.Timezone, next, state, at, id); err != nil {
		return nil, err
	}
	s, err = scanSystemSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

func (db *DB) ClaimSystemSchedule(at int64, token string, lease int64) (*SystemSchedule, error) {
	if token == "" || lease < 30 {
		return nil, errors.New("计划租约无效")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE system_schedules SET next_run=next_run WHERE 0`); err != nil {
		return nil, err
	}
	s, err := scanSystemSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE enabled=1 AND state!='running' AND next_run>0 AND next_run<=? ORDER BY next_run,id LIMIT 1`, at))
	if err != nil {
		return nil, err
	}
	next, err := NextSystemSchedule(s.Cron, s.Timezone, at)
	if err != nil {
		return nil, err
	}
	r, err := tx.Exec(`INSERT INTO tasks(type,payload,state,attempts,max_attempts,run_after,lease_until,created_at,updated_at) VALUES(?,?,'running',1,1,?,?,?,?)`, TaskSystemSchedule, TaskPayloadJSON(map[string]any{"schedule_id": s.ID, "name": s.Name}), at, at+lease, at, at)
	if err != nil {
		return nil, err
	}
	taskID, err := r.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE system_schedules SET state='running',last_run=?,last_status='running',last_error='',next_run=?,run_task_id=?,lease_token=?,lease_until=?,updated_at=? WHERE id=?`, at, next, taskID, token, at+lease, at, s.ID); err != nil {
		return nil, err
	}
	s, err = scanSystemSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE id=?`, s.ID))
	if err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

func (db *DB) HeartbeatSystemSchedule(id, token string, taskID, at, lease int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE system_schedules SET lease_until=? WHERE id=? AND state='running' AND lease_token=? AND run_task_id=? AND lease_until>?`, at+lease, id, token, taskID, at)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrScheduleLeaseLost
	}
	if _, err = tx.Exec(`UPDATE tasks SET lease_until=? WHERE id=? AND state='running' AND type=?`, at+lease, taskID, TaskSystemSchedule); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) FinishSystemSchedule(id, token string, taskID, at int64, result map[string]any, runErr error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE system_schedules SET next_run=next_run WHERE 0`); err != nil {
		return err
	}
	s, err := scanSystemSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM system_schedules WHERE id=? AND state='running' AND lease_token=? AND run_task_id=? AND lease_until>?`, id, token, taskID, at))
	if errors.Is(err, ErrNotFound) {
		return ErrScheduleLeaseLost
	}
	if err != nil {
		return err
	}
	next := int64(0)
	state := "disabled"
	if s.Enabled {
		next, err = NextSystemSchedule(s.Cron, s.Timezone, at)
		if err != nil {
			return err
		}
		state = "scheduled"
	}
	status, message := "done", ""
	if runErr != nil {
		status = "failed"
		message = runErr.Error()
	}
	if result == nil {
		result = map[string]any{}
	}
	if runErr == nil {
		if warning, ok := result["warning"].(string); ok {
			message = warning
		}
	}
	result["schedule_name"] = s.Name
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET state=?,result=?,last_error=?,done_at=?,updated_at=?,lease_until=NULL WHERE id=? AND type=?`, status, string(raw), message, at, at, taskID, TaskSystemSchedule); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE system_schedules SET state=?,next_run=?,last_status=?,last_error=?,last_result=?,run_task_id=0,lease_token='',lease_until=0,updated_at=? WHERE id=? AND lease_token=?`, state, next, status, message, string(raw), at, id, token); err != nil {
		return err
	}
	if runErr == nil && scheduleResultNoChange(result) {
		if _, err = tx.Exec(`DELETE FROM tasks WHERE id=? AND type=? AND state='done'
 AND NOT EXISTS(SELECT 1 FROM task_operation_jobs WHERE task_id=tasks.id)
 AND NOT EXISTS(SELECT 1 FROM file_conversions WHERE task_id=tasks.id)
 AND NOT EXISTS(SELECT 1 FROM cold_sync_files WHERE task_id=tasks.id)
 AND NOT EXISTS(SELECT 1 FROM album_topics WHERE scan_task_id=tasks.id)`, taskID, TaskSystemSchedule); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scheduleResultNoChange(result map[string]any) bool {
	switch n := result["changed"].(type) {
	case int:
		return n == 0
	case int64:
		return n == 0
	case float64:
		return n == 0
	}
	return false
}
