package store

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// Caller/callee neighbourhood queries.
//
// P12 rewrote these from "collect every neighbour id in Go, materialise every
// neighbour symbol, sort the slice, then slice off 20" into a single statement
// that dedupes, orders and pages inside SQLite. The old shape had three
// problems that only a hot hub makes visible:
//
//   - Work was proportional to fan-in, not to the requested page. A symbol with
//     18k callers materialised 18k graph.Symbol values (megabytes, ~80k
//     allocations) to return twenty of them.
//   - The id set was spliced into an `IN (?,?,...)` list, so a hub produced a
//     statement with tens of thousands of bound variables. That is over
//     SQLITE_MAX_VARIABLE_NUMBER on builds that still use the historical 999
//     limit -- a latent failure on the cgo driver, not merely a slow path.
//   - Ordering happened in Go after the fact, so SQLite could never use an
//     index to satisfy it.
//
// The result set is unchanged. The candidate set is the same union (edges bound
// to the target, plus unresolved edges whose `dst_name` names it), duplicates
// are still removed, and the order is the same total order `sortSymbols`
// produced: qualified_name, start_line, start_col, id.
//
// P22.34 closed the half of the second point P12 left open. P12 removed the
// *neighbour* set -- the one that grows with fan-in -- from the bound
// parameters, but the *input* sets (the ids a name lookup matched, the
// unresolved spellings a suffix scan found) were still bound in full, merely
// split across UNION branches. Splitting one 5000-id `IN` list into six bounds
// each expression list and does nothing at all to the statement's total, which
// is the number SQLITE_MAX_VARIABLE_NUMBER is measured against.
//
// The invariant now is per statement, not per expression list: every statement
// this pipeline issues binds at most sqliteInClauseBatchSize arguments, and
// never more than sqliteDefaultMaxVariables. It holds through one transport
// switch rather than two implementations. The candidate CTE is rendered by a
// builder that asks a neighborSets for each dynamic value set. Rendered inline,
// a set is a placeholder list and its values are bound -- exactly the statement
// the pre-P22.34 code issued, for the small queries that are almost all of
// them. When that statement's own argument count does not fit the budget, the
// same builder is called again against a connection-pinned staging table: each
// set is inserted in bounded batches and rendered as a scalar subquery, so the
// page statement's arguments no longer track the input size at all.
//
// What deliberately did not change: the candidate set is still computed,
// deduped, ordered and paged by one statement inside SQLite. Staging moves the
// *inputs* out of the parameter list, never the candidates into Go. Paging per
// input batch would be a different answer -- a global ORDER BY over the union
// is not the concatenation of per-batch orderings -- and materialising the
// candidates in Go would restore the cost P12 removed.

// symbolPageFixedArgs is what symbolPageSQL binds beyond its candidate CTE:
// the repository id, the LIMIT and the OFFSET.
const symbolPageFixedArgs = 3

// neighborKeysTable stages the value sets a neighbour candidate query matches
// against, when binding them would exceed the statement's variable budget.
//
// It is a TEMP table, so it is private to one SQLite connection and to the
// lifetime of that connection: concurrent FindCallers/FindCallees calls land on
// different pooled connections and cannot see each other's rows, and nothing
// here touches the persistent graph. The fixed name is safe for that same
// reason, provided every statement of one call runs on the connection that
// staged it -- see neighborPage, which pins one *sql.Conn for the whole
// operation.
//
// `val` is deliberately typeless: ids are stored as integers and spellings as
// text, and SQLite compares each against the column it is matched to. The
// primary key makes each set a set, which is what the UNION used to provide.
const neighborKeysTable = "temp.neighbor_query_keys"

const createNeighborKeysSQL = `CREATE TABLE IF NOT EXISTS ` + neighborKeysTable + `(
	kind INTEGER NOT NULL,
	val NOT NULL,
	PRIMARY KEY(kind, val)
) WITHOUT ROWID`

const clearNeighborKeysSQL = `DELETE FROM ` + neighborKeysTable

const insertNeighborKeysSQL = `INSERT OR IGNORE INTO ` + neighborKeysTable + `(kind, val) VALUES `

