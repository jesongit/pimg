package server

// Opt-in HTTP acceptance for large task histories. All files, credentials and
// records are synthetic and confined to testing.T.TempDir; workers never run.
import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAdminPagePerformance(t *testing.T) {
	output := os.Getenv("PIMG_PAGE_PERF_OUTPUT")
	if output == "" {
		t.Skip("set PIMG_PAGE_PERF_OUTPUT for synthetic page-load acceptance")
	}
	s := interactionServer(t)
	if err := s.DB.EnsureAdmin("page-performance", adminPasswordHash(t, "isolated-page-performance")); err != nil {
		t.Fatal(err)
	}
	cookie := adminCookie(t, s, "isolated-page-performance")
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000)
 INSERT INTO files(id,name,orig_name,ext,mime,size,sha256,kind,key_wrapped,key_nonce,archive_state,storage,cold_backend,created_at,updated_at)
 SELECT 'page-file-'||x,'image-'||x,'image-'||x||'.png','png','image/png',1024,'synthetic-'||x,'image',X'01',X'02','done','direct','local',unixepoch(),unixepoch() FROM n`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000)
 INSERT INTO task_operations(id,kind,scope,created_at,owner_id,request_key,input_state,updated_at,anchor_task_id)
 SELECT x,'upload','synthetic',unixepoch(),'admin:1',CASE WHEN x%2=0 THEN 'page-operation-'||x ELSE '' END,'sealed',unixepoch(),x FROM n`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<12000)
 INSERT INTO tasks(id,type,file_id,payload,state,run_after,created_at,updated_at)
 SELECT x,'direct_archive','page-file-'||((x-1)%2000+1),'{}',CASE WHEN x=12000 THEN 'running' WHEN x>11155 THEN 'pending' ELSE 'done' END,unixepoch()+86400,unixepoch(),unixepoch() FROM n`,
		`INSERT INTO task_operation_jobs(task_id,operation_id,item_key) SELECT id,(id-1)%1000+1,file_id FROM tasks`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000)
 INSERT INTO task_operation_items(operation_id,item_key,file_id,name,state,error,created_at,updated_at)
 SELECT (x-1)%1000+1,'page-file-'||x,'page-file-'||x,'image-'||x||'.png','queued','',unixepoch(),unixepoch() FROM n`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000)
 INSERT INTO ingest_events(source_key,event_type,reason,file_id,tool_file_id,upload_id,body,created_at)
 SELECT 'page-source-'||x,'accepted','','page-file-'||x,'','',json_object('source','pimg','operation_id',(x-1)%1000+1,'name','image-'||x||'.png'),unixepoch() FROM n`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PIMG_PAGE_PERF_MEDIA") == "1" {
		for _, statement := range []string{
			`UPDATE files SET media_version=2`,
			`INSERT INTO media_replicas(file_id,instance_id,policy,state) SELECT id,'local','{"encrypted":false,"split":false,"part_mb":256}','ready' FROM files`,
			`INSERT INTO media_parts(file_id,instance_id,idx,metadata) SELECT id,'local',0,json_object('index',0,'count',1,'ext','png','mime','image/png','size',size,'sha256',sha256,'reference',json_object('key','synthetic/'||id)||'','verified',json('true'),'original',json('true')) FROM files`,
		} {
			if _, err = s.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = s.DB.MediaReplicas("page-file-1"); err != nil {
			t.Fatal("invalid modern media fixture", err)
		}
	}
	// Table contents and schema must remain intact after all page requests.
	snapshot := func() map[string][32]byte {
		result := map[string][32]byte{}
		for _, table := range []string{"files", "tasks", "task_operations", "task_operation_jobs", "task_operation_items", "ingest_events", "media_replicas", "media_parts", "sqlite_schema"} {
			rows, e := s.DB.Query("SELECT * FROM " + table + " ORDER BY rowid")
			if e != nil {
				t.Fatal(e)
			}
			columns, e := rows.Columns()
			if e != nil {
				rows.Close()
				t.Fatal(e)
			}
			hash := sha256.New()
			for rows.Next() {
				values, dest := make([]any, len(columns)), make([]any, len(columns))
				for i := range values {
					dest[i] = &values[i]
				}
				if e = rows.Scan(dest...); e != nil {
					rows.Close()
					t.Fatal(e)
				}
				if e = json.NewEncoder(hash).Encode(values); e != nil {
					rows.Close()
					t.Fatal(e)
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				t.Fatal(e)
			}
			result[table] = [32]byte(hash.Sum(nil))
		}
		return result
	}
	before := snapshot()
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	type timing struct {
		Path string    `json:"path"`
		MS   []float64 `json:"milliseconds"`
	}
	paths := []string{
		"/api/admin/files?per_page=60", "/api/admin/files/timeline", "/api/admin/albums",
		"/api/admin/tasks/summary", "/api/admin/logs?record=tasks&grouped=1&state=active&sort=active&page=1",
		"/api/admin/logs?record=all&page=1", "/api/admin/storage", "/api/admin/accounts", "/api/admin/settings",
		"/api/admin/tokens", "/api/admin/schedules", "/api/admin/status", "/api/auth/2fa",
	}
	request := func(path string) (float64, error) {
		req, _ := http.NewRequest("GET", h.URL+path, nil)
		req.AddCookie(cookie)
		start := time.Now()
		response, e := h.Client().Do(req)
		if e != nil {
			return 0, e
		}
		defer response.Body.Close()
		body, e := io.ReadAll(response.Body)
		if e != nil || response.StatusCode != 200 {
			return 0, fmt.Errorf("%s: HTTP %d: %s: %v", path, response.StatusCode, body, e)
		}
		var data map[string]any
		if e = json.Unmarshal(body, &data); e != nil {
			return 0, e
		}
		if path == paths[0] && data["total"] != float64(2000) {
			return 0, fmt.Errorf("file list lost fixture records: %s", body)
		}
		if path == paths[0] && os.Getenv("PIMG_PAGE_PERF_MEDIA") == "1" {
			for _, item := range data["items"].([]any) {
				file := item.(map[string]any)
				parts, ok := file["media_parts"].([]any)
				if !ok || len(parts) != 1 {
					return 0, fmt.Errorf("file list lost media metadata: %v", file["id"])
				}
			}
		}
		if path == paths[3] && (data["running"] != float64(1) || data["pending"] != float64(844)) {
			return 0, fmt.Errorf("grouped navigation counts changed: %s", body)
		}
		return float64(time.Since(start).Microseconds()) / 1000, nil
	}
	results := []timing{}
	for _, path := range paths {
		item := timing{Path: path}
		for range 3 {
			elapsed, e := request(path)
			if e != nil {
				t.Fatal(e)
			}
			item.MS = append(item.MS, elapsed)
		}
		results = append(results, item)
		sorted := append([]float64{}, item.MS...)
		sort.Float64s(sorted)
		t.Logf("%s: median=%.1f ms, max=%.1f ms", path, sorted[1], sorted[2])
	}
	conditional := []timing{}
	for _, path := range []string{paths[0], paths[1], paths[3], paths[4], paths[5]} {
		r, _ := http.NewRequest("GET", h.URL+path, nil)
		r.AddCookie(cookie)
		response, e := h.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		tag := response.Header.Get("ETag")
		if response.StatusCode != 200 || tag == "" {
			t.Fatal(path, response.StatusCode)
		}
		item := timing{Path: path}
		for range 3 {
			r, _ := http.NewRequest("GET", h.URL+path, nil)
			r.AddCookie(cookie)
			r.Header.Set("If-None-Match", tag)
			start := time.Now()
			response, e := h.Client().Do(r)
			if e != nil {
				t.Fatal(e)
			}
			body, e := io.ReadAll(response.Body)
			response.Body.Close()
			if e != nil || response.StatusCode != 304 || len(body) != 0 {
				t.Fatal(path, response.StatusCode, e)
			}
			item.MS = append(item.MS, float64(time.Since(start).Microseconds())/1000)
		}
		conditional = append(conditional, item)
		sorted := append([]float64{}, item.MS...)
		sort.Float64s(sorted)
		t.Logf("unchanged %s: median=%.1f ms, max=%.1f ms", path, sorted[1], sorted[2])
	}
	// Exercise competition for the existing two-reader pool without enlarging it.
	var wg sync.WaitGroup
	var mu sync.Mutex
	concurrent := []timing{}
	for _, path := range []string{paths[0], paths[1], paths[3], paths[4], paths[5]} {
		wg.Go(func() {
			elapsed, e := request(path)
			if e != nil {
				t.Error(e)
			}
			mu.Lock()
			concurrent = append(concurrent, timing{Path: path, MS: []float64{elapsed}})
			mu.Unlock()
		})
	}
	wg.Wait()
	preserved := true
	for table, hash := range snapshot() {
		if hash != before[table] {
			preserved = false
			t.Error("page reads changed stored data or schema", table)
		}
	}
	var check string
	if err = s.DB.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatal(check, err)
	}
	report := map[string]any{"synthetic": true, "media_v2_fixture": os.Getenv("PIMG_PAGE_PERF_MEDIA") == "1", "files": 2000, "tasks": 12000, "operations": 1000, "receipts": 2000, "sequential": results, "unchanged": conditional, "concurrent": concurrent, "data_and_schema_unchanged": preserved, "integrity_check": check, "url": h.URL}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal("cannot write page performance report", err)
	}
	if err = os.WriteFile(output, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Optional browser inspection against this same isolated live HTTP server.
	if hold, _ := strconv.Atoi(os.Getenv("PIMG_PAGE_PERF_HOLD_SECONDS")); hold > 0 && hold <= 600 {
		t.Logf("Synthetic browser server %s, login page-performance / isolated-page-performance", h.URL)
		deadline := time.NewTimer(time.Duration(hold) * time.Second)
		defer deadline.Stop()
		<-deadline.C
	}
}
