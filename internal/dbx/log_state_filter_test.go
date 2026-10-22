package dbx

import "testing"

func TestUnifiedLogStateFiltersCountBeforePaginationAndSearch(t *testing.T) {
	d := telegramTestDB(t)
	for _, state := range []string{"done", "failed", "canceled"} {
		id, err := d.EnqueueTask(TaskCleanupTemp, "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET state=?,done_at=unixepoch(),result='{}' WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	warning, err := d.EnqueueTask("system_schedule", "system-schedule:storage", `{"name":"storage warning"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishTask(warning, `{"unavailable":1}`); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		state string
		want  int
	}{{"", 4}, {"done", 1}, {"failed", 1}, {"issues", 2}, {"canceled", 1}} {
		rows, n, err := d.QueryUnifiedLogs(LogQuery{State: item.state, Page: 1, Limit: 1})
		if err != nil || n != item.want || len(rows) != 1 {
			t.Fatal(item.state, n, len(rows), err)
		}
		rows, n, err = d.QueryUnifiedLogs(LogQuery{State: item.state, Page: 99, Limit: 1})
		if err != nil || n != item.want || len(rows) != 0 {
			t.Fatal("empty page changed total", item.state, n, err)
		}
	}
	rows, n, err := d.QueryUnifiedLogs(LogQuery{State: "issues", Search: "storage warning", Page: 1, Limit: 50})
	if err != nil || n != 1 || len(rows) != 1 {
		t.Fatal("search combined incorrectly", n, err)
	}
}
