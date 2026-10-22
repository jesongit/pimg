package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

const StoragePolicyVersion = 1
const StoragePolicyVersionKey = "storage.policy_version"

// This migration runs before clients or background work are constructed. Only
// absent fields inherit legacy semantics; explicit false/size/priority survive.
func (s *Service) migrateStoragePolicies() error {
	settings, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	v := 0
	if raw := settings[StoragePolicyVersionKey]; raw != "" {
		v, err = strconv.Atoi(raw)
		if err != nil || v < 0 || v > StoragePolicyVersion {
			return fmt.Errorf("存储策略版本 %q 不受当前程序支持", raw)
		}
	}
	rows, err := s.DB.StorageInstances()
	if err != nil {
		return err
	}
	legacyPartMB := 512
	if n, e := strconv.Atoi(settings["site.video_part_mb"]); e == nil && n > 0 && n <= 4096 {
		legacyPartMB = n
	}
	updates := map[string][]byte{}
	for _, row := range rows {
		plain, e := s.unsealConfigJSON(row.ID, row.Config)
		if e != nil {
			return fmt.Errorf("升级存储 %s: %w", row.ID, e)
		}
		var c InstanceConfig
		var fields map[string]json.RawMessage
		if e = json.Unmarshal(plain, &c); e != nil {
			return e
		}
		if e = json.Unmarshal(plain, &fields); e != nil {
			return e
		}
		if c.PolicyVersion < 0 || c.PolicyVersion > StoragePolicyVersion {
			return fmt.Errorf("存储 %s 的配置版本 %d 不受当前程序支持", row.ID, c.PolicyVersion)
		}
		if c.PolicyVersion == StoragePolicyVersion {
			continue
		}
		if _, ok := fields["encrypt"]; !ok {
			c.Encrypt = true
		}
		if _, ok := fields["split_video"]; !ok {
			c.SplitVideo = true
		}
		if _, ok := fields["video_part_mb"]; !ok {
			c.VideoPartMB = legacyPartMB
		}
		c.PolicyVersion = StoragePolicyVersion
		sealed, e := s.sealConfig(row.ID, c)
		if e != nil {
			return e
		}
		updates[row.ID] = sealed
	}
	// Verify the connection envelope too, before committing any new policies.
	if raw := settings[connectionKey]; raw != "" {
		b, e := base64.StdEncoding.DecodeString(raw)
		if e != nil {
			return fmt.Errorf("TG 连接配置损坏: %w", e)
		}
		if _, e = s.unsealConfig(connectionKey, b); e != nil {
			return e
		}
	}
	if v == StoragePolicyVersion && len(updates) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, b := range updates {
		if _, err = tx.Exec(`UPDATE storage_instances SET config=? WHERE id=?`, b, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, StoragePolicyVersionKey, strconv.Itoa(StoragePolicyVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
