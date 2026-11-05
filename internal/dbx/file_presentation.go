package dbx

import (
	"context"
	"database/sql"
	"strings"
)

// FilePresentation contains persisted display metadata for an already authorized
// page. It never grants access to original bytes or replaces public-read checks.
type FilePresentation struct {
	Exhibited            bool
	PublicationEncrypted *bool
	TemporaryEncrypted   map[string]bool
}

// FilePresentations loads page display state in two queries, independent of the
// number of files or temporary copies. Callers must supply only authorized IDs.
func (db *DB) FilePresentations(ctx context.Context, ids []string, at int64) (map[string]FilePresentation, error) {
	out := make(map[string]FilePresentation, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := db.ReadQueryContext(ctx, `SELECT id,(`+exhibitedFileWhere+`),
		(SELECT encrypted FROM media_publications p WHERE p.file_id=files.id
		 AND p.backend=files.hot_backend AND p.object_key=files.published_path AND p.ready=1)
		FROM files WHERE id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var state FilePresentation
		var encrypted sql.NullBool
		if err = rows.Scan(&id, &state.Exhibited, &encrypted); err != nil {
			rows.Close()
			return nil, err
		}
		if encrypted.Valid {
			state.PublicationEncrypted = &encrypted.Bool
		}
		out[id] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	args = append(args, at)
	rows, err = db.ReadQueryContext(ctx, `SELECT o.file_id,m.object_id,m.encrypted
		FROM temp_objects o JOIN media_temporary m ON m.object_id=o.id
		WHERE o.file_id IN (`+marks+`) AND o.deleted_at IS NULL AND o.expires_at>?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, object string
		var encrypted bool
		if err = rows.Scan(&id, &object, &encrypted); err != nil {
			return nil, err
		}
		state, ok := out[id]
		if !ok {
			continue
		}
		if state.TemporaryEncrypted == nil {
			state.TemporaryEncrypted = make(map[string]bool)
		}
		state.TemporaryEncrypted[object] = encrypted
		out[id] = state
	}
	return out, rows.Err()
}
