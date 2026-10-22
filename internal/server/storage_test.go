package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/r2x"
	"pimg/internal/store"
	"pimg/internal/tgx"
)

func TestStorageSettingsPersistenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Storage: config.Storage{Cold: "local", Hot: "local"}, Paths: config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")}, R2: config.R2{RetentionHours: 24, PresignMinutes: 30}, Upload: config.Upload{MaxSizeMB: 512, AllowedExts: []string{"png"}}}
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	svc, err := store.New(cfg, db, master, tgx.New("", "", ""), r2x.New("", "", "", "", "", "", "", ""), log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, db, svc, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	patch := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handlePatchSettings(w, httptest.NewRequest("PATCH", "/api/admin/settings", bytes.NewBufferString(body)))
		if w.Code != want {
			t.Fatalf("%s: %d want %d: %s", body, w.Code, want, w.Body.String())
		}
	}
	patch(`{"cold_storage":"telegram","site_name":"must not save"}`, 400)
	patch(`{"hot_storage":"r2"}`, 400)
	patch(`{"cold_storage":"bogus"}`, 400)
	patch(`{"cold_storage":null}`, 400)
	patch(`{"hot_storage":false}`, 400)
	patch(`{"theme":5}`, 400)
	patch(`{"retention_hours":1.5}`, 400)
	if name, _ := db.GetSetting("site.name"); name != "" {
		t.Fatal("rejected patch wrote unrelated changes")
	}
	cfg.Telegram = config.Telegram{BotToken: "test", ChannelID: "channel", ChunkMB: 16}
	cfg.R2 = config.R2{Endpoint: "http://unused", AccessKeyID: "key", SecretAccessKey: "secret", TempBucket: "temp", PermBucket: "perm", PublicBaseURL: "http://unused/perm", RetentionHours: 24, PresignMinutes: 30}
	if _, err := svc.SaveStorageInstance("r2", "R2 永久存储", "r2", store.InstanceConfig{Endpoint: cfg.R2.Endpoint, AccessKey: cfg.R2.AccessKeyID, SecretKey: cfg.R2.SecretAccessKey, Bucket: cfg.R2.PermBucket, PublicBase: cfg.R2.PublicBaseURL}, true); err != nil {
		t.Fatal(err)
	}
	patch(`{"cold_storage":"telegram","hot_storage":"r2"}`, 200)
	// New service and server must restore selections and policy from SQLite.
	patch(`{"retention_hours":12,"upload_max_mb":16}`, 200)
	restarted, err := store.New(cfg, db, master, svc.TG, svc.R2, log)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(cfg, db, restarted, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	cold, hot := restarted.StorageSelection()
	if cold != "telegram" || hot != "r2" || restarted.RetentionHours() != 12 || restarted.MaxSizeMB() != 16 {
		t.Fatal("saved storage/policy not restored")
	}
	w := httptest.NewRecorder()
	s2.handleGetSettings(w, httptest.NewRequest("GET", "/api/admin/settings", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cold_storage"] != "telegram" || body["retention_hours"] != float64(12) {
		t.Fatal("settings response differs from saved state")
	}
	patch(`{"cold_storage":"local","hot_storage":"local","retention_hours":24,"upload_max_mb":0}`, 200)
	if svc.RetentionHours() != 24 || svc.MaxSizeMB() != 0 {
		t.Fatal("web values must be authoritative; zero size means unlimited")
	}
	w = httptest.NewRecorder()
	s.handleStatus(w, httptest.NewRequest("GET", "/api/admin/status", nil))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"provider":"local","status":"ok"`)) {
		t.Fatal("local status unavailable", w.Body.String())
	}
	peer, err := svc.SaveStorageInstance("", "独立临时与归档", "local", store.InstanceConfig{Directory: t.TempDir()}, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := svc.StoragePolicy()
	policy.Archives = []string{"local", peer}
	policy.Temporary = peer
	if err = svc.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleStatus(w, httptest.NewRequest("GET", "/api/admin/status", nil))
	var status struct {
		Checks []struct {
			InstanceID string `json:"instance_id"`
			Purpose    string `json:"purpose"`
			Status     string `json:"status"`
		} `json:"storage_checks"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Checks) != 4 {
		t.Fatal("must report both archives, publication and independent temp", w.Body.String())
	}
	want := map[string]bool{"local/归档": true, peer + "/归档": true, "local/永久发布": true, peer + "/临时存储": true}
	for _, check := range status.Checks {
		if !want[check.InstanceID+"/"+check.Purpose] || check.Status != "ok" {
			t.Fatal("wrong storage health coverage", w.Body.String())
		}
		delete(want, check.InstanceID+"/"+check.Purpose)
	}
}
