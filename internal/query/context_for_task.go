package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// ContextForTaskOptions controls the behaviour of ContextForTask.
//
// MaxFiles and MaxSymbols keep their pre-P14 meaning: MaxSymbols bounds the
// semantic-search seed set, MaxFiles bounds how many production files the
// answer covers. Together with IncludeTests and IncludeCallers they define the
// candidate universe. MaxTokens then pages that universe; it never widens it.
type ContextForTaskOptions struct {
	MaxFiles       int
	MaxSymbols     int
	IncludeTests   bool
	IncludeCallers bool

	// MaxTokens bounds the serialized response. Zero selects
	// DefaultContextMaxTokens, negative is an error, and a request above
	// MaxContextMaxTokens is clamped to it. It may change between pages of one
	// cursor sequence.
	MaxTokens int
	// Cursor continues a previous call. It must be replayed with the same task
	// and the same ranking-affecting options against the same graph generation.
	Cursor string
}

// contextStoreOps is the read-only store surface context expansion uses. Only
// ContextForTask goes through it; every other Service method calls the store
// directly.
type contextStoreOps interface {
	LastScanID(ctx context.Context, repoID int64) (int64, error)
	SymbolsForRefs(ctx context.Context, repoID int64, refs []store.SymbolRef) (map[store.SymbolRef]graph.Symbol, error)
	SymbolNameCounts(ctx context.Context, repoID int64, names []string) (map[string]int, error)
	FindContextNeighbors(ctx context.Context, repoID int64, seeds []store.ContextSeed, fanout int) ([]store.ContextNeighbors, error)
	RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int) ([]store.RelatedTest, error)
}

const (
	defaultContextMaxFiles   = 10
	defaultContextMaxSymbols = 30
	// contextNeighborFanout bounds callers and callees fetched per expanded seed.
	contextNeighborFanout = 10
	// contextExpansionSeeds bounds how many seeds are expanded through the graph.
	//
	// It used to be 8, because expansion cost two queries per seed and the caller
	// leg re-scanned the repository's unresolved-edge population for each one:
	// per-seed cost was roughly constant, so the total was linear and 30 seeds
	// were not interactive. P19's FindContextNeighbors answers the whole seed set
	// in a fixed number of statements with one shared scan, which makes the
	// marginal cost of a seed small enough that the default seed count fits.
	//
	// It is deliberately pinned to the default MaxSymbols rather than left
	// unbounded. MaxSymbols is a public argument and P18 allows it well above the
	// default; expansion work is bounded by *this* number so a large explicit
	// MaxSymbols widens the direct-match set -- which is cheap, one batched
	// lookup -- without widening graph expansion. Every seed past it is still in
	// the candidate universe as a direct match.
	contextExpansionSeeds = defaultContextMaxSymbols
	// contextTestsPerFile bounds related tests fetched per returned file.
	contextTestsPerFile = 10
)

// ContextForTask returns the most relevant files, symbols, and relationships for
// a natural-language task description, ranked deterministically and trimmed to a
// token budget.
//
// It is a selector, not a source dump: every returned symbol carries symbol_id
// and stable_key so a caller can fetch source from find_symbol at
// detail=excerpt or detail=full. Context that did not fit the budget is not
// lost -- next_cursor continues the same ranked stream.
func (s *Service) ContextForTask(ctx context.Context, repoID int64, task string, opts ContextForTaskOptions) (*graph.TaskContext, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = defaultContextMaxFiles
	}
	if opts.MaxSymbols <= 0 {
		opts.MaxSymbols = defaultContextMaxSymbols
	}
	maxTokens, err := normalizeContextTokens(opts.MaxTokens)
	if err != nil {
		return nil, err
	}

	// The graph generation identity. A cursor issued before a re-index must not
	// be honoured after it, because the candidate stream it points into no longer
	// exists.
	scanID, err := s.ctxStore.LastScanID(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("graph generation: %w", err)
	}

	candidates, err := s.rankContextCandidates(ctx, repoID, task, opts)
	if err != nil {
		return nil, err
	}

	// Ranking is several statements, not one snapshot, so an indexing run that
	// commits while they execute would produce a page assembled from two
	// generations -- and stamp it with whichever one was read first. Re-read the
	// generation and refuse rather than serve the mixture.
	scanAfter, err := s.ctxStore.LastScanID(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("graph generation: %w", err)
	}
	if scanAfter != scanID {
		return nil, fmt.Errorf("graph was re-indexed while selecting context (scan %d -> %d); retry", scanID, scanAfter)
	}

	want := contextCursorState{
		V:    contextCursorVersion,
		Repo: repoID,
		Scan: scanID,
		Task: taskFingerprint(task),
		Opts: optionsFingerprint(opts),
		Rank: rankingFingerprint(candidates),
	}

	offset := 0
	if strings.TrimSpace(opts.Cursor) != "" {
		got, err := decodeContextCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		if err := validateContextCursor(got, want, len(candidates)); err != nil {
			return nil, err
		}
		offset = got.Offset
	}

	return buildBudgetedContext(budgetInput{
		task:       task,
		candidates: candidates,
		offset:     offset,
		maxTokens:  maxTokens,
		cursor:     want,
	})
}

