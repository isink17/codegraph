package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// Ruby implicit/self scope (P22.47).
//
// This pass OWNS every ordinary Ruby call edge (`edge_kind = 'calls'`).
// Ownership means two things, and the second holds even when the first proves
// nothing:
//
//  1. It binds a call whose receiver identity is already syntax-proven by the
//     P22.46 parser -- implicit `run()` and literal `self.run()` / `self::run()`
//     -- to the one eligible method of the caller's own semantic container.
//  2. Every other Ruby call is withheld from the generic strategies
//     (exact_name, exact_qualified, receiver_method, dot_tail*, dot_suffix):
//     in SQL by rubyScopeVetoSQL inside resolverBindableCandidateSQL, in the
//     Go-side binder by rubyScopeOwned dropping the target. Constant, value,
//     chained and safe-navigation receivers, class/module body calls and
//     top-level calls stay unresolved on purpose; repository-wide name matching
//     does not understand Ruby receivers and would invent an owner.
//
// The veto is a static predicate rather than a per-edge temp table because
// unsupported receiver forms dominate Ruby call volume and can never bind in
// this phase; materialising them on every incremental save would cost O(all
// unresolved Ruby calls) for nothing.
//
// The receiver's runtime identity comes from the SOURCE method, never from the
// spelling: an instance method's implicit/literal self is an instance of its
// container, a singleton method's is the class/module object. Source staticness
// therefore selects destination staticness exactly, and the two natures are
// never mixed. `def run` and `def self.run` on one container are distinct
// methods, not an ambiguity. Two eligible declarations of the same nature are
// an ambiguity: Ruby's runtime load order is not modelled, so the call fails
// closed rather than guessing a row.
//
// Method visibility is deliberately not consulted: the receiver is exactly
// self, which may call private and protected methods. Constant receivers
// (P22.48) will need a visibility audit before they can bind.
//
// There is no one-time resolver repair for this pass. The parser facts it
// consumes changed with it, so treesitter:ruby:v3 is the compatibility
// boundary: a repository indexed under v2 reparses every Ruby file on its next
// full scan, which replaces the edge rows themselves. A resolver repair can
// only re-decide bindings, and a v2 edge row for `self.name = v` or for a
// self-rebinding block is stale evidence no re-decision could make truthful.

const rubyScopeResolution = "tmp_ruby_scope_resolution"

// rubyScopeStrategies is every strategy this pass writes; the incremental
// invalidation keys on it.
var rubyScopeStrategies = []string{
	ResolutionStrategyRubyImplicitSelf,
	ResolutionStrategyRubyExplicitSelf,
}

// rubyScopeVetoSQL keeps every repo-wide strategy off ordinary Ruby calls. It
// requires the surroundings every strategy UPDATE already has: the update
// target is `edges` and `files` is joined in as `f`. Cross-language reference
// edges (P22.37) are not ordinary calls and are untouched.
const rubyScopeVetoSQL = `NOT (f.language = 'ruby' AND edges.edge_kind = '` + EdgeKindCalls + `')`

// rubyScopeOwned is the Go-side twin of rubyScopeVetoSQL.
func rubyScopeOwned(t edgeTarget) bool {
	return t.srcLanguage == "ruby" && t.edgeKind == EdgeKindCalls
}

// Parser evidence, not punctuation, decides the receiver category (P22.46).
const (
	rubyImplicitReceiver = "ruby:implicit_receiver"
	rubySelfReceiver     = "ruby:self_receiver"
)

// rubyScopeMethod returns the called method name and the strategy to record for
// a self-identified receiver, or ok=false when the spelling is not exactly a
// bare name or a literal `self.`/`self::` prefix on one. Anything else -- a
// chain, an operator, a receiver the evidence did not promise -- fails closed.
func rubyScopeMethod(evidence, dstName string) (method, strategy string, ok bool) {
	switch evidence {
	case rubyImplicitReceiver:
		method, strategy = dstName, ResolutionStrategyRubyImplicitSelf
	case rubySelfReceiver:
		rest, found := strings.CutPrefix(dstName, "self.")
		if !found {
			if rest, found = strings.CutPrefix(dstName, "self::"); !found {
				return "", "", false
			}
		}
		method, strategy = rest, ResolutionStrategyRubyExplicitSelf
	default:
		return "", "", false
	}
	if method == "" || strings.ContainsAny(method, ".:&(") {
		return "", "", false
	}
	return method, strategy, true
}

// rubyScopeQuery is the read/write surface the pass needs; *sql.Tx and *sql.DB
// both satisfy it. Callers on a pooled *sql.DB must wrap the pass in a
// transaction (resolveRubyScopeStandalone) so the resolution temp table stays
// on one connection.
type rubyScopeQuery = javaQuery

type rubyScopeEdge struct {
	id        int64
	fileID    int64
	evidence  string
	dstName   string
	container string
	static    int64
}

type rubyScopeBinding struct {
	dst      int64
	strategy string
}

