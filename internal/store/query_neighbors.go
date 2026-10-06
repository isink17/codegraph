package store

import (
	"context"
	"errors"
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
// produced, now ordered by path, qualified_name, kind, span, stable_key.
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
		-- Candidate ids come straight off edges, which outlive the file they
		-- describe: a neighbour in a soft-deleted file is not user-visible, and
		-- the exclusion is here rather than after the page so a ghost cannot
		-- consume a LIMIT/OFFSET slot.
		JOIN files f ON f.id = s.file_id AND f.is_deleted = 0
		WHERE s.repo_id = ?
		ORDER BY f.path ASC, s.qualified_name ASC, s.kind ASC, s.start_line ASC, s.start_col ASC,
		         s.end_line ASC, s.end_col ASC, s.stable_key ASC
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
	inline := &neighborSets{target: s.neighborTarget(s.parserSemanticQueryer(ctx))}
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
	var target execQuerier
	if q, ok := ctx.Value(parserSemanticGraphQueryerKey{}).(parserSemanticGraphQueryer); ok {
		target = s.neighborTarget(q)
	} else {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		target = s.neighborTarget(conn)
	}
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

// FindCallers returns symbols that call the named symbol, ordered by path,
// qualified name, kind, source span, and stable key.
//
// It has exactly two query modes, chosen by whether the input identifies an
// indexed symbol:
//
//   - persisted relationship: a known target (by name, or by an explicit id) is
//     answered only by edges whose `dst_symbol_id` is bound to it. That binding
//     is a decision the resolver already made on evidence -- including
//     ResolveCrossLanguageLinks' explicit `cross_language_ref` links -- and
//     nothing here re-derives or widens it. A stale explicit id answers empty
//     rather than falling back to spelling.
//   - unknown-target spelling hint: an input that matched no symbol is answered
//     by writers of unresolved edges whose `dst_name` names it (see
//     unknownTargetHints). No target exists, so no target language, package,
//     file or class is invented to scope those hints.
func (s *Store) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		defer tx.Rollback()
	}
	var targetIDs []int64
	if symbolID != 0 {
		identity, ok, lookupErr := s.lookupSymbolIdentity(ctx, repoID, symbolID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if !ok {
			return []graph.Symbol{}, nil
		}
		// An explicit id is authoritative: only edges bound to it answer.
		symbol = identity.QualifiedName
		targetIDs = []int64{identity.ID}
	} else {
		targetIDs, err = s.lookupQuerySymbolIDs(ctx, repoID, symbol, 0)
		if err != nil {
			return nil, err
		}
	}
	return s.findCallersResolved(ctx, repoID, symbol, targetIDs, limit, offset)
}

// unknownTargetHints is FindCallers' second query mode. The first mode follows
// persisted relationships: once a target identity is known (by name or by an
// explicit id, stale or not), only edges bound to it answer. This mode runs
// only when no indexed symbol matched the input, and it answers with writers of
// unresolved edges whose destination spelling names the input. Those writers
// are discovery hints, not callers: no target exists, so there is no target
// language, package, file or class to scope them by, and none is invented.
//
// Gathering is separated from rendering because the renderer runs twice when
// the inline transport does not fit, and every database read must happen
// exactly once regardless of which transport answers.
type unknownTargetHints struct {
	// spellings are the exact input spellings followed by the
	// boundary-suffix spellings, deduplicated.
	spellings []string
}

func (s *Store) findCallersResolved(ctx context.Context, repoID int64, symbol string, targetIDs []int64, limit, offset int) ([]graph.Symbol, error) {
	short := lookupSymbolShortName(strings.TrimSpace(strings.TrimPrefix(symbol, "::")))

	var hints unknownTargetHints
	// An unresolved spelling is a hint only when no indexed target exists.
	// Once a target is known, only its persisted edge identity is authoritative.
	if len(targetIDs) == 0 && short != "" {
		var err error
		if hints, err = s.unknownTargetSpellingHints(ctx, repoID, symbol, short); err != nil {
			return nil, err
		}
	}
	if len(targetIDs) == 0 && len(hints.spellings) == 0 {
		return []graph.Symbol{}, nil
	}

	return s.neighborPage(ctx, repoID, limit, offset, func(ctx context.Context, sets *neighborSets) (string, []any, error) {
		if len(targetIDs) > 0 {
			set, setArgs, err := sets.refInt64s(ctx, targetIDs)
			if err != nil {
				return "", nil, err
			}
			return edgeIDBranch("src_symbol_id", "dst_symbol_id", set, ""), append([]any{repoID}, setArgs...), nil
		}
		return hints.render(ctx, repoID, sets)
	})
}

// unknownTargetSpellingHints gathers the spellings that name an input no
// indexed symbol matched. Every rule it applies is a semantic one; the
// transport question is settled later, by render.
func (s *Store) unknownTargetSpellingHints(ctx context.Context, repoID int64, symbol, short string) (unknownTargetHints, error) {
	var out unknownTargetHints
	seen := map[string]bool{}
	// Exact spellings: the input as written and its short name.
	for _, spelling := range []string{symbol, short} {
		if spelling != "" && !seen[spelling] {
			seen[spelling] = true
			out.spellings = append(out.spellings, spelling)
		}
	}

	// Suffix evidence (P22.1): an unresolved spelling claims this input only
	// when one of the two extends the other at a separator boundary, so the
	// qualifier stays part of the match. The pre-P22.1 legs matched `%.` +
	// bare short, which let `rows.Close` claim every project Close.
	//
	// Direction one: the spelling is a qualified suffix of the input
	// (`App.Close` for cli.App.Close) -- a finite, indexed IN set. Direction
	// two: the spelling extends the input at a '.', '::' or '/' boundary
	// (`x.cli.App.Close`, `path/to/pkg.Func`). One scan of the distinct
	// unresolved destination names decides direction two -- the same shape
	// context expansion uses.
	for _, spelling := range boundaryProperSuffixes(symbol) {
		if !seen[spelling] {
			seen[spelling] = true
			out.spellings = append(out.spellings, spelling)
		}
	}
	// Direction two seeds only on a qualifier-bearing input. A bare seed is
	// extended by every receiver spelling that ends in it, which is the
	// bare-tail match P22.1 retired -- `pcsApmMap.LoadWorldMap` extends
	// `LoadWorldMap` on nothing but the tail. Direction one already applies
	// the same rule (boundaryProperSuffixes drops the bare tail).
	var extendSeeds []string
	if memberSeparated(symbol) {
		extendSeeds = []string{symbol}
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

// render spells the gathered hints into one candidate leg over the unresolved
// edges. A single equality IN set seeks idx_edges_repo_unresolved_name_src.
func (h unknownTargetHints) render(ctx context.Context, repoID int64, sets *neighborSets) (string, []any, error) {
	if len(h.spellings) == 0 {
		return "", nil, nil
	}
	set, setArgs, err := sets.refStrings(ctx, h.spellings)
	if err != nil {
		return "", nil, err
	}
	return `SELECT DISTINCT e.src_symbol_id FROM edges e
				WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
				  AND e.dst_name IN ` + set, append([]any{repoID}, setArgs...), nil
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
	ctx, tx, err := s.beginParserSemanticGraphRead(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		defer tx.Rollback()
	}
	srcIDs, err := s.lookupQuerySymbolIDs(ctx, repoID, symbol, symbolID)
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