// neighborTarget applies the test-only statement wrapper, if one is installed.
func (s *Store) neighborTarget(t execQuerier) execQuerier {
	if s.neighborStatementWrap != nil {
		return s.neighborStatementWrap(t)
	}
	return t
}

// errNeighborEmptySet reports a value set rendered with no members. Every leg
// here tests for evidence before rendering it, so this is a programming error.
var errNeighborEmptySet = errors.New("store: neighbour value set is empty")

// neighborSets renders the dynamic value sets a candidate query matches
// against, in whichever of the two transports the page statement can afford.
//
// Inline is the default and the fast path: the set becomes a placeholder list
// and its values join the page statement's arguments. Staged is the fallback:
// the set is inserted into neighborKeysTable in bounded batches and becomes a
// subquery that binds nothing.
type neighborSets struct {
	target execQuerier
	staged bool
	kinds  int
}

// ref renders `values` as SQL usable directly after `IN`, together with the
// arguments the rendering binds -- none, when staged. Callers must not pass an
// empty set: `IN ()` is not valid SQL, and every leg here already tests for
// evidence before rendering it.
func (n *neighborSets) ref(ctx context.Context, values []any) (string, []any, error) {
	if len(values) == 0 {
		// Enforced rather than documented, because the two transports would
		// disagree about it: inline would render invalid SQL and staged a
		// valid always-false subquery.
		return "", nil, errNeighborEmptySet
	}
	if !n.staged {
		return "(" + placeholders(len(values)) + ")", values, nil
	}
	kind := n.kinds
	n.kinds++
	rows := make([][]any, 0, len(values))
	for _, value := range values {
		rows = append(rows, []any{kind, value})
	}
	if err := sqliteBatchedValuesExec(ctx, n.target, insertNeighborKeysSQL, "(?,?)", nil, rows); err != nil {
		return "", nil, err
	}
	// The kind is generated here and rendered as a literal rather than bound,
	// so a staged set costs the page statement exactly zero parameters.
	return "(SELECT val FROM " + neighborKeysTable + " WHERE kind = " + strconv.Itoa(kind) + ")", nil, nil
}

func (n *neighborSets) refInt64s(ctx context.Context, ids []int64) (string, []any, error) {
	return n.ref(ctx, int64SliceToAny(ids))
}

func (n *neighborSets) refStrings(ctx context.Context, values []string) (string, []any, error) {
	return n.ref(ctx, stringSliceToAny(values))
}

// neighborCandidateBuilder renders one query's candidate CTE and the arguments
// it binds. It is called once per transport attempt, so it must derive
// everything it needs from evidence its caller already gathered: the only
// database work it may do is the staging neighborSets performs.
//
// An empty CTE means "no evidence at all", which is an empty page.
type neighborCandidateBuilder func(ctx context.Context, sets *neighborSets) (string, []any, error)

// symbolPageSQL renders the page query over a candidate-id CTE. The CTE is
// supplied by the caller because callers and callees differ only in how the
// candidate ids are derived.
func symbolPageSQL(candidateCTE string) string {
	return `
		WITH candidates(id) AS (` + candidateCTE + `)
		SELECT s.id, s.file_id, s.language, s.kind, s.name, s.qualified_name, s.container_name, s.signature, s.visibility,
		       s.start_line, s.start_col, s.end_line, s.end_col, s.doc_summary, s.stable_key, f.path
		-- CROSS JOIN pins the candidate set as the outer loop. Left to itself
		-- SQLite sometimes drove this from the symbols table and re-evaluated the
		-- candidate co-routine once per symbol row: on a 988-symbol repository
		-- that turned a 42-candidate page into ~300ms, and the cost grows with
		-- the size of the repository rather than with the size of the answer.
		-- Driving from the candidates is always the right shape here, because
		-- the join is an integer-primary-key seek and the candidate set is by
		-- construction a subset of the symbols.
		FROM candidates c
		CROSS JOIN symbols s ON s.id = c.id
		JOIN files f ON f.id = s.file_id
		WHERE s.repo_id = ?
		ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
		LIMIT ?
		OFFSET ?
	`
}

