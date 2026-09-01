package store

import (
	"cmp"
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// Batched caller/callee expansion for context_for_task.
//
// Why this exists at all. Context expansion used to call the public
// FindCallers/FindCallees once per seed, which is two statements a seed plus
// whatever each of them costs internally -- and the caller leg's suffix match
// is a scan of the repository's unresolved-edge population, so that scan ran
// once *per seed*. Thirty seeds meant sixty statements and thirty scans, which
// is why the seed set had to be capped at eight to stay interactive.
//
// FindContextNeighbors answers the same question for a whole seed set with a
// fixed number of statements plus one per chunk:
//
//	1. one DISTINCT scan of unresolved destination names, shared by every seed
//	   whose short name may take part in suffix evidence (skipped entirely when
//	   none may),
//	2. one query for the unresolved destinations of every seed at once,
//	3. the existing batched name cascade over the union of those destinations,
//	4. one ranked page query per seed chunk, for callers and for callees.
//
// The candidate set per seed is unchanged from what the public queries return
// for the same evidence policy -- see ContextSeed -- and the per-seed order is
// the same total order: qualified_name, start_line, start_col, id.

const (
	// contextSeedChunk bounds how many seeds share one page statement. The
	// binding constraint is SQLITE_MAX_VARIABLE_NUMBER, which is 999 on a cgo
	// build; contextStatementVarBudget enforces that directly and this is the
	// secondary cap that keeps one statement's window partition count sane.
	contextSeedChunk = 64
	// contextStatementVarBudget is the bound-variable ceiling for one generated
	// statement. 700 leaves ample headroom under the historical 999-variable
	// limit for the fixed repo_id/fanout arguments and for a driver that counts
	// slightly differently.
	contextStatementVarBudget = 700
	// contextFanoutMax bounds the per-seed neighbour count this API will return,
	// independently of the store's 2000-row public page backstop. Context
	// expansion wants a handful of neighbours per seed; nothing here is a
	// paging surface.
	contextFanoutMax = 200
)

// neighborQuery is QueryContext with the statement counter P19's regression
// test reads. Every database round trip the batched neighbour pipeline makes
// goes through it, including the shared name cascade.
func (s *Store) neighborQuery(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s.neighborStmts.Add(1)
	return s.db.QueryContext(ctx, query, args...)
}

// neighborQuerier adapts neighborQuery to the queryContexter the shared
// Go-package-scope helpers take, so the sentence above stays true of them too:
// a round trip the neighbour pipeline makes is a round trip the probe counts.
type neighborQuerier struct{ s *Store }

func (q neighborQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.s.neighborQuery(ctx, query, args...)
}

// contextNeighborStatements returns how many statements the batched
// context-neighbour pipeline has issued on this store, and
// resetContextNeighborStatements zeroes it. Both exist for the regression test
// that pins P19's central claim -- neighbour work is chunk-driven, not
// seed-driven -- and are unexported because nothing outside this package has
// any business reading a statement counter.
func (s *Store) contextNeighborStatements() int64      { return s.neighborStmts.Load() }
func (s *Store) resetContextNeighborStatements() int64 { return s.neighborStmts.Swap(0) }

// ContextSeed is one persisted symbol identity to expand. The name fields are
// retained for callers that construct seeds, but relationship expansion uses
// SymbolID only; unresolved spellings are not promoted in context.
type ContextSeed struct {
	SymbolID      int64
	QualifiedName string
	// ShortName is only consulted when AllowShortEvidence is true.
	ShortName string
	// AllowShortEvidence admits bare-short-name and dotted/scoped-suffix
	// unresolved evidence. False means "this short name is ambiguous in this
	// repository"; it never means "no evidence at all".
	AllowShortEvidence bool
}

// ContextNeighbors is one seed's bounded neighbourhood.
type ContextNeighbors struct {
	Callers []graph.Symbol
	Callees []graph.Symbol
}

// LookupSymbolShortName exposes the short-name derivation the neighbour
// queries use, so a caller can build a ContextSeed whose ShortName is exactly
// the string the suffix patterns will be built from.
func LookupSymbolShortName(name string) string { return lookupSymbolShortName(name) }

// FindContextNeighbors returns, for each seed, up to fanout callers and fanout
// callees. The returned slice is parallel to seeds: index i is seed i, always,
// including for a seed with no id and for a seed with no neighbours.
func (s *Store) FindContextNeighbors(ctx context.Context, repoID int64, seeds []ContextSeed, fanout int) ([]ContextNeighbors, error) {
	out := make([]ContextNeighbors, len(seeds))
	for i := range out {
		out[i] = ContextNeighbors{Callers: []graph.Symbol{}, Callees: []graph.Symbol{}}
	}
	if len(seeds) == 0 {
		return out, nil
	}
	if fanout <= 0 {
		fanout = 1
	}
	fanout = min(fanout, contextFanoutMax)

	// A seed without a resolved id has no exact identity to expand from. It can
	// still carry name evidence, so it is not dropped -- only its id legs are.
	live := make([]int, 0, len(seeds))
	for i, sd := range seeds {
		if sd.SymbolID != 0 || sd.QualifiedName != "" {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return out, nil
	}

	if err := s.expandContextCallers(ctx, repoID, seeds, live, fanout, out); err != nil {
		return nil, err
	}
	if err := s.expandContextCallees(ctx, repoID, seeds, live, fanout, out); err != nil {
		return nil, err
	}
	return out, nil
}

// idPair is a (seed index, symbol id) association. Seed identity travels with
// every candidate rather than being reconstructed afterwards, which is what
// keeps one seed's neighbours off another seed.
type idPair struct {
	idx int
	id  int64
}

// namePair is a (seed index, destination name) association carrying the
// bare-name scope policy for that spelling: P22.6's for Go, P22.13's for C and
// C++.
//
// Three states, matching sqlGoBareSourceScope exactly so the batched pipeline
// and the public FindCallers answer the same question:
//
//	gated == false            any writer may spell this name (every
//	                          qualifier-bearing spelling, and every seed in a
//	                          language no scope rule governs)
//	gated, scope == ""        no bare call can name this seed at all -- a Go
//	                          method, or an unprovable Go package, or a C/C++
//	                          symbol with no file path -- so only writers in
//	                          other languages count
//	gated, scope == key       writers in other languages count, and so do
//	                          same-language writers inside the seed's own scope:
//	                          its Go package, or its C/C++ file (bareScopeKey)
//
// The middle state is why a bool is needed rather than an empty-string
// sentinel: "unnameable by a bare call" and "ungated" are different answers,
// and collapsing them would drop every non-Go caller of a Go method seed.
//
// `language` is the seed's own persisted language (P22.7) and gates the leg
// independently of `gated`: a spelling written in another language does not
// name this seed, whatever its syntax. An empty language means the seed has no
// symbol row to read one from, and then the leg keeps the evidence it had --
// a language CodeGraph does not know is not a language it may invent.
//
// Since P22.7 the middle state (gated, scope == "") can match no writer at all,
// because the non-Go escape it used to preserve is exactly the class P22.7
// removed. expandContextCallers therefore stops emitting that leg, and the bool
// survives only to distinguish "no package requirement" from "this package".
type namePair struct {
	idx      int
	name     string
	language string
	gated    bool
	scope    string
}

// expandContextCallers fills the Callers field of every live seed.
func (s *Store) expandContextCallers(ctx context.Context, repoID int64, seeds []ContextSeed, live []int, fanout int, out []ContextNeighbors) error {
	ids := make([]idPair, 0, len(live))
	for _, i := range live {
		if seeds[i].SymbolID != 0 {
			ids = append(ids, idPair{idx: i, id: seeds[i].SymbolID})
		}
	}
	return s.pagePartitionedNeighbors(ctx, repoID, ids, nil, nil, fanout, callerCandidateSQL, func(idx int, syms []graph.Symbol) {
		out[idx].Callers = syms
	})
}

// expandContextCallees fills the Callees field of every live seed.
//
// A callee is reached from the seed's own id -- never from its name, because
// the public query resolves a symbol id first and ignores the name once it has
// one. Resolved destinations map straight through. Unresolved ones go through
// the same name cascade FindCallees uses, and the seed -> name -> candidate
// association is preserved the whole way: flattening the names of thirty seeds
// into one set and giving every seed the union would be a correctness bug, not
// a batching win.
func (s *Store) expandContextCallees(ctx context.Context, repoID int64, seeds []ContextSeed, live []int, fanout int, out []ContextNeighbors) error {
	ids := make([]idPair, 0, len(live))
	for _, i := range live {
		if seeds[i].SymbolID != 0 {
			ids = append(ids, idPair{idx: i, id: seeds[i].SymbolID})
		}
	}
	return s.pagePartitionedNeighbors(ctx, repoID, ids, nil, nil, fanout, calleeCandidateSQL, func(idx int, syms []graph.Symbol) {
		out[idx].Callees = syms
	})
}

// extendingDstNames returns, per seed, the unresolved destination names that
// extend that seed's qualified identity at a '.', "::" or '/' boundary,
// ASCII-case-insensitively (the folding the escaped LIKE legs in FindCallers
// apply).
//
// One DISTINCT scan for the whole batch, matched per seed in Go, so the scan
// count stays independent of the seed count.
func (s *Store) extendingDstNames(ctx context.Context, repoID int64, qnamesByIdx map[int]string, seedScopes map[int]goSymbolScope) ([]namePair, error) {
	if len(qnamesByIdx) == 0 {
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

	// Deterministic seed order per matched name, so the emitted namePair order
	// cannot depend on map iteration. Seed identities are folded once up
	// front and each scanned row once, not once per (row, seed) pair.
	idxs := make([]int, 0, len(qnamesByIdx))
	for idx := range qnamesByIdx {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	foldedByIdx := make(map[int]string, len(qnamesByIdx))
	for idx, qname := range qnamesByIdx {
		foldedByIdx[idx] = asciiLower(qname)
	}

	var out []namePair
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		d := asciiLower(name)
		for _, idx := range idxs {
			if prefoldedExtendsAtBoundary(d, foldedByIdx[idx]) {
				out = append(out, namePair{idx: idx, name: name, language: seedScopes[idx].language})
			}
		}
	}
	return out, rows.Err()
}

// unresolvedDstNamesBySeed returns every unresolved destination name of every
// seed, tagged with the seed it belongs to, in one query per id chunk.
func (s *Store) unresolvedDstNamesBySeed(ctx context.Context, repoID int64, srcIDs []idPair, bySymbol map[int64][]int) ([]namePair, error) {
	ids := make([]int64, 0, len(srcIDs))
	seen := map[int64]bool{}
	for _, p := range srcIDs {
		if !seen[p.id] {
			seen[p.id] = true
			ids = append(ids, p.id)
		}
	}
	var out []namePair
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := s.neighborQuery(ctx, `
			SELECT DISTINCT e.src_symbol_id, e.dst_name
			FROM edges e
			WHERE e.repo_id = ? AND e.src_symbol_id IN (`+placeholders(len(chunk))+`)
			  AND e.dst_symbol_id IS NULL AND e.dst_name != ''
			ORDER BY e.src_symbol_id ASC, e.dst_name ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var src int64
			var name string
			if err := rows.Scan(&src, &name); err != nil {
				_ = rows.Close()
				return nil, err
			}
			for _, idx := range bySymbol[src] {
				out = append(out, namePair{idx: idx, name: name})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// callerCandidateSQL and calleeCandidateSQL render the candidate half of the
// page statement from the CTE names the chunk provided. `sd` is (idx, sid),
// `nm` is (idx, dname), `fb` is (idx, nid).
//
// UNION, not UNION ALL: one caller can be reachable from a seed through the
// resolved edge, the qualified-name edge and the short-name edge at once, and
// it must appear in that seed's page once. Across seeds nothing is merged --
// (idx, nid) is the set element, so a caller that neighbours two seeds is
// legitimately in both pages.
// candidateLegs says which CTEs the chunk actually produced.
type candidateLegs struct {
	ids      bool
	names    bool
	fallback bool
}

func callerCandidateSQL(legs candidateLegs) (string, int) {
	var branches []string
	repoArgs := 0
	if legs.ids {
		branches = append(branches, `SELECT sd.idx, e.src_symbol_id
			FROM sd CROSS JOIN edges e ON e.dst_symbol_id = sd.sid
			WHERE e.repo_id = ?`)
		repoArgs++
	}
	if legs.names {
		// CROSS JOIN is load-bearing, not decoration. Left to itself SQLite
		// planned this branch from the edge table -- "SEARCH e ... SCAN nm" --
		// so every unresolved edge in the repository was compared against every
		// name in the seed batch instead of each name seeking
		// idx_edges_repo_unresolved_name_src once. On this repository (16k
		// edges, 12k of them unresolved) pinning the order took thirty-seed
		// caller expansion from ~75ms to ~3ms, and the cost it removes grows
		// with the unresolved population rather than with the answer.
		//
		// Two rules decide which writer may claim a seed by name, and one
		// correlated lookup of the writer decides both.
		//
		// nm.lang is the seed's own persisted language (P22.7): a writer in
		// another language did not name this seed. It is '' only for a seed
		// CodeGraph could not identify, and then the leg keeps the evidence it
		// had -- a language we do not know is not one we may invent. That is
		// also the only case the leading disjunct short-circuits, so the EXISTS
		// is the normal path, not a rare one.
		//
		// nm.gated is 1 only for a bare spelling against a scope-gated seed (see
		// goBareTargetSeedScope): a Go one, where the extra requirement is
		// P22.6's -- the writer lives in the seed's own package -- or a C/C++
		// one, where P22.13's is that the writer lives in the seed's own file.
		// nm.lang is the seed's language in both cases, and the key expression
		// is bareScopeKey, so the two rules cannot be given different keys here
		// than the resolver and FindCallers give them. There is deliberately no
		// `src.language <> 'go'` escape any more: before P22.7 that disjunct let
		// non-Go writers through, and P22.7's whole point is that they were
		// never really callers. A gated leg with an empty scope can match nobody
		// at all and is not emitted (expandContextCallers).
		//
		// `files` is joined inside the innermost EXISTS rather than beside
		// `symbols`, so only the gated branch pays for it: the scope key is
		// the one thing that needs the writer's path.
		branches = append(branches, `SELECT nm.idx, e.src_symbol_id
			FROM nm CROSS JOIN edges e ON e.dst_name = nm.dname
			WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL
			  AND ((nm.gated = 0 AND nm.lang = '') OR EXISTS (
			        SELECT 1 FROM symbols src
			        WHERE src.id = e.src_symbol_id
			          AND (nm.lang = '' OR src.language = nm.lang)
			          AND (nm.gated = 0
			               OR (nm.scope <> '' AND EXISTS (
			                     SELECT 1 FROM files srcf
			                     WHERE srcf.id = src.file_id
				                       AND `+sqlBareScopeKeyForSymbol("src", "srcf.path")+` = nm.scope)))))`)
		repoArgs++
	}
	return strings.Join(branches, " UNION "), repoArgs
}

func calleeCandidateSQL(legs candidateLegs) (string, int) {
	var branches []string
	repoArgs := 0
	if legs.ids {
		// The unary + is load-bearing too. `e.dst_symbol_id IS NOT NULL` is a
		// usable index term, and SQLite preferred it: it planned
		// "SEARCH e USING idx_edges_repo_dst_file (repo_id=? AND
		// dst_symbol_id>?)" -- a range scan of every resolved edge in the
		// repository, once per seed -- instead of seeking idx_edges_repo_src on
		// the join key. Measured here that was ~26ms of a 29ms thirty-seed
		// callee expansion. The + makes the term unindexable, so the join key
		// is the only thing left to seek on; the rows selected are identical.
		branches = append(branches, `SELECT sd.idx, e.dst_symbol_id
			FROM sd CROSS JOIN edges e ON e.src_symbol_id = sd.sid
			WHERE e.repo_id = ? AND +e.dst_symbol_id IS NOT NULL`)
		repoArgs++
	}
	if legs.fallback {
		branches = append(branches, `SELECT fb.idx, fb.nid FROM fb`)
	}
	return strings.Join(branches, " UNION "), repoArgs
}

// neighborWork is the evidence of one seed that fits into one statement. A
// seed's evidence can be split across several of these -- see
// pagePartitionedNeighbors.
type neighborWork struct {
	idx      int
	ids      []int64
	names    []namePair
	fallback []int64
}

// Bound variables per evidence row, by leg. An id row binds (idx, id); a name
// row binds (idx, dname, gated, scope) since P22.6 added the package-scope
// policy columns, plus (lang) since P22.7 added the language gate.
// splitNeighborWork budgets in variables rather than rows precisely because the
// legs no longer cost the same.
//
// A wider name row means fewer of them per statement: at
// contextStatementVarBudget = 700 a statement now holds 140 name rows rather
// than 175, so a seed with very large suffix evidence is split across ~25% more
// page statements. The statement count is still driven by evidence volume
// rather than by seed count, which is the invariant P19 pinned, and P22.7 also
// stops emitting the leg for seeds no writer can name.
const (
	neighborIDVars   = 2
	neighborNameVars = 5
)

// vars counts bound variables, not rows.
func (w neighborWork) vars() int {
	return neighborIDVars*(len(w.ids)+len(w.fallback)) + neighborNameVars*len(w.names)
}

func (w neighborWork) empty() bool { return w.vars() == 0 }

// pagePartitionedNeighbors runs the ranked page statement over evidence chunks
// and hands each row to emit, tagged with its seed index.
//
// Chunking is driven by the bound-variable budget, not by the seed count. That
// matters in both directions: many small seeds share a statement, and one seed
// whose short name matches hundreds of unresolved destinations is split across
// statements rather than being allowed to build one that exceeds the driver's
// ceiling -- 999 bound variables on a cgo build.
//
// Splitting one seed's evidence is safe because the per-seed page is a top-N
// under a total order: each statement returns the best fanout rows of the
// evidence it was given, so the union of the parts contains the best fanout
// rows of the whole, and mergeNeighborPage re-trims. It is not safe to assume a
// single statement, which is why the merge runs unconditionally.
func (s *Store) pagePartitionedNeighbors(
	ctx context.Context,
	repoID int64,
	ids []idPair,
	names []namePair,
	fallback []idPair,
	fanout int,
	candidate func(candidateLegs) (string, int),
	emit func(idx int, syms []graph.Symbol),
) error {
	idsBySeed := groupIDPairs(ids)
	namesBySeed := groupNamePairs(names)
	fallbackBySeed := groupIDPairs(fallback)

	collected := map[int][]graph.Symbol{}
	collect := func(idx int, sym graph.Symbol) { collected[idx] = append(collected[idx], sym) }

	var chunk []neighborWork
	vars := 0
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		err := s.pageNeighborChunk(ctx, repoID, chunk, fanout, candidate, collect)
		chunk, vars = nil, 0
		return err
	}

	for _, idx := range seedOrder(idsBySeed, namesBySeed, fallbackBySeed) {
		remaining := neighborWork{idx: idx, ids: idsBySeed[idx], names: namesBySeed[idx], fallback: fallbackBySeed[idx]}
		for !remaining.empty() {
			if vars >= contextStatementVarBudget || len(chunk) >= contextSeedChunk {
				if err := flush(); err != nil {
					return err
				}
			}
			var part neighborWork
			part, remaining = splitNeighborWork(remaining, contextStatementVarBudget-vars)
			if part.empty() {
				// No room left in this statement for even one row.
				if err := flush(); err != nil {
					return err
				}
				continue
			}
			chunk = append(chunk, part)
			vars += part.vars()
		}
	}
	if err := flush(); err != nil {
		return err
	}

	for idx, syms := range collected {
		emit(idx, mergeNeighborPage(syms, fanout))
	}
	return nil
}

// splitNeighborWork takes as much of w as fits in maxVars bound variables, in a
// fixed leg order, and returns the taken part and what is left.
//
// The budget is in variables, not rows, because the legs cost different amounts
// (neighborIDVars vs neighborNameVars). Counting rows and assuming two variables
// each would let one all-names part bind twice the budget -- 1400 variables at
// contextStatementVarBudget = 700, past the 999-variable ceiling a cgo build
// still enforces.
func splitNeighborWork(w neighborWork, maxVars int) (neighborWork, neighborWork) {
	part := neighborWork{idx: w.idx}
	room := maxVars
	take := func(n, cost int) int {
		if room < cost {
			return 0
		}
		return min(room/cost, n)
	}
	if n := take(len(w.ids), neighborIDVars); n > 0 {
		part.ids, w.ids = w.ids[:n], w.ids[n:]
		room -= n * neighborIDVars
	}
	if n := take(len(w.names), neighborNameVars); n > 0 {
		part.names, w.names = w.names[:n], w.names[n:]
		room -= n * neighborNameVars
	}
	if n := take(len(w.fallback), neighborIDVars); n > 0 {
		part.fallback, w.fallback = w.fallback[:n], w.fallback[n:]
		room -= n * neighborIDVars
	}
	return part, w
}

// mergeNeighborPage reduces the rows a seed collected across statements to one
// page: the same total order the SQL window used, deduplicated by symbol id,
// trimmed to fanout. A single-statement seed is already in that order and
// already unique, so this is a no-op for it beyond one pass.
func mergeNeighborPage(syms []graph.Symbol, fanout int) []graph.Symbol {
	if len(syms) == 0 {
		return []graph.Symbol{}
	}
	slices.SortStableFunc(syms, func(a, b graph.Symbol) int {
		if c := cmp.Compare(a.QualifiedName, b.QualifiedName); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Range.StartLine, b.Range.StartLine); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Range.StartCol, b.Range.StartCol); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	out := syms[:0]
	var last int64
	for _, sym := range syms {
		if len(out) > 0 && sym.ID == last {
			continue
		}
		last = sym.ID
		out = append(out, sym)
		if len(out) == fanout {
			break
		}
	}
	return out
}

func (s *Store) pageNeighborChunk(
	ctx context.Context,
	repoID int64,
	chunk []neighborWork,
	fanout int,
	candidate func(candidateLegs) (string, int),
	emit func(idx int, sym graph.Symbol),
) error {
	var ctes []string
	var cteArgs []any

	addIDCTE := func(name, col string, pick func(neighborWork) []int64) bool {
		var rows []string
		for _, w := range chunk {
			for _, id := range pick(w) {
				rows = append(rows, "(?,?)")
				cteArgs = append(cteArgs, int64(w.idx), id)
			}
		}
		if len(rows) == 0 {
			return false
		}
		ctes = append(ctes, name+"(idx, "+col+") AS (VALUES "+strings.Join(rows, ",")+")")
		return true
	}

	var legs candidateLegs
	legs.ids = addIDCTE("sd", "sid", func(w neighborWork) []int64 { return w.ids })
	{
		var rows []string
		for _, w := range chunk {
			for _, n := range w.names {
				gated := int64(0)
				if n.gated {
					gated = 1
				}
				rows = append(rows, "(?,?,?,?,?)")
				cteArgs = append(cteArgs, int64(w.idx), n.name, gated, n.scope, n.language)
			}
		}
		if len(rows) > 0 {
			ctes = append(ctes, "nm(idx, dname, gated, scope, lang) AS (VALUES "+strings.Join(rows, ",")+")")
			legs.names = true
		}
	}
	legs.fallback = addIDCTE("fb", "nid", func(w neighborWork) []int64 { return w.fallback })

	candSQL, repoArgs := candidate(legs)
	if candSQL == "" {
		return nil
	}

	args := make([]any, 0, len(cteArgs)+repoArgs+2)
	args = append(args, cteArgs...)
	for range repoArgs {
		args = append(args, repoID)
	}
	args = append(args, repoID, fanout)

	rows, err := s.neighborQuery(ctx, partitionedNeighborPageSQL(ctes, candSQL), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int64
		var sym graph.Symbol
		if err := rows.Scan(
			&idx,
			&sym.ID, &sym.FileID, &sym.Language, &sym.Kind, &sym.Name, &sym.QualifiedName, &sym.ContainerName,
			&sym.Signature, &sym.Visibility, &sym.Range.StartLine, &sym.Range.StartCol, &sym.Range.EndLine,
			&sym.Range.EndCol, &sym.DocSummary, &sym.StableKey, &sym.FilePath,
		); err != nil {
			return err
		}
		// Same normalization scanSymbol applies, for the same reason: every path
		// this store hands out is slash-form whatever the host wrote.
		sym.FilePath = filepath.ToSlash(sym.FilePath)
		emit(int(idx), sym)
	}
	return rows.Err()
}

// partitionedNeighborPageSQL is symbolPageSQL's per-seed twin.
//
// The page is taken with ROW_NUMBER() inside the same statement rather than by
// slicing in Go, for the reason P12 gave for the single-symbol query: a hub
// with thousands of callers must not turn into thousands of graph.Symbol
// values just to return ten of them. SQLite still has to order each partition,
// exactly as the LIMIT in symbolPageSQL makes it order the single candidate
// set, but what crosses into Go is bounded by seeds*fanout.
//
// The ORDER BY inside the window is the same total order symbolPageSQL uses,
// so a seed's neighbour page here is the same page, in the same order, that
// FindCallers/FindCallees would return for the same evidence.
func partitionedNeighborPageSQL(ctes []string, candidateSQL string) string {
	return `
		WITH ` + strings.Join(ctes, ",\n		     ") + `,
		cand(idx, nid) AS (` + candidateSQL + `),
		ranked AS (
			SELECT c.idx AS idx, s.id AS sid, s.file_id AS file_id, s.language AS language, s.kind AS kind,
			       s.name AS name, s.qualified_name AS qualified_name, s.container_name AS container_name,
			       s.signature AS signature, s.visibility AS visibility, s.start_line AS start_line,
			       s.start_col AS start_col, s.end_line AS end_line, s.end_col AS end_col,
			       s.doc_summary AS doc_summary, s.stable_key AS stable_key, f.path AS path,
			       ROW_NUMBER() OVER (
			           PARTITION BY c.idx
			           ORDER BY s.qualified_name ASC, s.start_line ASC, s.start_col ASC, s.id ASC
			       ) AS rn
			-- CROSS JOIN for the reason symbolPageSQL gives: it pins the candidate
			-- set as the outer loop instead of letting SQLite drive from symbols and
			-- re-evaluate the candidate co-routine per symbol row.
			FROM cand c
			CROSS JOIN symbols s ON s.id = c.nid
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ?
		)
		SELECT idx, sid, file_id, language, kind, name, qualified_name, container_name, signature, visibility,
		       start_line, start_col, end_line, end_col, doc_summary, stable_key, path
		FROM ranked
		WHERE rn <= ?
		ORDER BY idx ASC, qualified_name ASC, start_line ASC, start_col ASC, sid ASC
	`
}

func groupIDPairs(pairs []idPair) map[int][]int64 {
	out := map[int][]int64{}
	seen := map[idPair]bool{}
	for _, p := range pairs {
		if seen[p] {
			continue
		}
		seen[p] = true
		out[p.idx] = append(out[p.idx], p.id)
	}
	return out
}

func groupNamePairs(pairs []namePair) map[int][]namePair {
	out := map[int][]namePair{}
	seen := map[namePair]bool{}
	for _, p := range pairs {
		if seen[p] {
			continue
		}
		seen[p] = true
		out[p.idx] = append(out[p.idx], p)
	}
	return out
}

// seedOrder lists every seed index that has any evidence, ascending, so chunk
// membership is a function of the input and not of Go map iteration.
func seedOrder(ids map[int][]int64, names map[int][]namePair, fallback map[int][]int64) []int {
	present := map[int]bool{}
	for idx := range ids {
		present[idx] = true
	}
	for idx := range names {
		present[idx] = true
	}
	for idx := range fallback {
		present[idx] = true
	}
	out := make([]int, 0, len(present))
	for idx := range present {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}
