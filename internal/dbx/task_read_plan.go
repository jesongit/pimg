package dbx

import "strings"

// Search text is a separate, expensive read concern. Ordinary navigation does
// not need to concatenate the names/errors of every historical member.
func taskReadCTE(base string, search bool, active bool) string {
	if !search {
		for _, expr := range []string{
			`GROUP_CONCAT(name||' '||file_id||' '||error,' ')`,
			`GROUP_CONCAT(CASE WHEN i.state='failed' THEN i.path||' '||i.error END,' ')`,
			`GROUP_CONCAT(name||' '||file_id||' '||tool_file_id||' '||upload_id||' '||reason||' '||error,' ')`,
		} {
			base = strings.ReplaceAll(base, expr, `''`)
		}
		start := strings.Index(base, " timeline_member_content AS MATERIALIZED (")
		if start >= 0 {
			end := strings.Index(base[start:], " timeline_receipt_revisions AS MATERIALIZED (")
			if end > 0 {
				base = base[:start] + ` timeline_member_content AS MATERIALIZED (SELECT '' group_key,'' search_text WHERE 0),
` + base[start+end:]
			}
		}
	}
	if active {
		// Candidate groups are cheap and deliberately inclusive: keep all attempts
		// in a candidate group, then apply the usual newest-attempt/state rules.
		// A superseded pending attempt may overselect, but can never hide real work.
		base = strings.Replace(base, ` ranked_members AS MATERIALIZED (`, ` active_candidate_groups AS MATERIALIZED (
 SELECT DISTINCT m.group_key FROM task_members m
 LEFT JOIN task_operations o ON o.id=m.group_operation_id LEFT JOIN import_bounds b ON b.id=m.batch_id
 WHERE m.state IN ('pending','running') OR o.input_state='open' OR b.state IN ('paused','running','scanning')),
 ranked_members AS MATERIALIZED (`, 1)
		base = strings.Replace(base, ` FROM task_members m)`, ` FROM task_members m WHERE m.group_key IN (SELECT group_key FROM active_candidate_groups))`, 1)
	}
	return base
}

func historyReadCTE(base string) string {
	// Logs without a file/group context exclude active work. Resolve that cheap
	// state first, before constructing per-file progress and JSON for the backlog.
	// Receipt bindings still use ALL group keys, so active intakes cannot leak
	// back into history as separate receipt rows.
	base = strings.Replace(base, ` grouped_file_states AS MATERIALIZED (`, ` active_history_groups AS MATERIALIZED (
 SELECT m.group_key FROM ranked_members m
 LEFT JOIN task_operations o ON o.id=m.group_operation_id LEFT JOIN import_bounds b ON b.id=m.batch_id
 GROUP BY m.group_key HAVING MAX(m.relevance=1 AND m.type!='operation_intake' AND m.state IN ('pending','running'))=1
 OR MAX(COALESCE(o.input_state='open',0) OR COALESCE(b.state IN ('paused','running','scanning'),0))=1
 OR MAX(COALESCE(b.state,o.input_state,'')='ready')=1),
 history_members AS MATERIALIZED (SELECT * FROM ranked_members WHERE group_key NOT IN (SELECT group_key FROM active_history_groups)),
 grouped_file_states AS MATERIALIZED (`, 1)
	base = strings.Replace(base, `FROM ranked_members WHERE relevance=1`, `FROM history_members WHERE relevance=1`, 1)
	base = strings.Replace(base, `FROM ranked_members m LEFT JOIN import_bounds`, `FROM history_members m LEFT JOIN import_bounds`, 1)
	base = strings.Replace(base, `EXISTS(SELECT 1 FROM grouped_tasks g WHERE g.group_key=r.candidate_key)`, `EXISTS(SELECT 1 FROM task_members g WHERE g.group_key=r.candidate_key)`, 1)
	return base
}