// edgeIDBranch renders the `SELECT ... WHERE <matchCol> IN <set>` leg that
// turns an input id set into candidate ids.
//
// DISTINCT matters even though the surrounding UNION dedupes across legs: when
// this is the only leg the caller has (every other evidence leg came up empty),
// no UNION runs, and two edges to the same destination would otherwise surface
// the destination twice.
func edgeIDBranch(selectCol, matchCol, setSQL, extra string) string {
	sql := "SELECT DISTINCT e." + selectCol +
		" FROM edges e WHERE e.repo_id = ? AND e." + matchCol + " IN " + setSQL
	if extra != "" {
		sql += " AND " + extra
	}
	return sql
}

// neighborPage renders a candidate CTE and returns one page of the symbols it
// names, choosing the transport that keeps the page statement inside the shared
// variable budget.
//
// The inline attempt comes first and is measured, not guessed: the builder
// reports exactly what it would bind, and only a total -- inputs plus
// symbolPageSQL's own three arguments -- that does not fit falls back. The
// fallback re-renders the same CTE against a staging table on one pinned
// connection, so the two transports cannot disagree about semantics: they
// differ only in how a value set is spelled.
func (s *Store) neighborPage(ctx context.Context, repoID int64, limit, offset int, build neighborCandidateBuilder) ([]graph.Symbol, error) {
	inline := &neighborSets{target: s.neighborTarget(s.db)}
	cte, args, err := build(ctx, inline)
	if err != nil {
		return nil, err
	}
	if cte == "" {
		return []graph.Symbol{}, nil
	}
	if len(args)+symbolPageFixedArgs <= sqliteInClauseBatchSize {
		return s.symbolPage(ctx, inline.target, repoID, cte, args, limit, offset)
	}

	// Everything below -- the staging inserts and the page SELECT that reads
	// them -- must run on one physical connection, because a TEMP table belongs
	// to the connection that created it. Taking it from the pool per statement
	// would silently query an empty table on a different connection.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	target := s.neighborTarget(conn)
	if _, err := target.ExecContext(ctx, createNeighborKeysSQL); err != nil {
		return nil, err
	}
	// Clearing before use is the load-bearing half, not the cleanup below: a
	// pooled connection may carry rows an earlier call left behind when it
	// failed or its context was cancelled between staging and paging.
	if _, err := target.ExecContext(ctx, clearNeighborKeysSQL); err != nil {
		return nil, err
	}
	defer func() {
		// Best effort, and detached from ctx so a cancelled query still hands
		// back a clean connection. Correctness does not depend on it.
		_, _ = target.ExecContext(context.WithoutCancel(ctx), clearNeighborKeysSQL)
	}()

	staged := &neighborSets{target: target, staged: true}
	cte, args, err = build(ctx, staged)
	if err != nil {
		return nil, err
	}
	if cte == "" {
		return []graph.Symbol{}, nil
	}
	return s.symbolPage(ctx, target, repoID, cte, args, limit, offset)
}

func (s *Store) symbolPage(ctx context.Context, target execQuerier, repoID int64, candidateCTE string, cteArgs []any, limit, offset int) ([]graph.Symbol, error) {
	args := make([]any, 0, len(cteArgs)+symbolPageFixedArgs)
	args = append(args, cteArgs...)
	args = append(args, repoID, safeLimit(limit), safeOffset(offset))
	rows, err := target.QueryContext(ctx, symbolPageSQL(candidateCTE), args...)
	if err != nil {
		return nil, err
	}
	out, err := scanSymbols(rows)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return []graph.Symbol{}, nil
	}
	return out, nil
}

