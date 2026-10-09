package store

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"github.com/isink17/codegraph/internal/graph"
)

// Dart call ownership.
//
// Dart resolution is decided by the parser: an edge exists only for a bare
// call that lexical scoping binds to one local or top-level function of the
// same file, and its evidence names that declaration's position
// (dartLexicalCalls). This pass binds the edge to the one function symbol at
// that position or leaves it unresolved. The proof is per file: a top-level
// target is proven only in a library with no part files, and no other library
// can rebind a local or library declaration, so unlike the Lua pass nothing
// repository-wide gates it. No repo-wide strategy may answer a Dart call: a
// member, extension, import or part lookup by name is not provable without
// the receiver's static type and the library's file set.

// dartScopeVetoSQL keeps every repo-wide strategy off Dart calls.
const dartScopeVetoSQL = `NOT (f.language = 'dart' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// dartScopeOwned is the Go-side twin of dartScopeVetoSQL.
func dartScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "dart" && t.edgeKind == EdgeKindCalls
}

func resolveDartScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) (int, error) {
	targets := map[int64][]int64{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id, s.id
FROM edges e
JOIN files f ON f.id = e.file_id
JOIN symbols s ON s.repo_id = e.repo_id AND s.file_id = e.file_id
 AND s.language = 'dart' AND s.kind = 'function' AND s.name = e.dst_name
 AND substr(s.stable_key, 1, 10) = 'func:dart:'
 AND e.evidence = '`+graph.DartLexicalFunctionEvidence+`' || s.start_line || ':' || s.start_col
WHERE e.repo_id = ? AND f.language = 'dart' AND e.edge_kind = '`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL`,
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
	confidence := resolutionConfidenceFor(ResolutionStrategyDartLexicalFunction)
	bound := 0
	for _, edgeID := range slices.Sorted(maps.Keys(targets)) {
		// The position names one declaration; anything else is a stale or
		// inconsistent store and stays unresolved.
		if len(targets[edgeID]) != 1 {
			continue
		}
		// ponytail: one UPDATE per bound edge; batch through a temp table if
		// Dart-heavy repositories make this measurable.
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`,
			targets[edgeID][0], ResolutionStrategyDartLexicalFunction, confidence, edgeID); err != nil {
			return 0, err
		}
		bound++
	}
	return bound, nil
}

// resolveDartScopeStandalone runs the pass in its own transaction, for
// callers holding a pooled *sql.DB.
func (s *Store) resolveDartScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveDartScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
