package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"pimg/internal/cryptox"
	"pimg/internal/dbx"
)

// ValidateStorageCredentials checks connection envelopes without making a
// damaged/retired media record block a storage configuration upgrade.
func ValidateStorageCredentials(db *dbx.DB, master *cryptox.Master) error {
	s := &Service{DB: db, Master: master}
	var table int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='storage_instances' AND type='table'`).Scan(&table); err != nil {
		return err
	}
	if table > 0 {
		instances, err := db.StorageInstances()
		if err != nil {
			return err
		}
		for _, row := range instances {
			if _, err := s.unsealConfig(row.ID, row.Config); err != nil {
				return fmt.Errorf("存储凭据无法解密: %w", err)
			}
		}
	}
	if raw, err := db.GetSetting(connectionKey); err != nil {
		return err
	} else if raw != "" {
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return fmt.Errorf("TG 连接配置损坏: %w", err)
		}
		c, err := s.unsealConfig(connectionKey, b)
		if err != nil {
			return err
		}
		var connection StorageConnection
		if err = json.Unmarshal([]byte(c.Options["connection"]), &connection); err != nil {
			return fmt.Errorf("TG 连接配置损坏: %w", err)
		}
	}
	return nil
}

// ValidateMigrationKeys additionally checks file keys when restoring a package.
func ValidateMigrationKeys(db *dbx.DB, master *cryptox.Master) error {
	if err := ValidateStorageCredentials(db, master); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT key_wrapped,key_nonce FROM files WHERE length(key_wrapped)>0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var wrapped, nonce []byte
		if err := rows.Scan(&wrapped, &nonce); err != nil {
			return err
		}
		if _, err := master.UnwrapFileKey(wrapped, nonce); err != nil {
			return fmt.Errorf("文件密钥无法解密: %w", err)
		}
	}
	return rows.Err()
}

// RebaseMigrationStorage keeps encrypted credentials and stable instance IDs,
// changing only local paths when a restored instance lives at a new location.
func RebaseMigrationStorage(db *dbx.DB, master *cryptox.Master, oldRoot, newRoot string, external map[string]string) error {
	s := &Service{DB: db, Master: master}
	rows, err := db.StorageInstances()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Driver != "local" {
			continue
		}
		c, err := s.unsealConfig(row.ID, row.Config)
		if err != nil {
			return err
		}
		mapPath := func(p string) string {
			if strings.HasPrefix(p, "@data/") {
				return p
			}
			if target, ok := external[p]; ok {
				return target
			}
			a, b := strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/"), strings.TrimRight(strings.ReplaceAll(oldRoot, "\\", "/"), "/")
			if strings.EqualFold(a, b) {
				return newRoot
			}
			if len(a) > len(b) && strings.EqualFold(a[:len(b)], b) && a[len(b)] == '/' {
				return filepath.Join(newRoot, filepath.FromSlash(a[len(b)+1:]))
			}
			return p
		}
		c.Directory, c.ColdDirectory = mapPath(c.Directory), mapPath(c.ColdDirectory)
		sealed, err := s.sealConfig(row.ID, c)
		if err != nil {
			return err
		}
		if _, err = db.Exec(`UPDATE storage_instances SET config=? WHERE id=?`, sealed, row.ID); err != nil {
			return err
		}
	}
	return nil
}

// MigrationLocalDirectories returns resolved roots, without exposing credentials.
func (s *Service) MigrationLocalDirectories() map[string][]string {
	out := map[string][]string{}
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	for id, i := range s.storages.instances {
		if i.row.Driver != "local" {
			continue
		}
		for _, p := range []string{i.config.Directory, i.config.ColdDirectory} {
			if p != "" {
				out[id] = append(out[id], s.localPath(p))
			}
		}
	}
	return out
}
