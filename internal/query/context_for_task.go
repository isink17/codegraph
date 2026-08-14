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
	FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error)
	FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error)
	RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int) ([]store.RelatedTest, error)
}

const (
	defaultContextMaxFiles   = 10
	defaultContextMaxSymbols = 30
	// contextNeighborFanout bounds callers and callees fetched per expanded seed.
	contextNeighborFanout = 10
	// contextExpansionSeeds bounds how many seeds are expanded through the graph.
	//
	// Expansion costs two queries per seed, and the caller leg has to walk the
	// repository's unresolved-edge population to catch calls the resolver could
	// not bind. Measured on this repository that is ~2ms a seed, so expanding all
	// 30 default seeds cost ~130ms -- well past interactive for a tool an agent
	// calls first. Expansion now covers the strongest seeds only: the seed stream
	// is in search-relevance order, and a weak seed's neighbours score below a
	// strong seed's by construction, so they were already the last records a
	// budget would ever reach. Direct matches are unaffected -- all MaxSymbols of
	// them stay in the candidate universe.
	contextExpansionSeeds = 8
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
		// FindCallers/FindCallees take a symbol id and a name. The id half is
		// exact. The name half also matches edges the resolver left unresolved,
		// which is real caller evidence worth keeping -- but only when the name
		// belongs to exactly one symbol in the repository. Where it does not, the
		// name is dropped and expansion follows the seed's id alone, so a seed
		// never inherits the graph of a same-named symbol in another package.
		//
		// This is deliberately coarse: the name legs of those queries (exact
		// dst_name, and the dotted-suffix LIKE) are all-or-nothing, so an ambiguous
		// short name also costs the unambiguous `dst_name = qualified_name`
		// evidence. Splitting them needs a qualified-name-only mode on both store
		// queries; until then the safe direction is to lose recall, not to
		// attribute another package's callers to this seed.
		expanded := seeds
		if len(expanded) > contextExpansionSeeds {
			expanded = expanded[:contextExpansionSeeds]
		}
		names := make([]string, 0, len(expanded))
		for _, sd := range expanded {
			names = append(names, sd.sym.Name)
		}
		counts, err := s.ctxStore.SymbolNameCounts(ctx, repoID, names)
		if err != nil {
			return nil, fmt.Errorf("seed name ambiguity: %w", err)
		}
		for _, sd := range expanded {
			lookupName := ""
			if counts[sd.sym.Name] == 1 {
				lookupName = sd.sym.QualifiedName
			}
			callers, err := s.ctxStore.FindCallers(ctx, repoID, lookupName, sd.sym.ID, contextNeighborFanout, 0)
			if err != nil {
				return nil, fmt.Errorf("expand callers of %s: %w", sd.sym.QualifiedName, err)
			}
			for _, c := range callers {
				set.add(contextCandidate{sym: c, relevance: relevanceCaller, taskSignal: sd.signal})
			}
			callees, err := s.ctxStore.FindCallees(ctx, repoID, lookupName, sd.sym.ID, contextNeighborFanout, 0)
			if err != nil {
				return nil, fmt.Errorf("expand callees of %s: %w", sd.sym.QualifiedName, err)
			}
			for _, c := range callees {
				set.add(contextCandidate{sym: c, relevance: relevanceCallee, taskSignal: sd.signal})
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
			ref := store.SymbolRef{File: t.File, QualifiedName: t.Symbol}
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
