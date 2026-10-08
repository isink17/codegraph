package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ReconcileReferenceIdentities derives reference identities from the persisted
// call edges. References contain syntax facts; edges contain resolver facts.
// Keeping this as one set-based pass prevents reference binding from becoming
// a second resolver or a Go-side join.
func (s *Store) ReconcileReferenceIdentities(ctx context.Context, repoID int64) error {
	return reconcileReferenceIdentities(ctx, s.db, repoID)
}

// referenceIdentityDerivedSQL derives, for every call reference of the
// repository, the symbol and context symbol its call edges agree on. The one %s
// is an optional extra reference filter.
const referenceIdentityDerivedSQL = `
WITH matched AS (
			SELECT r.id AS reference_id, e.id AS edge_id,
				e.src_symbol_id, e.dst_symbol_id
			FROM references_tbl r
			JOIN edges e
			  ON e.repo_id = r.repo_id
			 AND e.file_id = r.file_id
			 AND e.line = r.start_line
			 AND e.start_col = r.start_col
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
			  AND e.start_col IS NOT NULL
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
			WHERE r.repo_id = ?%s
			GROUP BY r.id
		)
`

func reconcileReferenceIdentities(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repoID int64) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf(referenceIdentityDerivedSQL, "")+`
		UPDATE references_tbl AS r
		SET symbol_id = (SELECT d.symbol_id FROM derived d WHERE d.id = r.id),
			context_symbol_id = (SELECT d.context_symbol_id FROM derived d WHERE d.id = r.id)
		WHERE r.repo_id = ?
	`, repoID, repoID, repoID)
	return err
}

// reconcileReferenceIdentitiesForLanguages is reconcileReferenceIdentities for
// the references of files in the given languages only. The scoped derivation is
// materialised once and joined: written as the repo-wide statement with a file
// filter, the planner re-runs the derivation per updated row (20 s against
// 0.4 s for 24k references).
func reconcileReferenceIdentitiesForLanguages(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repoID int64, languages []string) error {
	if len(languages) == 0 {
		return reconcileReferenceIdentities(ctx, q, repoID)
	}
	scope := ` AND r.file_id IN (SELECT id FROM files WHERE repo_id = ? AND language IN (` + placeholders(len(languages)) + `))`
	scopeArgs := []any{repoID}
	for _, language := range languages {
		scopeArgs = append(scopeArgs, language)
	}
	args := append([]any{repoID, repoID}, scopeArgs...)
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS temp.tmp_reference_identity`,
		`CREATE TEMP TABLE tmp_reference_identity(id INTEGER PRIMARY KEY, symbol_id INTEGER, context_symbol_id INTEGER)`,
	} {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, fmt.Sprintf(referenceIdentityDerivedSQL, scope)+`
		INSERT INTO tmp_reference_identity SELECT id, symbol_id, context_symbol_id FROM derived`, args...); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `UPDATE references_tbl SET symbol_id = d.symbol_id, context_symbol_id = d.context_symbol_id
		FROM tmp_reference_identity d WHERE references_tbl.id = d.id`); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DROP TABLE temp.tmp_reference_identity`)
	return err
}
