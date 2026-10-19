package store

import (
	"context"
	"database/sql"
)

// ReconcileReferenceIdentities derives reference identities from the persisted
// call edges. References contain syntax facts; edges contain resolver facts.
// Keeping this as one set-based pass prevents reference binding from becoming
// a second resolver or a Go-side join.
func (s *Store) ReconcileReferenceIdentities(ctx context.Context, repoID int64) error {
	return reconcileReferenceIdentities(ctx, s.db, repoID)
}

// referenceIdentityDeriveSQL derives, for every reference staged in
// tmp_reference_scope, the symbol and context symbol its call edges agree on.
// Staging the references with a position index keeps the edge join a point
// lookup; joining references_tbl directly scans every reference of a file per
// edge (4.6 s against 0.2 s for 75k references).
const referenceIdentityDeriveSQL = `
WITH matched AS (
	SELECT r.id AS reference_id, e.id AS edge_id,
		e.src_symbol_id, e.dst_symbol_id
	FROM edges e
	JOIN tmp_reference_scope r
	  ON r.file_id = e.file_id
	 AND r.start_line = e.line
	 AND r.start_col = e.start_col
	WHERE e.repo_id = ? AND e.edge_kind = 'calls'
	  AND r.call_name IS NOT NULL
	  AND (
		e.dst_name = r.call_name
		OR (
			r.language = 'cpp'
			AND substr(e.evidence, -length(r.call_name)) = r.call_name
			AND substr(e.evidence, -length(r.call_name) - 1, 1) IN (':', '.', '>')
		)
	  )
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
	FROM tmp_reference_scope r
	LEFT JOIN matched m ON m.reference_id = r.id
	GROUP BY r.id
)
UPDATE references_tbl SET symbol_id = d.symbol_id, context_symbol_id = d.context_symbol_id
FROM derived d WHERE references_tbl.id = d.id
  AND (references_tbl.symbol_id IS NOT d.symbol_id OR references_tbl.context_symbol_id IS NOT d.context_symbol_id)`

func reconcileReferenceIdentities(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repoID int64) error {
	return reconcileReferenceIdentitiesForLanguages(ctx, q, repoID, nil)
}

// reconcileReferenceIdentitiesForLanguages is reconcileReferenceIdentities for
// the references of files in the given languages only; no languages means
// every reference of the repository.
func reconcileReferenceIdentitiesForLanguages(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repoID int64, languages []string) error {
	filter := ""
	args := []any{repoID}
	if len(languages) > 0 {
		filter = ` AND f.language IN (` + placeholders(len(languages)) + `)`
		for _, language := range languages {
			args = append(args, language)
		}
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS temp.tmp_reference_scope`,
		`CREATE TEMP TABLE tmp_reference_scope(id INTEGER PRIMARY KEY, file_id INTEGER, start_line INTEGER, start_col INTEGER, call_name TEXT, language TEXT)`,
	} {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO tmp_reference_scope
		SELECT r.id, r.file_id, r.start_line, r.start_col,
			CASE WHEN r.ref_kind = 'call' AND f.id IS NOT NULL
				THEN CASE WHEN r.qualified_name != '' THEN r.qualified_name ELSE r.name END
			END,
			f.language
		FROM references_tbl r
		LEFT JOIN files f ON f.id = r.file_id
		WHERE r.repo_id = ?`+filter, args...); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `CREATE INDEX temp.tmp_reference_scope_pos ON tmp_reference_scope(file_id, start_line, start_col) WHERE call_name IS NOT NULL`); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, referenceIdentityDeriveSQL, repoID); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DROP TABLE temp.tmp_reference_scope`)
	return err
}
