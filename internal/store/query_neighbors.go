package store

import (
	"context"
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

// neighborIDChunk bounds how many symbol ids go into one `IN` or `VALUES`
// branch.
//
// Be precise about what this does and does not buy. All branches end up in one
// statement, so the statement's total bound-variable count still tracks the
// total id count -- splitting one 5000-id `IN` list into six does not change
// that, and the driver's ceiling (32766 on the current build) still applies to
// the sum. What chunking bounds is the size of a single expression list, and
// the chunk size trades against SQLITE_MAX_COMPOUND_SELECT (default 500): at
// 900 ids per branch that ceiling sits around 450k ids, comfortably past the
// variable ceiling, so the variable limit is the binding one.
//
// The variable-count win of the P12 rewrite is elsewhere: the *neighbour* set
// -- the one that grows with fan-in, and reached 18k for the hub in the
// benchmark fixture -- is no longer bound into a statement at all. The id sets
// still bound here are the ones a name lookup produced, which the pre-P12 code
// bound too.
const neighborIDChunk = 900

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

// edgeIDBranches renders one `SELECT ... WHERE <col> IN (...)` branch per id
// chunk, UNIONed together. UNION (not UNION ALL) so the CTE is already a set.
//
// DISTINCT inside each branch matters even though UNION dedupes across
// branches: when this is the only branch the caller has (every other evidence
// leg came up empty), no UNION runs, and two edges to the same destination
// would otherwise surface the destination twice.
func edgeIDBranches(repoID int64, ids []int64, selectCol, matchCol, extra string) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(ids)+len(ids)/neighborIDChunk+1)
	for i, chunk := range chunkInt64s(ids, neighborIDChunk) {
		if i > 0 {
			sb.WriteString(" UNION ")
		}
		sb.WriteString("SELECT DISTINCT e.")
		sb.WriteString(selectCol)
		sb.WriteString(" FROM edges e WHERE e.repo_id = ? AND e.")
		sb.WriteString(matchCol)
		sb.WriteString(" IN (")
		sb.WriteString(strings.TrimRight(strings.Repeat("?,", len(chunk)), ","))
		sb.WriteString(")")
		if extra != "" {
			sb.WriteString(" AND ")
			sb.WriteString(extra)
		}
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
	}
	return sb.String(), args
}

// valueBranches renders a literal id set as a single UNIONable branch.
//
// A `VALUES` list rather than N chained `SELECT ?` terms: the chained form
// makes the number of compound SELECTs grow with the number of ids, and SQLite
// pays to plan every one of them. On a real repository a symbol with a few
// hundred unresolved destinations turned that into a several-hundred-way UNION
// and ~290ms of query planning for a twenty-row page.
func valueBranches(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	var sb strings.Builder
	sb.WriteString("SELECT column1 FROM (VALUES ")
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?)")
		args = append(args, id)
	}
	sb.WriteString(")")
	return sb.String(), args
}