// rankContextCandidates builds the one deterministically ordered candidate
// stream for a task. The same options and the same graph always produce the same
// stream, which is what makes an offset cursor safe.
func (s *Service) rankContextCandidates(ctx context.Context, repoID int64, task string, opts ContextForTaskOptions) ([]contextCandidate, error) {
	rawHits, err := s.SemanticSearch(ctx, repoID, task, opts.MaxSymbols, 0)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	hits := parseSearchHits(rawHits)

	// One batched lookup turns (file, qualified_name) hits into real symbol rows:
	// identity for drill-down, and the exact node to expand the graph from.
	resolved, err := s.ctxStore.SymbolsForRefs(ctx, repoID, hitRefs(hits))
	if err != nil {
		return nil, fmt.Errorf("resolve seed symbols: %w", err)
	}

	signals := normalizeTaskSignal(hits)
	set := newCandidateSet()

	type seed struct {
		sym    graph.Symbol
		signal float64
	}
	seeds := make([]seed, 0, len(hits))
	for i, hit := range hits {
		sym, ok := resolved[store.SymbolRef{File: hit.File, QualifiedName: hit.QualifiedName}]
		if !ok {
			// The hit named no row we can identify (an aggregate over symbols
			// without a qualified name, or a row deleted since the search).
			continue
		}
		set.add(contextCandidate{sym: sym, relevance: relevanceDirectMatch, taskSignal: signals[i]})
		seeds = append(seeds, seed{sym: sym, signal: signals[i]})
	}

	if opts.IncludeCallers {
		// Expansion evidence is per seed and typed -- see store.ContextSeed. The
		// symbol id and the fully qualified name identify one symbol, so both are
		// always safe. The bare short name and the dotted/scoped suffix do not:
		// `Renew` fits billing.Renew and subscription.Renew equally. Those two legs
		// are therefore admitted only for a short name that exactly one symbol in
		// the repository carries.
		//
		// Before P19 this was one lookup name that turned all three name legs on or
		// off together, so an ambiguous short name also cost the unambiguous
		// `dst_name = qualified_name` evidence. That is recall thrown away for no
		// safety: the qualified name is not ambiguous.
		//
		// The ambiguity question is asked about the short name the *evidence* would
		// match on -- LookupSymbolShortName(qualified_name) -- not about the
		// symbol's `name` column, because that is the string the short and suffix
		// patterns are built from.
		expanded := seeds
		if len(expanded) > contextExpansionSeeds {
			expanded = expanded[:contextExpansionSeeds]
		}
		shorts := make([]string, len(expanded))
		for i, sd := range expanded {
			shorts[i] = store.LookupSymbolShortName(sd.sym.QualifiedName)
		}
		counts, err := s.ctxStore.SymbolNameCounts(ctx, repoID, shorts)
		if err != nil {
			return nil, fmt.Errorf("seed name ambiguity: %w", err)
		}
		storeSeeds := make([]store.ContextSeed, len(expanded))
		for i, sd := range expanded {
			storeSeeds[i] = store.ContextSeed{
				SymbolID:           sd.sym.ID,
				QualifiedName:      sd.sym.QualifiedName,
				ShortName:          shorts[i],
				AllowShortEvidence: shorts[i] != "" && counts[shorts[i]] == 1,
			}
		}
		neighbors, err := s.ctxStore.FindContextNeighbors(ctx, repoID, storeSeeds, contextNeighborFanout)
		if err != nil {
			return nil, fmt.Errorf("expand seed neighbours: %w", err)
		}
		if len(neighbors) != len(expanded) {
			return nil, fmt.Errorf("expand seed neighbours: got %d results for %d seeds", len(neighbors), len(expanded))
		}
		for i, n := range neighbors {
			for _, c := range n.Callers {
				set.add(contextCandidate{sym: c, relevance: relevanceCaller, taskSignal: expanded[i].signal})
			}
			for _, c := range n.Callees {
				set.add(contextCandidate{sym: c, relevance: relevanceCallee, taskSignal: expanded[i].signal})
			}
		}
	}

	// Apply the file cap before test expansion, so related tests are fetched for
	// the files the answer actually covers -- one query per returned file rather
	// than one per candidate file.
	production := limitContextFiles(set.finalize(), opts.MaxFiles)
	if !opts.IncludeTests {
		return production, nil
	}

	withTests := newCandidateSet()
	for _, cand := range production {
		withTests.add(cand)
	}
	testCands, err := s.testCandidates(ctx, repoID, production)
	if err != nil {
		return nil, err
	}
	for _, cand := range testCands {
		withTests.add(cand)
	}
	return withTests.finalize(), nil
}

