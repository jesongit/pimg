package store

import (
	"errors"

	"pimg/internal/dbx"
)

// Keep the permanent instance's historical ID so archive locations, published
// URLs and in-flight jobs never have to be rewritten during the split.
const defaultR2Temporary = "r2-temp"

func (s *Service) splitDefaultR2(rows []dbx.StorageInstance) ([]dbx.StorageInstance, error) {
	permanent, temporary := -1, -1
	for n, row := range rows {
		if row.ID != "r2" && row.ID != defaultR2Temporary {
			continue
		}
		c, err := s.unsealConfig(row.ID, row.Config)
		if err != nil {
			return nil, err
		}
		if row.Driver != "r2" || !c.Bootstrap {
			if row.ID == defaultR2Temporary {
				return nil, errors.New("R2 临时存储的保留编号已被其他实例占用")
			}
			continue
		}
		if row.ID == "r2" {
			permanent = n
		} else {
			temporary = n
		}
	}
	if permanent < 0 {
		return rows, nil
	}
	c, _ := s.unsealConfig(rows[permanent].ID, rows[permanent].Config)
	if !c.Managed && rows[permanent].Name != "R2 永久存储" {
		rows[permanent].Name = "R2 永久存储"
		if err := s.DB.SaveStorageInstance(rows[permanent]); err != nil {
			return nil, err
		}
	}
	if temporary < 0 {
		sealed, err := s.sealConfig(defaultR2Temporary, InstanceConfig{Bootstrap: true})
		if err != nil {
			return nil, err
		}
		row := dbx.StorageInstance{ID: defaultR2Temporary, Name: "R2 临时存储", Driver: "r2", Config: sealed, Enabled: rows[permanent].Enabled}
		if err := s.DB.SaveStorageInstance(row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Service) publicR2Temporary(backend string) bool {
	// Old temporary rows keep backend=r2; their original bucket and URL remain
	// authoritative even though new rows now bind to the single-bucket instance.
	if s.Cfg.DevMock {
		return (backend == "r2" || backend == defaultR2Temporary) && s.Cfg.R2.TempPublicBaseURL != ""
	}
	if backend == "r2" {
		backend = defaultR2Temporary
	}
	i, e := s.instance(backend)
	return e == nil && i.row.Driver == "r2" && i.config.TempPublicBase != "" && i.hot != nil
}