// FindCallers returns the symbols that call the named symbol, ordered by
// qualified_name, start_line, start_col, id.
//
// For a known target, only edges whose `dst_symbol_id` is bound to that target
// are relationships. If no indexed target matches, unresolved `dst_name`
// spellings are returned as discovery hints.
//
// The two legs answer different questions, and P22.7 gates only the second. A
// bound `dst_symbol_id` is a decision something already made on evidence --
// including ResolveCrossLanguageLinks' explicit `cross_language_ref` links,
// which are foreign-language on purpose and must keep surfacing. An unresolved
// `dst_name` is a spelling, and a spelling written in another language does not
// name this symbol, exactly as the resolver refuses to bind it.
//
// "Another language" is measured against the languages of the symbols the input
// actually matched, as a set. This query has always answered for every symbol a
// name matches, so an ambiguous name that names a Go symbol and a Python one
// keeps returning both languages' callers -- each of them a real caller of one
// of the matched targets. The gate is at its tightest when the input identifies
// one symbol, which is the form `TestExactSymbolIDStaysExact` pins.
//
// The bare-name leg's scope predicate (P22.6/P22.13) is target-derived in the
// same way, and P22.14 draws the same line under it: an input that matched no
// symbol has no package or file to scope against, so the predicate is omitted
// rather than asked for zero scopes, and the unresolved writers stay as name
// evidence. An input that DID match keeps the predicate, including when its
// targets yield no scope at all -- that is a refusal, not an absence.
func (s *Store) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	var targetIDs []int64
	var err error
	if symbolID != 0 {
		identity, ok, lookupErr := s.lookupSymbolIdentity(ctx, repoID, symbolID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if !ok {
			return []graph.Symbol{}, nil
		}
		// An explicit id is authoritative. All later name/suffix evidence is
		// derived from this persisted identity, never from caller spelling.
		symbol = identity.QualifiedName
		targetIDs = []int64{identity.ID}
	} else {
		targetIDs, err = s.lookupSymbolIDs(ctx, repoID, symbol, 0)
		if err != nil {
			return nil, err
		}
	}
	return s.findCallersResolved(ctx, repoID, symbol, targetIDs, limit, offset)
}

// callerNameEvidence is the unresolved-spelling evidence FindCallers gathers
// when no indexed target matched: the value sets its legs match against, and
// nothing about how they are spelled into SQL.
//
// Gathering is separated from rendering because the renderer runs twice when
// the inline transport does not fit, and every database read must happen
// exactly once regardless of which transport answers.
type callerNameEvidence struct {
	qualifiedExact []string
	bareExact      []string
	scopeKeys      []string
	scopeKeyed     bool
	bareLangs      []string
	spellings      []string
	targetLangs    []string
}

func (e callerNameEvidence) empty() bool {
	return len(e.qualifiedExact) == 0 && len(e.bareExact) == 0 && len(e.spellings) == 0
}

func (s *Store) findCallersResolved(ctx context.Context, repoID int64, symbol string, targetIDs []int64, limit, offset int) ([]graph.Symbol, error) {
	short := lookupSymbolShortName(strings.TrimSpace(strings.TrimPrefix(symbol, "::")))

	var evidence callerNameEvidence
	// An unresolved spelling is a hint only when no indexed target exists.
	// Once a target is known, only its persisted edge identity is authoritative.
	if len(targetIDs) == 0 && short != "" {
		var err error
		if evidence, err = s.callerNameEvidence(ctx, repoID, symbol, short, targetIDs); err != nil {
			return nil, err
		}
	}
	if len(targetIDs) == 0 && evidence.empty() {
		return []graph.Symbol{}, nil
	}

	return s.neighborPage(ctx, repoID, limit, offset, func(ctx context.Context, sets *neighborSets) (string, []any, error) {
		var branches []string
		var args []any
		if len(targetIDs) > 0 {
			set, setArgs, err := sets.refInt64s(ctx, targetIDs)
			if err != nil {
				return "", nil, err
			}
			branches = append(branches, edgeIDBranch("src_symbol_id", "dst_symbol_id", set, ""))
			args = append(args, repoID)
			args = append(args, setArgs...)
		}
		if !evidence.empty() {
			nameSQL, nameArgs, err := evidence.render(ctx, repoID, sets)
			if err != nil {
				return "", nil, err
			}
			branches = append(branches, nameSQL)
			args = append(args, nameArgs...)
		}
		if len(branches) == 0 {
			return "", nil, nil
		}
		return strings.Join(branches, " UNION "), args, nil
	})
}

