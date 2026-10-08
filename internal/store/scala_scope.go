package store

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"github.com/isink17/codegraph/internal/graph"
)

// Scala call ownership.
//
// Scala resolution is decided by the parser: an edge exists only for a bare
// call that Scala's scoping provably binds to one local def of the same file,
// and its evidence names that def's position (ScalaAdapter). This pass binds
// the edge to the one local symbol at that position or leaves it unresolved.
// The proof is per file: no other file can rebind a local def, so unlike the
// Lua pass nothing repository-wide gates it. No repo-wide strategy may answer
// a Scala call: members, inheritance, overloads, implicits, givens and
// extension methods decide every other one.

// scalaScopeVetoSQL keeps every repo-wide strategy off Scala calls.
const scalaScopeVetoSQL = `NOT (f.language = 'scala' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// scalaScopeOwned is the Go-side twin of scalaScopeVetoSQL.
func scalaScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "scala" && t.edgeKind == EdgeKindCalls
}

func resolveScalaScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) (int, error) {
	targets := map[int64][]int64{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id, s.id
FROM edges e
JOIN files f ON f.id = e.file_id
JOIN symbols s ON s.repo_id = e.repo_id AND s.file_id = e.file_id
 AND s.language = 'scala' AND s.kind = 'function' AND s.name = e.dst_name
 AND substr(s.stable_key, 1, 17) = 'func:scala:local:'
 AND e.evidence = '`+graph.ScalaLocalFunctionEvidence+`' || s.start_line || ':' || s.start_col
WHERE e.repo_id = ? AND f.language = 'scala' AND e.edge_kind = '`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var edgeID, symbolID int64
			if err := rows.Scan(&edgeID, &symbolID); err != nil {
				return err
			}
			targets[edgeID] = append(targets[edgeID], symbolID)
			return nil
		}); err != nil {
		return 0, err
	}
	confidence := resolutionConfidenceFor(ResolutionStrategyScalaLocalFunction)
	bound := 0
	for _, edgeID := range slices.Sorted(maps.Keys(targets)) {
		// The position names one declaration; anything else is a stale or
		// inconsistent store and stays unresolved.
		if len(targets[edgeID]) != 1 {
			continue
		}
		// ponytail: one UPDATE per bound edge; batch through a temp table if
		// Scala-heavy repositories make this measurable.
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`,
			targets[edgeID][0], ResolutionStrategyScalaLocalFunction, confidence, edgeID); err != nil {
			return 0, err
		}
		bound++
	}
	return bound, nil
}

// resolveScalaScopeStandalone runs the pass in its own transaction, for
// callers holding a pooled *sql.DB.
func (s *Store) resolveScalaScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveScalaScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
