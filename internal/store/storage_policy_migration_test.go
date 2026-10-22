package store

import (
	"bytes"
	"encoding/json"
	"testing"
)

func legacyConfigEnvelope(t *testing.T, s *Service, id string, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	g := s.configCipher()
	nonce := bytes.Repeat([]byte{byte(len(id) + 7)}, g.NonceSize())
	return g.Seal(nonce, nonce, b, []byte(id))
}

func TestStoragePolicyUpgradePreservesSecretsAndExplicitChoices(t *testing.T) {
	s, _, _ := newTestEnv(t)
	legacy := legacyConfigEnvelope(t, s, "telegram", map[string]any{"bootstrap": true, "managed": true, "options": map[string]string{"kept": "private"}})
	explicit := legacyConfigEnvelope(t, s, "r2", map[string]any{"encrypt": false, "split_video": false, "video_part_mb": 64, "priority": 17, "secret_key": "kept-secret", "access_key": "kept-access", "bucket": "kept-bucket"})
	s.DB.Exec(`UPDATE storage_instances SET config=? WHERE id='telegram'`, legacy)
	s.DB.Exec(`UPDATE storage_instances SET config=? WHERE id='r2'`, explicit)
	s.DB.Exec(`DELETE FROM settings WHERE k=?`, StoragePolicyVersionKey)
	s.DB.SetSetting("site.video_part_mb", "37")
	if err := s.migrateStoragePolicies(); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.DB.StorageInstances()
	before := map[string][]byte{}
	for _, row := range rows {
		before[row.ID] = append([]byte{}, row.Config...)
		c, err := s.unsealConfig(row.ID, row.Config)
		if err != nil {
			t.Fatal(err)
		}
		if row.ID == "telegram" && (!c.Encrypt || !c.SplitVideo || c.VideoPartMB != 37 || c.Options["kept"] != "private") {
			t.Fatal(c)
		}
		if row.ID == "r2" && (c.Encrypt || c.SplitVideo || c.VideoPartMB != 64 || c.Priority != 17 || c.SecretKey != "kept-secret" || c.AccessKey != "kept-access") {
			t.Fatal("explicit choices or credentials changed")
		}
	}
	if err := s.migrateStoragePolicies(); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.DB.StorageInstances()
	for _, row := range rows {
		if !bytes.Equal(row.Config, before[row.ID]) {
			t.Fatal("second startup resealed config", row.ID)
		}
	}
}

func TestStoragePolicyUpgradeRejectsCorruptionWithoutPartialWrites(t *testing.T) {
	s, _, _ := newTestEnv(t)
	legacy := legacyConfigEnvelope(t, s, "local", map[string]any{"directory": "kept"})
	s.DB.Exec(`UPDATE storage_instances SET config=? WHERE id='local'`, legacy)
	s.DB.Exec(`UPDATE storage_instances SET config=X'01' WHERE id='r2'`)
	s.DB.Exec(`DELETE FROM settings WHERE k=?`, StoragePolicyVersionKey)
	if err := s.migrateStoragePolicies(); err == nil {
		t.Fatal("corrupt config accepted")
	}
	var got []byte
	s.DB.QueryRow(`SELECT config FROM storage_instances WHERE id='local'`).Scan(&got)
	if !bytes.Equal(got, legacy) {
		t.Fatal("partial encrypted policy writes")
	}
	v, _ := s.DB.GetSetting(StoragePolicyVersionKey)
	if v != "" {
		t.Fatal("failed migration marked complete")
	}
}

func TestStoragePolicyUpgradeRejectsFutureEnvelope(t *testing.T) {
	s, _, _ := newTestEnv(t)
	future := legacyConfigEnvelope(t, s, "local", map[string]any{"policy_version": StoragePolicyVersion + 1})
	s.DB.Exec(`UPDATE storage_instances SET config=? WHERE id='local'`, future)
	if err := s.migrateStoragePolicies(); err == nil {
		t.Fatal("future policy accepted")
	}
}

func TestStoragePolicyUpgradeRollsBackInterruptedWrites(t *testing.T) {
	s, _, _ := newTestEnv(t)
	before := map[string][]byte{}
	for _, id := range []string{"local", "r2"} {
		b := legacyConfigEnvelope(t, s, id, map[string]any{"options": map[string]string{"kept": id}})
		before[id] = b
		if _, err := s.DB.Exec(`UPDATE storage_instances SET config=? WHERE id=?`, b, id); err != nil {
			t.Fatal(err)
		}
	}
	s.DB.Exec(`DELETE FROM settings WHERE k=?`, StoragePolicyVersionKey)
	_, err := s.DB.Exec(`CREATE TABLE policy_test_writes(n INTEGER); INSERT INTO policy_test_writes VALUES(0);
CREATE TRIGGER policy_test_fail BEFORE UPDATE ON storage_instances BEGIN
 UPDATE policy_test_writes SET n=n+1;
 SELECT CASE WHEN n>1 THEN RAISE(ABORT,'fixture interrupted') END FROM policy_test_writes;
END;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.migrateStoragePolicies(); err == nil {
		t.Fatal("interrupted migration accepted")
	}
	for id, want := range before {
		var got []byte
		s.DB.QueryRow(`SELECT config FROM storage_instances WHERE id=?`, id).Scan(&got)
		if !bytes.Equal(got, want) {
			t.Fatal("partial policy committed", id)
		}
	}
	var writes int
	s.DB.QueryRow(`SELECT n FROM policy_test_writes`).Scan(&writes)
	if writes != 0 {
		t.Fatal("earlier successful statement was not rolled back")
	}
	if v, _ := s.DB.GetSetting(StoragePolicyVersionKey); v != "" {
		t.Fatal("failed migration marked complete")
	}
}