// callerNameEvidence gathers the unknown-target evidence legs. Every rule it
// applies is a semantic one; the transport question is settled later, by
// render.
func (s *Store) callerNameEvidence(ctx context.Context, repoID int64, symbol, short string, targetIDs []int64) (callerNameEvidence, error) {
	var out callerNameEvidence

	// One read of the targets answers both name-evidence rules: their
	// persisted languages (P22.7) and, for the Go ones, the package scopes a
	// bare spelling could name them from (P22.6).
	//
	// No target row means no language evidence at all -- the name is not in
	// the index, and the honest answer to "who spells this" is still every
	// writer. Inventing a language there would be the guess this phase
	// removes, in the other direction.
	targetScopes, err := symbolScopesByIDs(ctx, neighborQuerier{s}, repoID, targetIDs)
	if err != nil {
		return out, err
	}
	out.targetLangs = symbolLanguagesOf(targetScopes)
	// The name legs are collected separately from the bound-destination leg
	// so the language gate can be applied once to their union, rather than
	// once per branch. The writer lookup is an integer-primary-key seek, and
	// the union already has to be materialised, so paying for it once keeps
	// the gate off the per-leg path entirely.
	//
	// Split rather than one many-way OR. A single OR-of-predicates is not
	// seekable, so SQLite fell back to walking every edge in the repo
	// through idx_edges_repo_src and fetching each row. Separated, the
	// equality half seeks idx_edges_repo_unresolved_name_src directly and
	// the LIKE half is confined to the unresolved population by the
	// `dst_symbol_id IS NULL` equality. UNION makes the halves one set,
	// so the candidate set is identical to the combined predicate's.
	//
	// The exact spellings are split once more, by whether they are bare
	// (P22.6). A qualified spelling names this target wherever it is
	// written; a bare one only does so from inside the target's own Go
	// package, so its leg carries the package-scope predicate. Without the
	// split, a bare `countTags` unresolved in package `profile` claimed
	// `graph.countTags` -- the same fabrication the resolver now refuses.
	//
	// P22.9 narrows the bare half by what the input matched: in a language
	// whose visibility this rule decides, a bare spelling may not claim a
	// type. Every relationship of that shape the evidence supports is
	// already a bound `dst_symbol_id` and arrives through the id leg above,
	// so what this leg would still add is exactly the population the
	// resolver refused -- `pathlib.Path` claiming a project `class Path`
	// among it.
	//
	// The narrowing is per LANGUAGE, not over the whole match set: an input
	// matching a Python class and a Kotlin class must lose the Python
	// writers and keep the Kotlin ones, because Kotlin resolves a bare class
	// name across files with no import at all. When every matched language
	// is type-only the leg has nobody left to serve and is dropped.
	// See resolver_type_scope.go.
	out.bareLangs = languagesExcept(out.targetLangs, typeOnlyGatedLanguages(targetScopes))
	seenExact := map[string]bool{}
	for _, spelling := range []string{symbol, short} {
		if spelling == "" || seenExact[spelling] {
			continue
		}
		seenExact[spelling] = true
		if goBareCallName(spelling) {
			if len(out.targetLangs) > 0 && len(out.bareLangs) == 0 {
				continue
			}
			out.bareExact = append(out.bareExact, spelling)
		} else {
			out.qualifiedExact = append(out.qualifiedExact, spelling)
		}
	}
	// P22.14: the scope predicate is target-derived evidence, so it only
	// exists when a target does. Zero scope keys has two causes and they are
	// opposite answers: a matched target whose own rules say no bare spelling
	// reaches it (a Go method, a C/C++ symbol with no file) is a refusal and
	// keeps the predicate, which then admits only writers in ungated
	// languages; no matched target at all is an absence of evidence, and
	// gating on it deleted precisely the unresolved Go and C/C++ writers this
	// leg exists to surface. The same reasoning already governs `bareLangs`
	// above.
	//
	// The test is `targetIDs`, not the rows they loaded: an explicit
	// `symbol_id` is an assertion of identity, and a stale one whose row is
	// gone must stay refused rather than fail open into the unknown-name
	// contract. Only a lookup that matched nothing at all is an absence of
	// evidence.
	out.scopeKeyed = len(targetIDs) > 0
	if out.scopeKeyed {
		out.scopeKeys = goBareTargetScopes(targetScopes)
	}

	// Suffix evidence (P22.1): an unresolved spelling claims this target
	// only when one of the two extends the other at a separator boundary,
	// so the qualifier stays part of the match. The pre-P22.1 legs matched
	// `%.` + bare short, which let `rows.Close` claim every project Close.
	//
	// Direction one: the spelling is a qualified suffix of the target's
	// identity (`App.Close` for cli.App.Close) -- a finite, indexed IN set
	// built from the input and the resolved targets' qualified names.
	// Direction two: the spelling extends an identity at a '.', '::' or
	// '/' boundary (`x.cli.App.Close`, `path/to/pkg.Func`). One scan of
	// the distinct unresolved destination names decides direction two for
	// every identity at once -- the same shape context expansion uses --
	// so neither the statement's compound-SELECT terms nor its bound
	// variables grow with how many symbols share the input's bare name.
	qnames := []string{symbol}
	targetQNames, err := s.qualifiedNamesByIDs(ctx, repoID, targetIDs)
	if err != nil {
		return out, err
	}
	qnames = append(qnames, targetQNames...)
	seen := map[string]bool{symbol: true, short: true}
	for _, qname := range qnames {
		for _, spelling := range boundaryProperSuffixes(qname) {
			if !seen[spelling] {
				seen[spelling] = true
				out.spellings = append(out.spellings, spelling)
			}
		}
	}
	// Direction two seeds only on qualifier-bearing identities. A bare seed
	// is extended by every receiver spelling that ends in it, which is the
	// bare-tail match P22.1 retired -- `pcsApmMap.LoadWorldMap` extends
	// `LoadWorldMap` and would name `AgcmMinimap::F` a caller of
	// `ApmMap::LoadWorldMap`, on nothing but the tail. Direction one already
	// applies the same rule (boundaryProperSuffixes drops the bare tail);
	// this is the other half of it. `qnames` still carries the bare input
	// for direction one and for the exact legs above, where an equal
	// spelling is evidence rather than a suffix of one.
	extendSeeds := make([]string, 0, len(qnames))
	for _, qname := range qnames {
		if memberSeparated(qname) {
			extendSeeds = append(extendSeeds, qname)
		}
	}
	extending, err := s.unresolvedDstNamesExtending(ctx, repoID, extendSeeds)
	if err != nil {
		return out, err
	}
	for _, name := range extending {
		if !seen[name] {
			seen[name] = true
			out.spellings = append(out.spellings, name)
		}
	}
	return out, nil
}

