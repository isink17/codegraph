package store

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"github.com/isink17/codegraph/internal/graph"
)

// Lua call ownership.
//
// Lua resolution is decided entirely by the parser: an edge exists only for a
// bare call whose innermost lexical binding is a never-reassigned
// `local function` statement of the same file, and its evidence names that
// statement's position. This pass binds the edge to the one symbol at that
// position or leaves it unresolved. No repo-wide strategy may answer a Lua
// call: a global, field, method or module lookup by name is not provable
// without modelling run-time table and environment state.

// luaScopeVetoSQL keeps every repo-wide strategy off Lua calls.
const luaScopeVetoSQL = `NOT (f.language = 'lua' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// luaScopeOwned is the Go-side twin of luaScopeVetoSQL.
func luaScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "lua" && t.edgeKind == EdgeKindCalls
}

func resolveLuaScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) (int, error) {
	targets := map[int64][]int64{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id, s.id
FROM edges e
JOIN files f ON f.id = e.file_id
JOIN symbols s ON s.repo_id = e.repo_id AND s.file_id = e.file_id
 AND s.language = 'lua' AND s.kind = 'function' AND s.name = e.dst_name
 AND substr(s.stable_key, 1, 15) = 'func:lua:local:'
 AND e.evidence = '`+graph.LuaLocalFunctionEvidence+`' || s.start_line || ':' || s.start_col
WHERE e.repo_id = ? AND f.language = 'lua' AND e.edge_kind = '`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL`,
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
	confidence := resolutionConfidenceFor(ResolutionStrategyLuaLocalFunction)
	bound := 0
	for _, edgeID := range slices.Sorted(maps.Keys(targets)) {
		// The position names one declaration; anything else is a stale or
		// inconsistent store and stays unresolved.
		if len(targets[edgeID]) != 1 {
			continue
		}
		// ponytail: one UPDATE per bound edge; batch through a temp table if
		// Lua-heavy repositories make this measurable.
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`,
			targets[edgeID][0], ResolutionStrategyLuaLocalFunction, confidence, edgeID); err != nil {
			return 0, err
		}
		bound++
	}
	return bound, nil
}

// resolveLuaScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB.
func (s *Store) resolveLuaScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveLuaScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
