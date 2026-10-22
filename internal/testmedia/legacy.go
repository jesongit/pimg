// Package testmedia builds explicitly legacy records for regression tests of
// historical carriers, conversion and repository synchronization. New media
// tests must use the normal upload API and per-storage replica records.
package testmedia

import (
	"encoding/json"
	"pimg/internal/dbx"
)

func LegacyRecord(db *dbx.DB, f *dbx.File) error {
	f.MediaVersion = 0
	f.MediaPolicies = nil
	var policy struct {
		Archives []string `json:"archives"`
	}
	if raw, err := db.GetSetting("storage.policy"); err == nil && json.Unmarshal([]byte(raw), &policy) == nil {
		for _, id := range policy.Archives {
			if id == "local" {
				f.ColdBackend = "local"
				break
			}
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM media_replicas WHERE file_id=?`, f.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE files SET media_version=0,cold_backend=? WHERE id=?`, f.ColdBackend, f.ID); err != nil {
		return err
	}
	return tx.Commit()
}