// render spells the gathered evidence into one UNIONed candidate leg.
//
// The legs, their predicates and their union are exactly what they were before
// P22.34; only the value sets change shape, and only when the inline transport
// cannot afford them.
func (e callerNameEvidence) render(ctx context.Context, repoID int64, sets *neighborSets) (string, []any, error) {
	var branches []string
	var args []any

	if len(e.qualifiedExact) > 0 {
		set, setArgs, err := sets.refStrings(ctx, e.qualifiedExact)
		if err != nil {
			return "", nil, err
		}
		branches = append(branches, `SELECT e.src_symbol_id AS src FROM edges e
				WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
				  AND e.dst_name IN `+set)
		args = append(args, repoID)
		args = append(args, setArgs...)
	}

	if len(e.bareExact) > 0 {
		set, setArgs, err := sets.refStrings(ctx, e.bareExact)
		if err != nil {
			return "", nil, err
		}
		// `scopeKeys` is empty whenever the predicate is omitted, so the bound
		// arguments stay in step with the rendered statement.
		bareScopeFilter := ""
		var scopeArgs []any
		if e.scopeKeyed {
			scopeSet := ""
			if len(e.scopeKeys) > 0 {
				var err error
				if scopeSet, scopeArgs, err = sets.refStrings(ctx, e.scopeKeys); err != nil {
					return "", nil, err
				}
			}
			bareScopeFilter = ` AND ` + sqlGoBareSourceScope(scopeSet)
		}
		// `bareLangs` rather than the union's `targetLangs`: this leg alone
		// drops the languages whose matched targets are all types (P22.9).
		// The outer language gate below still applies and is a superset.
		bareLangFilter := ""
		var langArgs []any
		if len(e.bareLangs) > 0 {
			langSet, a, err := sets.refStrings(ctx, e.bareLangs)
			if err != nil {
				return "", nil, err
			}
			bareLangFilter = ` AND src.language IN ` + langSet
			langArgs = a
		}
		branches = append(branches, `SELECT e.src_symbol_id AS src FROM edges e
				JOIN symbols src ON src.id = e.src_symbol_id
				JOIN files srcf ON srcf.id = src.file_id
				WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
				  AND e.dst_name IN `+set+`
				  `+bareScopeFilter+bareLangFilter)
		args = append(args, repoID)
		args = append(args, setArgs...)
		args = append(args, scopeArgs...)
		args = append(args, langArgs...)
	}

	if len(e.spellings) > 0 {
		set, setArgs, err := sets.refStrings(ctx, e.spellings)
		if err != nil {
			return "", nil, err
		}
		branches = append(branches, `SELECT e.src_symbol_id AS src FROM edges e
				WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
				  AND e.dst_name IN `+set)
		args = append(args, repoID)
		args = append(args, setArgs...)
	}

	if len(branches) == 0 {
		return "", nil, nil
	}
	nameSQL := strings.Join(branches, " UNION ")
	if len(e.targetLangs) > 0 {
		// P22.7: only a writer in one of the target's own languages may
		// claim it by name. With no target row there is no language to
		// require, and the union stands as it did.
		langSet, langArgs, err := sets.refStrings(ctx, e.targetLangs)
		if err != nil {
			return "", nil, err
		}
		nameSQL = `SELECT nl.src FROM (` + nameSQL + `) nl
					JOIN symbols srclang ON srclang.id = nl.src
					WHERE srclang.language IN ` + langSet
		args = append(args, langArgs...)
	}
	return nameSQL, args, nil
}

