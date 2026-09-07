package store

import (
	"context"
)

// ReconcileReferenceIdentities derives reference identities from the persisted
// call edges. References contain syntax facts; edges contain resolver facts.
// Keeping this as one set-based pass prevents reference binding from becoming
// a second resolver or a Go-side join.
func (s *Store) ReconcileReferenceIdentities(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `
		WITH matched AS (
			SELECT r.id AS reference_id, e.id AS edge_id,
				e.src_symbol_id, e.dst_symbol_id
			FROM references_tbl r
			JOIN edges e
			  ON e.repo_id = r.repo_id
			 AND e.file_id = r.file_id
			 AND e.line = r.start_line
			 AND e.edge_kind = 'calls'
			JOIN files f ON f.id = e.file_id
			 AND (
				e.dst_name = CASE
					WHEN r.qualified_name != '' THEN r.qualified_name
					ELSE r.name
				END
				OR (
					f.language = 'cpp'
					AND substr(e.evidence, -length(CASE WHEN r.qualified_name != '' THEN r.qualified_name ELSE r.name END)) = CASE WHEN r.qualified_name != '' THEN r.qualified_name ELSE r.name END
					AND substr(e.evidence, -length(CASE WHEN r.qualified_name != '' THEN r.qualified_name ELSE r.name END) - 1, 1) IN (':', '.', '>')
				)
			 )
			WHERE r.repo_id = ? AND r.ref_kind = 'call'
		), derived AS (
			SELECT r.id,
				CASE
					WHEN COUNT(m.edge_id) > 0
					 AND COUNT(DISTINCT m.dst_symbol_id) = 1
					 AND SUM(CASE WHEN m.dst_symbol_id IS NULL THEN 1 ELSE 0 END) = 0
					THEN MIN(m.dst_symbol_id)
					ELSE NULL
				END AS symbol_id,
				CASE
					WHEN COUNT(m.edge_id) > 0
					 AND COUNT(DISTINCT m.src_symbol_id) = 1
					 AND SUM(CASE WHEN m.src_symbol_id IS NULL THEN 1 ELSE 0 END) = 0
					THEN MIN(m.src_symbol_id)
					ELSE NULL
				END AS context_symbol_id
			FROM references_tbl r
			LEFT JOIN matched m ON m.reference_id = r.id
			WHERE r.repo_id = ?
			GROUP BY r.id
		)
		UPDATE references_tbl AS r
		SET symbol_id = (SELECT d.symbol_id FROM derived d WHERE d.id = r.id),
			context_symbol_id = (SELECT d.context_symbol_id FROM derived d WHERE d.id = r.id)
		WHERE r.repo_id = ?
	`, repoID, repoID, repoID)
	return err
}
