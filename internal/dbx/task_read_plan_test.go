package dbx

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func readPlanFixture(t *testing.T, n int) *DB {
	t.Helper()
	d := activityDB(t)
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, query := range []string{
		`WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<200)
 INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,created_at,updated_at)
 SELECT 'rp-file-'||x,'image-'||x,'image-'||x||'.png','png','image/png',1,'hash-'||x,'image',X'01',X'02','done',100,100 FROM n`,
		`WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100)
 INSERT INTO task_operations(id,kind,scope,created_at,owner_id,request_key,input_state,updated_at,anchor_task_id,expected_files)
 SELECT x,'upload','test',100,'admin:1','rp-operation-'||x,CASE WHEN x=100 THEN 'open' ELSE 'sealed' END,100,x,2 FROM n`,
		`WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<` + fmtReadCount(n) + `)
 INSERT INTO tasks(id,type,file_id,payload,state,run_after,created_at,updated_at)
 SELECT x,CASE WHEN x%3=0 THEN 'archive_sync' ELSE 'direct_archive' END,'rp-file-'||((x-1)%200+1),json_object('repo',(x-1)%3),
 CASE WHEN x%97=0 THEN 'running' WHEN x>` + fmtReadCount(n*3/4) + ` THEN 'pending' WHEN x%11=0 THEN 'failed' WHEN x%13=0 THEN 'canceled' ELSE 'done' END,9999999999,100+x,100+x FROM n`,
		`INSERT INTO task_operation_jobs SELECT id,(id-1)%100+1,file_id FROM tasks WHERE id%5!=0`,
		`INSERT INTO task_operation_items(operation_id,item_key,file_id,name,state,error,created_at,updated_at) SELECT (rowid-1)%100+1,id,id,orig_name,'queued','',100,100 FROM files`,
		`INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at) SELECT 'rp-source-'||rowid,'accepted','',id,'','',json_object('source','pimg','operation_id',(rowid-1)%100+1,'name',orig_name),100 FROM files`,
		`INSERT INTO local_import_batches(id,root,paths,album_id,encrypted,recursive,schedule,state,created_at) VALUES(1,'synthetic','[]',0,1,0,'now','ready',100)`,
		`INSERT INTO tasks(type,file_id,payload,state,run_after,created_at,updated_at) VALUES('local_import_scan','', '{"batch_id":1}', 'done',100,100,100),('system_schedule','','{"name":"probe"}','done',100,100,100)`,
		`UPDATE tasks SET result='{"unavailable":2}' WHERE type='system_schedule'`,
	} {
		if _, err = tx.Exec(query); err != nil {
			t.Fatal(query, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

func fmtReadCount(n int) string { data, _ := json.Marshal(n); return string(data) }

func TestTaskReadPlanMatchesReleasedSemantics(t *testing.T) {
	d := readPlanFixture(t, 600)
	for _, q := range []LogQuery{{}, {State: "active", Sort: "active"}, {State: "terminal"}, {View: "current"}, {State: "failed"}, {State: "done"}, {State: "issues"}, {Search: "image-1"}, {Search: "rp-file-55"}, {FileID: "rp-file-1"}, {GroupID: 2}, {Page: 2, Limit: 7}, {Type: "archive_sync"}, {State: "active", Search: "rp-file-1"}} {
		q.Grouped = true
		q.Limit = 7
		got, total, err := d.QueryActivityTasks(q)
		want, wtotal, werr := d.legacyActivityTasks(q)
		if err != nil || werr != nil || total != wtotal || !reflect.DeepEqual(got, want) {
			t.Fatalf("task plan mismatch %+v counts %d/%d err %v/%v", q, total, wtotal, err, werr)
		}
		logs, count, err := d.QueryUnifiedLogs(q)
		old, oldcount, werr := d.legacyUnifiedLogs(q)
		if err != nil || werr != nil || count != oldcount || !reflect.DeepEqual(logs, old) {
			a, _ := json.Marshal(logs)
			b, _ := json.Marshal(old)
			t.Fatalf("log plan mismatch %+v counts %d/%d err %v/%v\n%s\n%s", q, count, oldcount, err, werr, a, b)
		}
	}
}

func TestTaskReadPlanPerformance(t *testing.T) {
	out := os.Getenv("PIMG_READ_PLAN_PERF_OUTPUT")
	if out == "" {
		t.Skip("opt-in isolated SQL performance comparison")
	}
	d := readPlanFixture(t, 12000)
	report := map[string]any{"isolated": true, "tasks": 12002, "same_database": true}
	for _, kind := range []string{"tasks", "logs"} {
		times := map[string][]float64{"released": {}, "optimized": {}}
		for i := 0; i < 3; i++ {
			for _, plan := range []string{"released", "optimized"} {
				start := time.Now()
				var err error
				q := LogQuery{Grouped: true, Page: 1, Limit: 50}
				if kind == "tasks" {
					q.State = "active"
					q.Sort = "active"
					if plan == "released" {
						_, _, err = d.legacyActivityTasks(q)
					} else {
						_, _, err = d.QueryActivityTasks(q)
					}
				} else {
					if plan == "released" {
						_, _, err = d.legacyUnifiedLogs(q)
					} else {
						_, _, err = d.QueryUnifiedLogs(q)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				times[plan] = append(times[plan], float64(time.Since(start).Microseconds())/1000)
			}
		}
		report[kind] = times
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(out, data, 0600); err != nil {
		t.Fatal(err)
	}
}
