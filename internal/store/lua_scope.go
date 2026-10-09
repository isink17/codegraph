package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// Lua call ownership.
//
// Lua resolution is decided entirely by the parser: an edge exists only for a
// bare call whose innermost lexical binding is a never-reassigned
// `local function` statement of the same file, and its evidence names that
// statement's position. This pass binds the edge to the one symbol at that
// position or leaves it unresolved, and binds nothing at all while any indexed
// Lua file can reach the debug library. No repo-wide strategy may answer a Lua
// call: a global, field, method or module lookup by name is not provable
// without modelling run-time table and environment state.

// luaScopeVetoSQL keeps every repo-wide strategy off Lua calls.
const luaScopeVetoSQL = `NOT (f.language = 'lua' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// luaScopeOwned is the Go-side twin of luaScopeVetoSQL.
func luaScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "lua" && t.edgeKind == EdgeKindCalls
}

// isLuaPath reports whether a changed path can change a Lua decision.
func isLuaPath(p string) bool { return strings.EqualFold(filepath.Ext(p), ".lua") }

// luaDebugReachableSQL reports whether any live Lua file of the repository
// lacks the parser's debug-free evidence: one whose code can reach the debug
// library, or that was not scanned (an oversize or failed parse, the non-cgo
// fallback, an older profile).
const luaDebugReachableSQL = `SELECT EXISTS(SELECT 1 FROM files f
WHERE f.repo_id = ? AND f.language = 'lua' AND f.is_deleted = 0
  AND NOT EXISTS (SELECT 1 FROM file_scope_evidence fs WHERE fs.repo_id = f.repo_id AND fs.file_id = f.id AND fs.language = 'lua'))`

// resolveLuaScope re-decides every Lua call of the repository. The debug
// library rewrites the locals and upvalues of any function in the Lua state
// (setupvalue and upvaluejoin on an exported closure, setlocal from a hook or
// a callback), so one indexed file that can reach it withdraws every lexical
// proof, including those of files it never names. The decision depends on
// every Lua file, so the pass clears and rebinds all of them each time it
// runs, as the HCL pass does.
//
// ponytail: an update can run the pass up to three times (binder batch,
// suffix pass, repo-wide); each run is two set-based statements.
func resolveLuaScope(ctx context.Context, q execQuerier, repoID int64) (int, error) {
	if _, err := q.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+`
		WHERE repo_id = ? AND edge_kind = '`+EdgeKindCalls+`' AND (dst_symbol_id IS NOT NULL OR COALESCE(resolution_strategy, '') <> '' OR COALESCE(resolution_confidence, '') <> '') AND file_id IN (SELECT id FROM files WHERE repo_id = ? AND language = 'lua')`, repoID, repoID); err != nil {
		return 0, err
	}
	var reachable bool
	if err := sqliteScanRows(ctx, q, luaDebugReachableSQL, []any{repoID}, func(rows *sql.Rows) error {
		return rows.Scan(&reachable)
	}); err != nil || reachable {
		return 0, err
	}
	// The start_line term is implied by the evidence equality; with the index
	// hint it turns the symbol lookup into a point probe by file and line
	// instead of a scan of every same-named symbol of the repository.
	// The position names one declaration; a stale or inconsistent store with
	// several matching symbols leaves the edge unresolved.
	res, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = t.sid, resolution_strategy = ?, resolution_confidence = ?
FROM (SELECT e.id AS eid, MIN(s.id) AS sid
FROM edges e
JOIN files f ON f.id = e.file_id
JOIN symbols s INDEXED BY idx_symbols_file_start ON s.repo_id = e.repo_id AND s.file_id = e.file_id
 AND s.start_line = CAST(substr(e.evidence, `+strconv.Itoa(len(graph.LuaLocalFunctionEvidence)+1)+`) AS INTEGER)
 AND s.language = 'lua' AND s.kind = 'function' AND s.name = e.dst_name
 AND substr(s.stable_key, 1, 15) = 'func:lua:local:'
 AND e.evidence = '`+graph.LuaLocalFunctionEvidence+`' || s.start_line || ':' || s.start_col
WHERE e.repo_id = ? AND f.language = 'lua' AND e.edge_kind = '`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL
GROUP BY e.id HAVING COUNT(*) = 1) AS t
WHERE edges.id = t.eid`,
		ResolutionStrategyLuaLocalFunction, resolutionConfidenceFor(ResolutionStrategyLuaLocalFunction), repoID)
	if err != nil {
		return 0, err
	}
	bound, err := res.RowsAffected()
	return int(bound), err
}

// resolveLuaScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB.
func (s *Store) resolveLuaScopeStandalone(ctx context.Context, repoID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveLuaScope(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