// qualifiedNamesByIDs returns the distinct qualified names of the given
// symbols, sorted, so downstream statement text is a function of the graph
// rather than of how the ids were batched.
//
// The ids are read one bounded batch per statement, so SQLite can only order
// within a batch; the sort is applied once to the union, which is the
// guarantee the single pre-P22.34 statement's ORDER BY gave.
func (s *Store) qualifiedNamesByIDs(ctx context.Context, repoID int64, ids []int64) ([]string, error) {
	var out []string
	err := sqliteBatchedIDQuery(ctx, s.db, ids, `
		SELECT DISTINCT qualified_name FROM symbols
		WHERE repo_id = ? AND id IN (`, []any{repoID},
		func(scan func(...any) error) error {
			var qname string
			if err := scan(&qname); err != nil {
				return err
			}
			out = append(out, qname)
			return nil
		})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// unresolvedDstNamesExtending returns the distinct unresolved destination
// names that extend any of the given identities at a '.', "::" or '/'
// boundary, ASCII-case-insensitively, in scan order. One scan serves every
// identity; each row is folded once.
func (s *Store) unresolvedDstNamesExtending(ctx context.Context, repoID int64, qnames []string) ([]string, error) {
	folded := make([]string, 0, len(qnames))
	seen := map[string]bool{}
	for _, qname := range qnames {
		if f := asciiLower(qname); f != "" && !seen[f] {
			seen[f] = true
			folded = append(folded, f)
		}
	}
	if len(folded) == 0 {
		return nil, nil
	}
	rows, err := s.neighborQuery(ctx, `
		SELECT DISTINCT e.dst_name
		FROM edges e
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name != ''
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		d := asciiLower(name)
		for _, q := range folded {
			if prefoldedExtendsAtBoundary(d, q) {
				out = append(out, name)
				break
			}
		}
	}
	return out, rows.Err()
}

// FindCallees returns the symbols the named symbol calls. Only persisted
// destination identities are semantic relationships; unresolved destinations
// remain evidence and are not promoted by query-time name lookup.
func (s *Store) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	srcIDs, err := s.lookupSymbolIDs(ctx, repoID, symbol, symbolID)
	if err != nil {
		return nil, err
	}
	return s.findCalleesResolved(ctx, repoID, srcIDs, limit, offset)
}

func (s *Store) findCalleesResolved(ctx context.Context, repoID int64, srcIDs []int64, limit, offset int) ([]graph.Symbol, error) {
	if len(srcIDs) == 0 {
		return []graph.Symbol{}, nil
	}
	return s.neighborPage(ctx, repoID, limit, offset, func(ctx context.Context, sets *neighborSets) (string, []any, error) {
		set, setArgs, err := sets.refInt64s(ctx, srcIDs)
		if err != nil {
			return "", nil, err
		}
		args := append([]any{repoID}, setArgs...)
		return edgeIDBranch("dst_symbol_id", "src_symbol_id", set, "e.dst_symbol_id IS NOT NULL"), args, nil
	})
}
