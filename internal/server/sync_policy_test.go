package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"pimg/internal/store"
)

func TestLocalSyncPolicyHTTPCompatibilityAndRestart(t *testing.T) {
	s := interactionServer(t)
	policy := s.Svc.StoragePolicy()
	policy.LocalMB = 48
	update := func(body []byte, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleStoragePolicy(w, httptest.NewRequest("PATCH", "/api/admin/storage/policy", bytes.NewReader(body)))
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if status == 200 {
			var response struct {
				Policy store.StoragePolicy `json:"policy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Policy.LocalMB != 48 {
				t.Fatal("saved value was not returned", response, err)
			}
		}
	}
	body, _ := json.Marshal(policy)
	update(body, 200)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	for _, invalid := range []any{0, -1, 1025, 1.5, "64"} {
		fields["local_mbps"] = invalid
		body, _ = json.Marshal(fields)
		update(body, 400)
		if s.Svc.StoragePolicy().LocalMB != 48 {
			t.Fatal("invalid update changed policy")
		}
	}
	delete(fields, "local_mbps")
	body, _ = json.Marshal(fields)
	update(body, 200)
	restored, err := store.New(s.Cfg, s.DB, s.Svc.Master, s.Svc.TG, s.Svc.R2, s.Log)
	if err != nil || restored.StoragePolicy().LocalMB != 48 {
		t.Fatal("restart did not preserve local copy limit", err)
	}
	// A pre-upgrade saved policy gets a default without rewriting its network
	// schedule or storage choices during startup.
	if err = s.DB.SetSetting("storage.policy", string(body)); err != nil {
		t.Fatal(err)
	}
	restored, err = store.New(s.Cfg, s.DB, s.Svc.Master, s.Svc.TG, s.Svc.R2, s.Log)
	if err != nil || restored.StoragePolicy().LocalMB != 64 || restored.StoragePolicy().NormalMB != policy.NormalMB || restored.StoragePolicy().AcceleratedMB != policy.AcceleratedMB {
		t.Fatal("old policy did not restore default local and original network limits", err)
	}
}
