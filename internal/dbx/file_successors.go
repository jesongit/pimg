package dbx

import "strings"

// FileSuccessor is presentation metadata for administrator audit views. The
// original file_id and its ownership remain the immutable historical identity.
type FileSuccessor struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Kind    string `json:"kind"`
}

func (db *DB) FileSuccessors(ids []string) (map[string]*FileSuccessor, error) {
	out := map[string]*FileSuccessor{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	rows, err := db.ReadQuery(`WITH RECURSIVE chain(root,id,kind,depth,visited) AS (
 SELECT id,id,'',0,'|'||id||'|' FROM files WHERE id IN (`+marks+`)
 UNION ALL
 SELECT p.root,c.target_id,CASE WHEN p.kind='transferred' OR old.owner_id!=next.owner_id THEN 'transferred' ELSE 'converted' END,p.depth+1,p.visited||c.target_id||'|'
 FROM chain p JOIN file_conversions c ON c.source_id=p.id AND c.phase='done'
 JOIN files old ON old.id=c.source_id JOIN files next ON next.id=c.target_id
 WHERE p.depth<32 AND instr(p.visited,'|'||c.target_id||'|')=0)
 SELECT c.root,f.id,f.owner_id,c.kind FROM chain c JOIN files f ON f.id=c.id WHERE c.depth>0 AND f.hidden=0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		next := &FileSuccessor{}
		if err = rows.Scan(&root, &next.ID, &next.OwnerID, &next.Kind); err != nil {
			return nil, err
		}
		out[root] = next
	}
	return out, rows.Err()
}