// resolveRubyScope runs the pass over every unresolved ordinary Ruby call edge
// of the repository, or only over `only` when it is non-empty, and reports how
// many edges it bound. Unsupported receiver forms are owned but never loaded:
// nothing here can bind them, and the veto that keeps the generic strategies
// off them is a static predicate.
func resolveRubyScope(ctx context.Context, q rubyScopeQuery, repoID int64, only map[int64]struct{}) (int, error) {
	// The source method is required, not optional: an edge with no trustworthy
	// source symbol cannot prove whose self this is. It is still owned, so it
	// simply never appears here and stays unresolved.
	var edges []rubyScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.evidence,e.dst_name,src.container_name,src.is_static
FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id
WHERE e.repo_id=? AND f.language='ruby' AND e.edge_kind='`+EdgeKindCalls+`' AND e.dst_symbol_id IS NULL
  AND e.evidence IN ('`+rubyImplicitReceiver+`','`+rubySelfReceiver+`')
  AND src.repo_id=e.repo_id AND src.language='ruby' AND src.kind='function'
  AND src.container_name<>'' AND src.is_static IS NOT NULL`,
		" AND e.id IN (%s)", []any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e rubyScopeEdge
			if err := rows.Scan(&e.id, &e.fileID, &e.evidence, &e.dstName, &e.container, &e.static); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}

	// One exact qualified name per wanted member, loaded in batches: never a
	// query per edge, and never a scan of every Ruby method named `run`.
	type want struct {
		method, strategy, qname string
	}
	wants := make(map[int64]want, len(edges))
	qnameSet := make(map[string]struct{}, len(edges))
	for _, e := range edges {
		method, strategy, ok := rubyScopeMethod(e.evidence, e.dstName)
		if !ok {
			continue
		}
		qname := e.container + "." + method
		wants[e.id] = want{method: method, strategy: strategy, qname: qname}
		qnameSet[qname] = struct{}{}
	}
	if len(qnameSet) == 0 {
		return 0, nil
	}
	qnames := make([]any, 0, len(qnameSet))
	for q := range qnameSet {
		qnames = append(qnames, q)
	}
	sort.Slice(qnames, func(i, j int) bool { return qnames[i].(string) < qnames[j].(string) })

	// Test-file classification is P7's, computed once from `files` rather than
	// per candidate, and read the same way on both resolver paths so the two
	// cannot disagree. Ruby needs it more than its siblings do: a spec file may
	// reopen a production class and add methods to it, and a production caller
	// must never be bound into one.
	testFiles, err := testFileIDsForRepo(ctx, q, repoID)
	if err != nil {
		return 0, err
	}

	// candidates is keyed by (qualified name, staticness): the two natures are
	// separate methods, so one never makes the other ambiguous. A key holding
	// more than one declaration of the kind a caller may bind is a redefinition
	// whose runtime winner Ruby decides by load order, which is not modelled --
	// count it and fail closed.
	type natureKey struct {
		qname  string
		static int64
	}
	type candidate struct {
		anyID, productionID       int64
		anyCount, productionCount int
	}
	candidates := make(map[natureKey]candidate, len(qnameSet))
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.qualified_name,s.container_name,s.is_static,s.file_id
FROM symbols s JOIN files f ON f.id=s.file_id
WHERE s.repo_id=? AND s.language='ruby' AND s.kind='function' AND s.is_static IS NOT NULL AND f.is_deleted=0
  AND s.qualified_name IN (`,
		"%s)", []any{repoID}, qnames, true,
		func(rows *sql.Rows) error {
			var id, static, fileID int64
			var qname, container string
			if err := rows.Scan(&id, &qname, &container, &static, &fileID); err != nil {
				return err
			}
			// The container must match exactly; a qualified name alone could be
			// a same-named member of a differently nested container.
			if container+"."+qname[strings.LastIndexByte(qname, '.')+1:] != qname {
				return nil
			}
			key := natureKey{qname: qname, static: static}
			c := candidates[key]
			c.anyCount++
			c.anyID = id
			if _, isTest := testFiles[fileID]; !isTest {
				c.productionCount++
				c.productionID = id
			}
			candidates[key] = c
			return nil
		}); err != nil {
		return 0, err
	}

	res := make(map[int64]rubyScopeBinding, len(wants))
	for _, e := range edges {
		w, ok := wants[e.id]
		if !ok {
			continue
		}
		c, found := candidates[natureKey{qname: w.qname, static: e.static}]
		if !found {
			continue
		}
		// P7: a test caller may reach any sole declaration; a production caller
		// may reach only a sole production one.
		dst, count := c.productionID, c.productionCount
		if _, callerIsTest := testFiles[e.fileID]; callerIsTest {
			dst, count = c.anyID, c.anyCount
		}
		if count != 1 {
			continue
		}
		res[e.id] = rubyScopeBinding{dst: dst, strategy: w.strategy}
	}
	if len(res) == 0 {
		return 0, nil
	}
	return rubyScopeApply(ctx, q, res)
}

// rubyScopeApply writes the decided bindings through a temp table so one
// UPDATE covers every edge, whatever the batch size.
func rubyScopeApply(ctx context.Context, q rubyScopeQuery, res map[int64]rubyScopeBinding) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+rubyScopeResolution+`(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+rubyScopeResolution); err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(res))
	for id := range res {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, []any{id, res[id].dst, res[id].strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO `+rubyScopeResolution+`(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	// Both Ruby strategies are registered at the same tier, so the confidence
	// is a constant here; resolutionConfidenceFor keeps that honest.
	confidence := resolutionConfidenceFor(ResolutionStrategyRubyImplicitSelf)
	if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM `+rubyScopeResolution+` r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM `+rubyScopeResolution+` r WHERE r.edge_id=edges.id),resolution_confidence='`+confidence+`' WHERE id IN (SELECT edge_id FROM `+rubyScopeResolution+`)`); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+rubyScopeResolution)
	return len(res), err
}

// resolveRubyScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB: the resolution temp table must see the same
// connection as the statements that fill and read it.
func (s *Store) resolveRubyScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolveRubyScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
