package tasks

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/store"
	"testing"
)

func TestStorageScheduleResultRecordsEveryCheckedStorage(t *testing.T) {
	dir := t.TempDir()
	d, e := dbx.Open(filepath.Join(dir, "result.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}}
	svc, e := store.New(cfg, d, master, nil, nil, logger)
	if e != nil {
		t.Fatal(e)
	}
	engine := New(d, svc, 1, logger)
	defer engine.cancel()
	result, e := engine.executeSystemSchedule(t.Context(), "storage_probe")
	if e != nil {
		t.Fatal(e)
	}
	body, e := json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	var decoded struct {
		Checks []struct {
			Name      string  `json:"name"`
			Available *bool   `json:"available"`
			Latency   float64 `json:"latency_ms"`
			Error     string  `json:"error"`
		}
	}
	if e = json.Unmarshal(body, &decoded); e != nil {
		t.Fatal(e)
	}
	count := 0
	localReady := false
	for _, i := range svc.StorageInstances() {
		if i.Enabled {
			count++
		}
	}
	for _, i := range decoded.Checks {
		if i.Name == "" || i.Available == nil {
			t.Fatalf("incomplete result: %+v", i)
		}
		if *i.Available {
			localReady = true
		} else if i.Error == "" {
			t.Fatalf("unavailable storage omitted cause: %+v", i)
		}
	}
	if len(decoded.Checks) != count || !localReady {
		t.Fatal("missing checked storage results", count, len(decoded.Checks))
	}
}