// testCandidates fetches related tests for the files of the production
// candidates, in candidate-rank order, and resolves each test symbol to a real
// row so it carries the same drill-down identity as any other candidate.
func (s *Service) testCandidates(ctx context.Context, repoID int64, production []contextCandidate) ([]contextCandidate, error) {
	seenFile := map[string]bool{}
	var refs []store.SymbolRef
	scores := map[store.SymbolRef]float64{}
	maxScore := 0.0
	for _, cand := range production {
		path := cand.sym.FilePath
		if seenFile[path] {
			continue
		}
		seenFile[path] = true
		tests, err := s.ctxStore.RelatedTests(ctx, repoID, "", path, contextTestsPerFile, 0)
		if err != nil {
			return nil, fmt.Errorf("related tests for %s: %w", path, err)
		}
		for _, t := range tests {
			// Canonical on the way in: SymbolsForRefs keys its result map
			// canonically, so a raw producer path here would look up a key that is
			// not there and lose every test candidate without an error.
			ref := store.SymbolRef{File: store.CanonicalRelPath(t.File), QualifiedName: t.Symbol}
			if _, ok := scores[ref]; !ok {
				refs = append(refs, ref)
			}
			// A test can link to several returned files; the strongest link is the
			// one that describes it.
			if t.Score > scores[ref] {
				scores[ref] = t.Score
			}
			if t.Score > maxScore {
				maxScore = t.Score
			}
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	resolved, err := s.ctxStore.SymbolsForRefs(ctx, repoID, refs)
	if err != nil {
		return nil, fmt.Errorf("resolve test symbols: %w", err)
	}
	out := make([]contextCandidate, 0, len(refs))
	for _, ref := range refs {
		sym, ok := resolved[ref]
		if !ok {
			continue
		}
		signal := 0.0
		if maxScore > 0 {
			signal = scores[ref] / maxScore
		}
		out = append(out, contextCandidate{sym: sym, relevance: relevanceTest, taskSignal: signal, isTest: true})
	}
	return out, nil
}

// limitContextFiles keeps the candidates that live in the highest-ranked
// maxFiles files. File rank is the position of a file's best candidate in the
// already-ordered stream, so this trims the universe without disturbing order.
func limitContextFiles(ordered []contextCandidate, maxFiles int) []contextCandidate {
	if maxFiles <= 0 {
		return ordered
	}
	keep := make(map[string]bool, maxFiles)
	out := make([]contextCandidate, 0, len(ordered))
	for _, cand := range ordered {
		path := cand.sym.FilePath
		if !keep[path] {
			if len(keep) >= maxFiles {
				continue
			}
			keep[path] = true
		}
		out = append(out, cand)
	}
	return out
}
