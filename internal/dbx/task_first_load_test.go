package dbx

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Opt-in: all records are synthetic; include wide historical results and one
// small active operation so first reads exercise backlog costs, not HTTP caches.
func TestTaskFirstLoadPerformance(t *testing.T) {
	output := os.Getenv("PIMG_FIRST_LOAD_PERF_OUTPUT")
	if output == "" {
		t.Skip("set PIMG_FIRST_LOAD_PERF_OUTPUT for isolated uncached-query measurements")
	}
	d := readPlanFixture(t, 12000)
	for _, query := range []string{
		`UPDATE task_operations SET input_state='sealed'`,
		`UPDATE tasks SET state='done',result=json_object('summary',printf('%02000d',id))`,
		`UPDATE tasks SET state='pending' WHERE id=12000`,
	} {
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	report := map[string]any{"tasks": 12002, "synthetic": true}
	for _, kind := range []string{"summary", "tasks", "logs"} {
		times := []float64{}
		for i := 0; i < 3; i++ {
			start := time.Now()
			var err error
			switch kind {
			case "summary":
				_, err = d.ActivityTaskCounts("")
			case "tasks":
				_, _, err = d.QueryActivityTasks(LogQuery{Grouped: true, State: "active", Sort: "active", Limit: 50})
			case "logs":
				_, _, err = d.QueryUnifiedLogs(LogQuery{Limit: 50})
			}
			if err != nil {
				t.Fatal(err)
			}
			times = append(times, float64(time.Since(start).Microseconds())/1000)
		}
		report[kind+"_ms"] = times
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
}