func (s *Store) symbolPage(ctx context.Context, repoID int64, candidateCTE string, cteArgs []any, limit, offset int) ([]graph.Symbol, error) {
	args := make([]any, 0, len(cteArgs)+3)
	args = append(args, cteArgs...)
	args = append(args, repoID, safeLimit(limit), safeOffset(offset))
	rows, err := s.db.QueryContext(ctx, symbolPageSQL(candidateCTE), args...)
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
// Two evidence sources are unioned, exactly as before P12: edges whose
// `dst_symbol_id` is bound to the target, and unresolved edges whose `dst_name`
// spells it. The second leg is what surfaces callers the resolver could not
// bind; dropping it would silently shrink the answer.
func (s *Store) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	targetIDs, err := s.lookupSymbolIDs(ctx, repoID, symbol, symbolID)
	if err != nil {
		return nil, err
	}
	short := lookupSymbolShortName(strings.TrimSpace(strings.TrimPrefix(symbol, "::")))

	var branches []string
	var args []any
	if len(targetIDs) > 0 {
		sql, a := edgeIDBranches(repoID, targetIDs, "src_symbol_id", "dst_symbol_id", "")
		branches = append(branches, sql)
		args = append(args, a...)
	}
	if short != "" {
		// Split rather than one many-way OR. A single OR-of-predicates is not
		// seekable, so SQLite fell back to walking every edge in the repo
		// through idx_edges_repo_src and fetching each row. Separated, the
		// equality half seeks idx_edges_repo_unresolved_name_src directly and
		// the LIKE half is confined to the unresolved population by the
		// `dst_symbol_id IS NULL` equality. UNION makes the halves one set,
		// so the candidate set is identical to the combined predicate's.
		branches = append(branches, `SELECT e.src_symbol_id FROM edges e
			WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name IN (?, ?)`)
		args = append(args, repoID, symbol, short)

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
		if targetQNames, err := s.qualifiedNamesByIDs(ctx, repoID, targetIDs); err != nil {
			return nil, err
		} else {
			qnames = append(qnames, targetQNames...)
		}
		spellings, seen := []string{}, map[string]bool{symbol: true, short: true}
		for _, qname := range qnames {
			for _, spelling := range boundaryProperSuffixes(qname) {
				if !seen[spelling] {
					seen[spelling] = true
					spellings = append(spellings, spelling)
				}
			}
		}
		extending, err := s.unresolvedDstNamesExtending(ctx, repoID, qnames)
		if err != nil {
			return nil, err
		}
		for _, name := range extending {
			if !seen[name] {
				seen[name] = true
				spellings = append(spellings, name)
			}
		}
		for start := 0; start < len(spellings); start += neighborIDChunk {
			end := min(start+neighborIDChunk, len(spellings))
			chunk := spellings[start:end]
			branches = append(branches, `SELECT e.src_symbol_id FROM edges e
				WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
				  AND e.dst_name IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)`)
			args = append(args, repoID)
			for _, spelling := range chunk {
				args = append(args, spelling)
			}
		}
	}
	if len(branches) == 0 {
		return []graph.Symbol{}, nil
	}
	return s.symbolPage(ctx, repoID, strings.Join(branches, " UNION "), args, limit, offset)
}

// qualifiedNamesByIDs returns the distinct qualified names of the given
// symbols, ordered by name so downstream statement text is deterministic.
func (s *Store) qualifiedNamesByIDs(ctx context.Context, repoID int64, ids []int64) ([]string, error) {
	var out []string
	for _, chunk := range chunkInt64s(ids, neighborIDChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := s.db.QueryContext(ctx, `
			SELECT DISTINCT qualified_name FROM symbols
			WHERE repo_id = ? AND id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			ORDER BY qualified_name`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var qname string
			if err := rows.Scan(&qname); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, qname)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
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

// FindCallees returns the symbols the named symbol calls.
//
// Resolved edges are followed directly. Unresolved ones fall back to a name
// lookup, which is the same cascade a single-symbol lookup uses; that fallback
// is what lets a callee show up before the resolver has bound its edge.
func (s *Store) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	srcIDs, err := s.lookupSymbolIDs(ctx, repoID, symbol, symbolID)
	if err != nil {
		return nil, err
	}
	if len(srcIDs) == 0 {
		return []graph.Symbol{}, nil
	}

	resolvedSQL, args := edgeIDBranches(repoID, srcIDs, "dst_symbol_id", "src_symbol_id", "e.dst_symbol_id IS NOT NULL")
	branches := []string{resolvedSQL}

	dstNames, err := s.queryUnresolvedDstNamesBySrcIDs(ctx, repoID, srcIDs)
	if err != nil {
		return nil, err
	}
	if len(dstNames) > 0 {
		fallbackIDs, err := s.lookupSymbolIDsForNames(ctx, repoID, dstNames)
		if err != nil {
			return nil, err
		}
		for _, chunk := range chunkInt64s(fallbackIDs, neighborIDChunk) {
			sql, a := valueBranches(chunk)
			branches = append(branches, sql)
			args = append(args, a...)
		}
	}
	return s.symbolPage(ctx, repoID, strings.Join(branches, " UNION "), args, limit, offset)
}
