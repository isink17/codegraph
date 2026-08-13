package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/parser/heuristic"
	"github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

// SchemaID versions the JSON report shape.
const SchemaID = "codegraph.resolver_correctness_audit/v1"

// DefaultRepeats is how many independent index runs the harness performs to
// decide whether resolver output is stable. Indexing is concurrent and symbol
// row ids depend on worker completion order, so a single run cannot tell a
// stable selection from a coin flip.
const DefaultRepeats = 5

// Options configures a harness run.
type Options struct {
	// Repeats is the number of independent fixture+database runs. <1 means
	// DefaultRepeats.
	Repeats int
	// WorkDir, when set, is used instead of a temporary directory and is not
	// removed, for debugging.
	WorkDir string
	// IncludeIncrementalParity additionally measures the incremental
	// (update) resolution path and compares it to the full-index path.
	IncludeIncrementalParity bool
}

// Report is the machine-readable audit result. Every slice is emitted in a
// deterministic order and no field carries a database row id, because row ids
// are not stable between runs.
type Report struct {
	Schema       string            `json:"schema"`
	CreatedAt    string            `json:"created_at"`
	Command      []string          `json:"command"`
	Context      RunContext        `json:"context"`
	Fixture      FixtureInfo       `json:"fixture"`
	Metrics      Metrics           `json:"metrics"`
	Cases        []CaseResult      `json:"cases"`
	Unclassified []EdgeObservation `json:"unclassified_edges"`
	Determinism  Determinism       `json:"determinism"`
	Incremental  *Incremental      `json:"incremental_parity,omitempty"`
	Findings     []string          `json:"findings"`
}

// RunContext records provenance. It is the only part of the report expected to
// differ between machines or runs.
type RunContext struct {
	GoVersion    string   `json:"go_version"`
	GOOS         string   `json:"goos"`
	GOARCH       string   `json:"goarch"`
	NumCPU       int      `json:"num_cpu"`
	SQLiteDriver string   `json:"sqlite_driver"`
	Adapters     []string `json:"adapters"`
	CommitSHA    string   `json:"commit_sha,omitempty"`
}

// FixtureInfo describes what was measured.
type FixtureInfo struct {
	Version     string   `json:"version"`
	Cases       int      `json:"cases"`
	Files       int      `json:"files"`
	Languages   []string `json:"languages"`
	FFIDeclared bool     `json:"ffi_declared"`
}

// Metrics are the aggregate counters. They are computed from every call edge in
// the persisted graph, not only from the declared cases.
type Metrics struct {
	CallEdgesInspected      int `json:"call_edges_inspected"`
	Resolved                int `json:"resolved"`
	Unresolved              int `json:"unresolved"`
	CrossLanguageResolved   int `json:"cross_language_resolved"`
	SameLanguageResolved    int `json:"same_language_resolved"`
	UnknownLanguageResolved int `json:"unknown_language_resolved"`

	// The counters below classify a case only when every repeat agreed.
	Cases               int `json:"cases"`
	ExpectedValid       int `json:"expected_valid"`
	ExpectedUnresolved  int `json:"expected_unresolved"`
	CorrectlyUnresolved int `json:"correctly_unresolved"`
	Miswired            int `json:"miswired"`
	MissingEdge         int `json:"missing_edge"`
	NoCallEdge          int `json:"no_call_edge"`
	Nondeterministic    int `json:"nondeterministic"`

	// MiswiredAnyRun counts cases that miswired in at least one repeat. It is
	// the worst-case figure and the one to quote: a case that miswires
	// intermittently is classified Nondeterministic above and would otherwise
	// contribute nothing to Miswired, making an unstable resolver look better
	// than a consistently wrong one.
	MiswiredAnyRun int `json:"miswired_any_run"`
	// TestShadowBindingsAnyRun counts cases that bound a production call to a
	// test/mock definition in at least one repeat.
	TestShadowBindingsAnyRun int `json:"test_shadow_bindings_any_run"`

	// AmbiguousCases counts cases whose call name matches more than one
	// candidate definition. Unlike the counters above it is derived from the
	// persisted symbols rather than from a resolution choice, so it is stable
	// across runs and is the safest number to compare between commits.
	AmbiguousCases int `json:"ambiguous_cases"`
}

// EdgeObservation is one call edge as persisted, keyed by content only.
type EdgeObservation struct {
	SrcQualifiedName string `json:"src_qualified_name"`
	SrcFile          string `json:"src_file"`
	SrcLanguage      string `json:"src_language"`
	Line             int    `json:"line"`
	DstName          string `json:"dst_name"`
	Resolved         bool   `json:"resolved"`
	DstQualifiedName string `json:"dst_qualified_name,omitempty"`
	DstFile          string `json:"dst_file,omitempty"`
	DstLanguage      string `json:"dst_language,omitempty"`
	DstKind          string `json:"dst_kind,omitempty"`
	// TargetClassification is the P8 answer to "what is this destination":
	// `project` for a bound edge, otherwise `builtin`, `stdlib`, `external`, or
	// `unknown`. Read from the export record, not recomputed here -- the audit
	// measures the production classifier rather than a copy of its rules.
	//
	// It is reported, never asserted on. An unresolved edge that classifies as
	// `builtin` is still an unresolved edge, so the outcome vocabulary
	// (correctly_unresolved, miswired, ...) is unchanged by this field.
	TargetClassification string `json:"target_classification,omitempty"`
}

func (e EdgeObservation) key() string {
	return fmt.Sprintf("%s|%s|%d|%s", e.SrcFile, e.SrcQualifiedName, e.Line, e.DstName)
}

// target renders the selected destination as a stable string.
func (e EdgeObservation) target() string {
	if !e.Resolved {
		return "<unresolved>"
	}
	return fmt.Sprintf("%s (%s) %s", e.DstQualifiedName, e.DstLanguage, e.DstFile)
}

// CaseResult is the per-case audit outcome.
//
// Selected is populated only when every repeat selected the same destination.
// When the resolver's choice varies between runs, Selected is nil and
// SelectedVariants holds the sorted set of everything observed, so the report
// stays deterministic while still telling the truth about the resolver.
type CaseResult struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	Expectation      Expectation `json:"expectation"`
	Outcome          Outcome     `json:"outcome"`
	SrcFile          string      `json:"src_file"`
	SrcLanguage      string      `json:"src_language,omitempty"`
	DstName          string      `json:"dst_name"`
	CandidateCount   int         `json:"candidate_definitions"`
	CandidateTargets []string    `json:"candidate_definition_targets"`

	Selected         *EdgeObservation `json:"selected,omitempty"`
	SelectedStable   bool             `json:"selected_stable"`
	SelectedVariants []string         `json:"selected_variants,omitempty"`
	OutcomeVariants  []string         `json:"outcome_variants,omitempty"`

	// ShadowCandidates lists competing test/mock definitions that were present
	// in the graph for this case, whether or not one was selected. Since P7 a
	// production caller resolving to the production definition while shadow
	// candidates existed is the rule working, not luck; the list stays in the
	// report so the shadowing is still visible and TestShadowBinding below still
	// says whether one of them was bound.
	ShadowCandidates []string `json:"shadow_candidates,omitempty"`
	// TestShadowBinding and MiswiredAnyRun are true when the condition was seen
	// in at least one repeat, so an intermittent defect is never rounded away.
	TestShadowBinding bool   `json:"test_shadow_binding"`
	MiswiredAnyRun    bool   `json:"miswired_any_run"`
	Note              string `json:"note,omitempty"`

	// TargetClassification is the P8 classification every repeat agreed on;
	// TargetClassificationVariants holds the sorted set instead when they did
	// not. Classification is a pure function of persisted evidence, so variance
	// here means the underlying graph varied, and it is reported as a divergence
	// rather than folded away.
	//
	// Reported, not asserted: this field never participates in Outcome.
	TargetClassification         string   `json:"target_classification,omitempty"`
	TargetClassificationVariants []string `json:"target_classification_variants,omitempty"`
}

// Determinism reports whether repeated measurement agreed.
//
// This is a sampled property, not a proof. Indexing runs on a worker pool and
// symbol row ids follow worker completion order, so any resolver path that
// still selects by row order can vary between identical runs. Implicit
// resolution no longer breaks ties between equally valid same-language
// candidates, which removes the known source of that variance -- but a case can
// still look stable across N repeats and differ on the next one. Stable == true
// means "no variance observed in this sample".
type Determinism struct {
	Repeats               int      `json:"repeats"`
	Sampled               bool     `json:"sampled"`
	MetricsStable         bool     `json:"metrics_stable"`
	CaseOutcomesStable    bool     `json:"case_outcomes_stable"`
	SelectedTargetsStable bool     `json:"selected_targets_stable"`
	Divergences           []string `json:"divergences,omitempty"`
	Note                  string   `json:"note"`
}

// Projection is the subset of the report that is deterministic by
// construction: it is derived from the fixture and from the persisted symbol
// set, never from a resolution tie-break. Two invocations on the same checkout
// must produce an identical projection; comparing projections across commits is
// the safe way to track progress.
type Projection struct {
	FixtureVersion string `json:"fixture_version"`
	// CallEdgesInspected counts persisted call edges, which does not depend on
	// how any of them resolved. Resolved/unresolved counts are deliberately
	// excluded: the incremental path already flips an ambiguous name between the
	// two, so they are not safe to treat as invariant.
	CallEdgesInspected int              `json:"call_edges_inspected"`
	AmbiguousCases     int              `json:"ambiguous_cases"`
	Cases              []ProjectionCase `json:"cases"`
}

// ProjectionCase is the stable per-case facts.
type ProjectionCase struct {
	ID               string      `json:"id"`
	Expectation      Expectation `json:"expectation"`
	SrcFile          string      `json:"src_file"`
	SrcLanguage      string      `json:"src_language,omitempty"`
	DstName          string      `json:"dst_name"`
	CandidateCount   int         `json:"candidate_definitions"`
	CandidateTargets []string    `json:"candidate_definition_targets,omitempty"`
	ShadowCandidates []string    `json:"shadow_candidates,omitempty"`
	EdgeProduced     bool        `json:"call_edge_produced"`
}

// StableProjection extracts the deterministic-by-construction facts.
func (r *Report) StableProjection() Projection {
	p := Projection{
		FixtureVersion:     r.Fixture.Version,
		CallEdgesInspected: r.Metrics.CallEdgesInspected,
		AmbiguousCases:     r.Metrics.AmbiguousCases,
	}
	for _, c := range r.Cases {
		p.Cases = append(p.Cases, ProjectionCase{
			ID:               c.ID,
			Expectation:      c.Expectation,
			SrcFile:          c.SrcFile,
			SrcLanguage:      c.SrcLanguage,
			DstName:          c.DstName,
			CandidateCount:   c.CandidateCount,
			CandidateTargets: c.CandidateTargets,
			ShadowCandidates: c.ShadowCandidates,
			EdgeProduced:     c.Outcome != OutcomeNoEdge,
		})
	}
	return p
}

// Incremental compares the full-index resolution path against the incremental
// (update) path, which uses a different resolver entrypoint.
type Incremental struct {
	FullResolveMode        string   `json:"full_resolve_mode"`
	IncrementalResolveMode string   `json:"incremental_resolve_mode"`
	Divergences            []string `json:"divergences,omitempty"`
	Agrees                 bool     `json:"agrees"`
}

// runObservation is one measurement pass.
type runObservation struct {
	edges       []EdgeObservation
	metrics     Metrics
	resolveMode string
	// candidates maps a candidate call name to the sorted set of definitions.
	candidates map[string][]string
	symbols    map[int64]symbolInfo
	// languages is the sorted set of languages actually indexed, so the report
	// states what was measured rather than what the fixture intended.
	languages []string
}

// newRegistry builds the parser registry the harness measures with.
//
// It is pinned explicitly rather than reusing the CLI's default registry: that
// one swaps every adapter based on the cgo build tag, which would silently
// change the fixture's parsed symbols and therefore the measured numbers. The
// adapters used here compile identically with and without cgo.
func newRegistry() *parser.Registry {
	return parser.NewRegistry(
		golang.New(),
		python.New(),
		heuristic.NewJava(),
		heuristic.NewKotlin(),
		heuristic.NewRust(),
		heuristic.NewSwift(),
		heuristic.NewPHP(),
		heuristic.NewCSharp(),
		heuristic.NewRuby(),
		heuristic.NewTypeScriptJavaScript(),
		heuristic.NewCAndCpp(),
	)
}

func adapterNames() []string {
	return []string{
		"go(ast)", "python(regex)",
		"java(heuristic)", "kotlin(heuristic)", "rust(heuristic)", "swift(heuristic)",
		"php(heuristic)", "csharp(heuristic)", "ruby(heuristic)",
		"typescript(heuristic)", "cpp(heuristic)",
	}
}

// Run measures the fixture and returns the report. CreatedAt, Command and
// Context.CommitSHA are left to the caller so that library output stays free of
// ambient state.
func Run(ctx context.Context, opts Options) (*Report, error) {
	repeats := opts.Repeats
	if repeats < 1 {
		repeats = DefaultRepeats
	}

	runs := make([]runObservation, 0, repeats)
	for i := 0; i < repeats; i++ {
		obs, err := measureOnce(ctx, opts, i, false)
		if err != nil {
			return nil, fmt.Errorf("audit run %d: %w", i+1, err)
		}
		runs = append(runs, obs)
	}

	report := &Report{
		Schema: SchemaID,
		Context: RunContext{
			GoVersion:    runtime.Version(),
			GOOS:         runtime.GOOS,
			GOARCH:       runtime.GOARCH,
			NumCPU:       runtime.NumCPU(),
			SQLiteDriver: store.SQLiteDriverName(),
			Adapters:     adapterNames(),
		},
		Fixture: FixtureInfo{
			Version:   FixtureVersion,
			Cases:     len(Cases()),
			Files:     len(FixtureFilePaths()),
			Languages: runs[0].languages,
			// Verified against the fixture sources rather than asserted: the
			// "cross-language edges are wrong here" premise depends on it.
			FFIDeclared: FixtureDeclaresFFI(),
		},
	}

	report.Cases, report.Determinism = classify(runs)
	report.Metrics = aggregate(runs, report.Cases)
	report.Determinism.Repeats = repeats
	report.Unclassified = unclassified(runs[0])
	report.Findings = findings(report)

	if opts.IncludeIncrementalParity {
		inc, err := measureIncrementalParity(ctx, opts, runs[0])
		if err != nil {
			return nil, err
		}
		report.Incremental = inc
	}
	return report, nil
}

// measureOnce writes a fresh fixture into a fresh directory, indexes it into a
// fresh database, and reads the persisted graph back.
func measureOnce(ctx context.Context, opts Options, seq int, incremental bool) (runObservation, error) {
	var zero runObservation

	base := opts.WorkDir
	if base == "" {
		dir, err := os.MkdirTemp("", "codegraph-audit-")
		if err != nil {
			return zero, err
		}
		defer os.RemoveAll(dir)
		base = dir
	}
	root := filepath.Join(base, fmt.Sprintf("run%02d", seq))
	repoRoot := filepath.Join(root, "repo")
	dbDir := filepath.Join(root, "db")
	// Every pass must start from an empty tree and an empty database. With an
	// explicit WorkDir the directory survives between invocations, and reusing
	// it would index an unchanged tree over an existing graph and measure a
	// no-op.
	if err := os.RemoveAll(root); err != nil {
		return zero, err
	}
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return zero, err
	}
	if err := WriteFixture(repoRoot); err != nil {
		return zero, err
	}

	st, err := store.Open(filepath.Join(dbDir, "codegraph.sqlite"))
	if err != nil {
		return zero, err
	}
	defer st.Close()

	idx := indexer.New(st, newRegistry(), nil)
	summary, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "index"})
	if err != nil {
		return zero, err
	}
	resolveMode := summary.ResolveMode

	if incremental {
		// Touch one caller and re-run through the update path, which uses a
		// different resolver entrypoint than a full index.
		//
		// The appended text must be a comment: adding a second call would
		// create another edge with the same (src file, dst name) key as case C
		// and the parity check would then measure the injected edge instead of
		// the case's own.
		touched := filepath.Join(repoRoot, filepath.FromSlash("src/py/caller_c.py"))
		content, readErr := os.ReadFile(touched)
		if readErr != nil {
			return zero, readErr
		}
		if err := os.WriteFile(touched, append(content, []byte("\n# audit: touched to force an incremental update\n")...), 0o644); err != nil {
			return zero, err
		}
		updateSummary, updateErr := idx.Update(ctx, indexer.Options{RepoRoot: repoRoot})
		if updateErr != nil {
			return zero, updateErr
		}
		resolveMode = updateSummary.ResolveMode
	}

	repo, ok, err := st.PrimaryRepo(ctx)
	if err != nil {
		return zero, err
	}
	if !ok {
		return zero, fmt.Errorf("audit: no repo recorded after indexing")
	}

	symbols, err := loadSymbols(ctx, st, repo.ID)
	if err != nil {
		return zero, err
	}
	edges, err := loadCallEdges(ctx, st, repo.ID, symbols)
	if err != nil {
		return zero, err
	}

	langSet := map[string]struct{}{}
	for _, sym := range symbols {
		if sym.Language != "" {
			langSet[sym.Language] = struct{}{}
		}
	}

	obs := runObservation{
		edges:       edges,
		resolveMode: resolveMode,
		candidates:  candidateIndex(symbols),
		symbols:     symbols,
		languages:   sortedKeys(langSet),
	}
	// For the incremental pass these aggregate counters describe a touched
	// tree, so only the per-case destinations are compared; see
	// measureIncrementalParity.
	obs.metrics = edgeMetrics(edges)
	return obs, nil
}

type symbolInfo struct {
	QualifiedName string
	Name          string
	Language      string
	Kind          string
	File          string
}

func loadSymbols(ctx context.Context, st *store.Store, repoID int64) (map[int64]symbolInfo, error) {
	out := map[int64]symbolInfo{}
	const page = 500
	for offset := 0; ; offset += page {
		batch, err := st.ExportSymbolsPage(ctx, repoID, page, offset)
		if err != nil {
			return nil, err
		}
		for _, sym := range batch {
			out[sym.ID] = symbolInfo{
				QualifiedName: sym.QualifiedName,
				Name:          sym.Name,
				Language:      sym.Language,
				Kind:          sym.Kind,
				File:          filepath.ToSlash(sym.FilePath),
			}
		}
		if len(batch) < page {
			return out, nil
		}
	}
}

func loadCallEdges(ctx context.Context, st *store.Store, repoID int64, symbols map[int64]symbolInfo) ([]EdgeObservation, error) {
	var out []EdgeObservation
	const page = 500
	for offset := 0; ; offset += page {
		batch, err := st.ExportEdgesPage(ctx, repoID, page, offset)
		if err != nil {
			return nil, err
		}
		for _, edge := range batch {
			if edge.Kind != "calls" {
				continue
			}
			obs := EdgeObservation{
				SrcQualifiedName:     edge.SrcQualifiedName,
				SrcFile:              filepath.ToSlash(edge.FilePath),
				Line:                 edge.Line,
				DstName:              edge.DstName,
				TargetClassification: edge.TargetClassification,
			}
			if src, ok := symbols[edge.SrcSymbolID]; ok {
				obs.SrcLanguage = src.Language
			}
			if edge.DstSymbolID != nil {
				obs.Resolved = true
				obs.DstQualifiedName = edge.DstQualifiedName
				if dst, ok := symbols[*edge.DstSymbolID]; ok {
					obs.DstLanguage = dst.Language
					obs.DstKind = dst.Kind
					obs.DstFile = dst.File
					if obs.DstQualifiedName == "" {
						obs.DstQualifiedName = dst.QualifiedName
					}
				}
			}
			out = append(out, obs)
		}
		if len(batch) < page {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

// candidateIndex maps a candidate call name to the sorted set of definitions
// the resolver could select for it, so a case can report how much ambiguity the
// resolver faced.
//
// The keys mirror every match the resolver can make: exact qualified name,
// bare name, and the persisted suffix keys. No kind filter is applied even
// though one resolution strategy has one, because the suffix strategies and the
// incremental path do not filter by kind - a narrower model here would
// under-report ambiguity, which is the failure mode that matters.
func candidateIndex(symbols map[int64]symbolInfo) map[string][]string {
	byName := map[string]map[string]struct{}{}
	add := func(key string, sym symbolInfo) {
		if key == "" {
			return
		}
		if byName[key] == nil {
			byName[key] = map[string]struct{}{}
		}
		byName[key][fmt.Sprintf("%s (%s) %s", sym.QualifiedName, sym.Language, sym.File)] = struct{}{}
	}
	for _, sym := range symbols {
		add(sym.Name, sym)
		add(sym.QualifiedName, sym)
		add(qualifiedSuffix(sym.QualifiedName), sym)
		add(dotTail2(sym.QualifiedName), sym)
		add(dotTail3(sym.QualifiedName), sym)
	}
	out := make(map[string][]string, len(byName))
	for name, set := range byName {
		out[name] = sortedKeys(set)
	}
	return out
}

// candidatesFor returns the candidate definitions for a call name, including
// the resolver's generic `qualified_name LIKE '%.' || dst_name` fallback, which
// no persisted key covers.
func candidatesFor(candidates map[string][]string, symbols map[int64]symbolInfo, dstName string) []string {
	set := map[string]struct{}{}
	for _, cand := range candidates[dstName] {
		set[cand] = struct{}{}
	}
	for _, sym := range symbols {
		if strings.HasSuffix(sym.QualifiedName, "."+dstName) {
			set[fmt.Sprintf("%s (%s) %s", sym.QualifiedName, sym.Language, sym.File)] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// The three helpers below mirror internal/store/store.go's qualifiedSuffix,
// dotTail2 and dotTail3 exactly. They exist so the harness models the same
// candidate set the resolver sees; they read persisted data only and change no
// resolution behaviour. If the store's versions change, these must follow.
func qualifiedSuffix(qname string) string {
	if i := strings.LastIndexByte(qname, '/'); i >= 0 && i+1 < len(qname) {
		return qname[i+1:]
	}
	return ""
}

func dotTail2(qname string) string {
	afterSlash := qualifiedSuffix(qname)
	if afterSlash == "" {
		afterSlash = qname
	}
	lastDot := strings.LastIndexByte(afterSlash, '.')
	if lastDot < 0 || lastDot+1 >= len(afterSlash) {
		return ""
	}
	start := 0
	if prevDot := strings.LastIndexByte(afterSlash[:lastDot], '.'); prevDot >= 0 {
		start = prevDot + 1
	}
	return afterSlash[start:]
}

func dotTail3(qname string) string {
	afterSlash := qualifiedSuffix(qname)
	if afterSlash == "" {
		afterSlash = qname
	}
	dots := strings.Count(afterSlash, ".")
	if dots < 3 {
		return ""
	}
	rest := afterSlash
	for i := 0; i < dots-2; i++ {
		idx := strings.IndexByte(rest, '.')
		if idx < 0 {
			return ""
		}
		rest = rest[idx+1:]
	}
	return rest
}

func edgeMetrics(edges []EdgeObservation) Metrics {
	m := Metrics{CallEdgesInspected: len(edges)}
	for _, edge := range edges {
		if !edge.Resolved {
			m.Unresolved++
			continue
		}
		m.Resolved++
		switch {
		case edge.SrcLanguage == "" || edge.DstLanguage == "":
			// Language missing on one side. This is where a cross-language
			// miswire would hide, so it gets its own counter rather than
			// vanishing between the two buckets.
			m.UnknownLanguageResolved++
		case edge.SrcLanguage == edge.DstLanguage:
			m.SameLanguageResolved++
		default:
			m.CrossLanguageResolved++
		}
	}
	return m
}

// classify folds every repeat into per-case results plus a determinism verdict.
func classify(runs []runObservation) ([]CaseResult, Determinism) {
	det := Determinism{
		Sampled:               len(runs) > 1,
		MetricsStable:         true,
		CaseOutcomesStable:    true,
		SelectedTargetsStable: true,
		Note:                  "sampled over the repeats below; stability here is an observation, not a proof. Implicit strategies no longer tie-break between equally valid same-language candidates, so a destination that varies across identical runs points at a resolver path that still selects by row or insertion order",
	}
	if !det.Sampled {
		det.Note = "single run: variance was not sampled, so the stability flags below mean nothing was compared"
	}

	for i := 1; i < len(runs); i++ {
		if runs[i].metrics != runs[0].metrics {
			det.MetricsStable = false
			det.Divergences = append(det.Divergences, fmt.Sprintf(
				"aggregate metrics differ between run 1 and run %d: %+v vs %+v", i+1, runs[0].metrics, runs[i].metrics))
		}
		// The candidate model and the set of unclaimed edges are derived from
		// persisted symbols, not from a tie-break, so they are expected to be
		// run-invariant. Verify that instead of assuming it.
		if !sameStrings(unclassifiedKeys(runs[i]), unclassifiedKeys(runs[0])) {
			det.MetricsStable = false
			det.Divergences = append(det.Divergences, fmt.Sprintf(
				"the set of call edges belonging to no declared case differs between run 1 and run %d", i+1))
		}
	}

	results := make([]CaseResult, 0, len(Cases()))
	for _, c := range Cases() {
		res := CaseResult{
			ID:          c.ID,
			Title:       c.Title,
			Expectation: c.Expect,
			SrcFile:     c.SrcFile,
			DstName:     c.DstName,
			Note:        c.Note,
		}

		targets := map[string]struct{}{}
		outcomes := map[Outcome]struct{}{}
		classifications := map[string]struct{}{}
		var sample *EdgeObservation
		shadow := false

		for _, run := range runs {
			edge := findCaseEdge(run, c)
			outcome := caseOutcome(c, edge)
			outcomes[outcome] = struct{}{}
			if outcome == OutcomeMiswired {
				res.MiswiredAnyRun = true
			}
			if edge == nil {
				targets["<no call edge>"] = struct{}{}
				continue
			}
			targets[edge.target()] = struct{}{}
			classifications[edge.TargetClassification] = struct{}{}
			// Prefer run 0 so the reported destination and the candidate set
			// below describe the same measurement pass.
			if sample == nil {
				copyEdge := *edge
				sample = &copyEdge
				res.SrcLanguage = edge.SrcLanguage
			}
			if edge.Resolved && isShadowTarget(c, edge) {
				shadow = true
			}
		}

		cands := candidatesFor(runs[0].candidates, runs[0].symbols, c.DstName)
		res.CandidateCount = len(cands)
		res.CandidateTargets = cands
		for _, cand := range cands {
			if candidateIsShadow(c, cand) {
				res.ShadowCandidates = append(res.ShadowCandidates, cand)
			}
		}
		sort.Strings(res.ShadowCandidates)

		// The harness's candidate model must cover whatever the resolver chose.
		// If it does not, the ambiguity and shadow numbers understate reality
		// for exactly the edge that was miswired, so say so loudly.
		for target := range targets {
			if target == "<unresolved>" || target == "<no call edge>" {
				continue
			}
			if !containsString(cands, target) {
				det.Divergences = append(det.Divergences, fmt.Sprintf(
					"case %s selected %s, which the harness candidate model does not cover; ambiguity counts understate reality", c.ID, target))
			}
		}

		switch len(classifications) {
		case 0:
			// No call edge in any run; nothing to classify.
		case 1:
			for value := range classifications {
				res.TargetClassification = value
			}
		default:
			// Recorded on the case, deliberately NOT appended to
			// det.Divergences. The determinism block is about resolver
			// stability, and its flags gate the "no divergences while stable"
			// invariant; a classification-only variance would fail that
			// invariant under an unrelated message. Reported, never asserted --
			// render.go surfaces the variant set.
			res.TargetClassificationVariants = sortedKeys(classifications)
		}

		res.TestShadowBinding = shadow
		res.SelectedStable = len(targets) == 1
		if !res.SelectedStable {
			det.SelectedTargetsStable = false
			res.SelectedVariants = sortedKeys(targets)
			det.Divergences = append(det.Divergences, fmt.Sprintf(
				"case %s selected different destinations across runs: %s", c.ID, strings.Join(res.SelectedVariants, " | ")))
		} else if sample != nil {
			res.Selected = sample
		}

		if len(outcomes) == 1 {
			for outcome := range outcomes {
				res.Outcome = outcome
			}
		} else {
			res.Outcome = OutcomeNondeterministic
			det.CaseOutcomesStable = false
			variants := make([]string, 0, len(outcomes))
			for outcome := range outcomes {
				variants = append(variants, string(outcome))
			}
			sort.Strings(variants)
			res.OutcomeVariants = variants
			det.Divergences = append(det.Divergences, fmt.Sprintf(
				"case %s classified differently across runs: %s", c.ID, strings.Join(variants, " | ")))
		}
		results = append(results, res)
	}
	sort.Strings(det.Divergences)
	return results, det
}

// findCaseEdge returns the call edge belonging to a case, or nil. A case is
// keyed by (source file, dst name); if a case ever matches more than one edge
// the first in sorted order is used, and the fixture-integrity test asserts
// the one-edge-per-case invariant.
func findCaseEdge(run runObservation, c Case) *EdgeObservation {
	for i := range run.edges {
		if run.edges[i].SrcFile == c.SrcFile && run.edges[i].DstName == c.DstName {
			return &run.edges[i]
		}
	}
	return nil
}

// caseOutcome is the classification rule. It reads the fixture's declared
// expectation and the observed edge; it never asserts that current resolver
// behaviour is desirable.
func caseOutcome(c Case, edge *EdgeObservation) Outcome {
	if edge == nil {
		return OutcomeNoEdge
	}
	switch c.Expect {
	case ExpectInvalid:
		if edge.Resolved {
			return OutcomeMiswired
		}
		return OutcomeCorrectlyUnresolved
	case ExpectUnresolved:
		if edge.Resolved {
			return OutcomeMiswired
		}
		return OutcomeExpectedUnresolved
	case ExpectAmbiguous:
		// Any binding is a miswire here, including one that happens to land on
		// the production definition: the fixture declares that no evidence
		// distinguishes the candidates, so a selection can only have come from
		// insertion order or another non-semantic tie-break.
		if edge.Resolved {
			return OutcomeMiswired
		}
		return OutcomeExpectedUnresolved
	case ExpectValid:
		if !edge.Resolved {
			return OutcomeMissingEdge
		}
		if matchesAny(edge.DstQualifiedName, c.ValidTargets) {
			return OutcomeExpectedValid
		}
		return OutcomeMiswired
	}
	return OutcomeNondeterministic
}

// isShadowTarget reports whether a selected destination is a test, mock or
// fixture definition, either because the case declared it or because the
// destination file is in a test location.
func isShadowTarget(c Case, edge *EdgeObservation) bool {
	return matchesAny(edge.DstQualifiedName, c.ShadowTargets) || looksLikeTestPath(edge.DstFile)
}

// candidateIsShadow applies the same rule to a rendered candidate string,
// which has the form "qualified (language) file".
func candidateIsShadow(c Case, candidate string) bool {
	for _, shadowTarget := range c.ShadowTargets {
		if strings.HasPrefix(candidate, shadowTarget+" (") {
			return true
		}
	}
	if i := strings.LastIndex(candidate, ") "); i >= 0 {
		return looksLikeTestPath(candidate[i+2:])
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unclassifiedKeys(run runObservation) []string {
	out := []string{}
	for _, edge := range unclassified(run) {
		out = append(out, edge.key())
	}
	sort.Strings(out)
	return out
}

func matchesAny(qualified string, targets []string) bool {
	for _, target := range targets {
		if qualified == target {
			return true
		}
	}
	return false
}

// aggregate combines edge counters with case classifications.
func aggregate(runs []runObservation, cases []CaseResult) Metrics {
	m := runs[0].metrics
	m.Cases = len(cases)
	for _, c := range cases {
		switch c.Outcome {
		case OutcomeExpectedValid:
			m.ExpectedValid++
		case OutcomeExpectedUnresolved:
			m.ExpectedUnresolved++
		case OutcomeCorrectlyUnresolved:
			m.CorrectlyUnresolved++
		case OutcomeMiswired:
			m.Miswired++
		case OutcomeMissingEdge:
			m.MissingEdge++
		case OutcomeNoEdge:
			m.NoCallEdge++
		case OutcomeNondeterministic:
			m.Nondeterministic++
		}
		if c.TestShadowBinding {
			m.TestShadowBindingsAnyRun++
		}
		if c.MiswiredAnyRun {
			m.MiswiredAnyRun++
		}
		if c.CandidateCount > 1 {
			m.AmbiguousCases++
		}
	}
	return m
}

// unclassified lists call edges that belong to no declared case, so fixture
// drift or parser noise is visible instead of silently inflating counters.
func unclassified(run runObservation) []EdgeObservation {
	claimed := map[string]struct{}{}
	for _, c := range Cases() {
		claimed[c.SrcFile+"|"+c.DstName] = struct{}{}
	}
	out := []EdgeObservation{}
	for _, edge := range run.edges {
		if _, ok := claimed[edge.SrcFile+"|"+edge.DstName]; !ok {
			out = append(out, edge)
		}
	}
	return out
}

func findings(r *Report) []string {
	var out []string
	if r.Metrics.MiswiredAnyRun > 0 {
		out = append(out, fmt.Sprintf("%d of %d declared cases resolved to a destination the fixture declares invalid in at least one run (%d in every run)",
			r.Metrics.MiswiredAnyRun, r.Metrics.Cases, r.Metrics.Miswired))
	}
	if r.Metrics.CrossLanguageResolved > 0 {
		out = append(out, fmt.Sprintf("%d call edges resolved across a language boundary although the fixture declares no FFI", r.Metrics.CrossLanguageResolved))
	}
	if r.Metrics.UnknownLanguageResolved > 0 {
		out = append(out, fmt.Sprintf("%d resolved call edges had no language on one side, so they could not be classified as same- or cross-language", r.Metrics.UnknownLanguageResolved))
	}
	if r.Metrics.TestShadowBindingsAnyRun > 0 {
		out = append(out, fmt.Sprintf("%d case(s) bound a production call to a test/mock definition in at least one run", r.Metrics.TestShadowBindingsAnyRun))
	}
	if len(r.Unclassified) > 0 {
		out = append(out, fmt.Sprintf("%d call edges belong to no declared case; the fixture has drifted and the counters above cover more than the cases", len(r.Unclassified)))
	}
	if !r.Determinism.Sampled {
		out = append(out, "measured with a single run: nondeterminism was not sampled, so the stability flags are not evidence")
	}
	for _, c := range r.Cases {
		// P7 gives *production* callers evidence that separates a production
		// definition from a test one, so a production case resolving while shadow
		// candidates existed is the rule working and is not reported. A call from
		// a test file gets no such preference, so a test caller that resolved
		// while several test/mock definitions competed still rests on something
		// other than evidence and is still worth saying out loud.
		if !store.IsTestFilePath(c.SrcFile) {
			continue
		}
		resolvedSomewhere := (c.Selected != nil && c.Selected.Resolved) || !c.SelectedStable
		if len(c.ShadowCandidates) > 0 && !c.TestShadowBinding && resolvedSomewhere {
			out = append(out, fmt.Sprintf(
				"case %s is called from a test file and resolved while %d competing test definition(s) existed; test callers get no production preference, so this outcome does not rest on the test-shadow rule",
				c.ID, len(c.ShadowCandidates)))
		}
	}
	if r.Metrics.MissingEdge > 0 {
		out = append(out, fmt.Sprintf("%d legitimate same-language call edges were left unresolved", r.Metrics.MissingEdge))
	}
	if r.Metrics.NoCallEdge > 0 {
		out = append(out, fmt.Sprintf("%d cases produced no call edge at all; the fixture no longer exercises them", r.Metrics.NoCallEdge))
	}
	if r.Metrics.AmbiguousCases > 0 {
		out = append(out, fmt.Sprintf(
			"%d cases had more than one candidate definition; each such case must either resolve on evidence that singles one out or stay unresolved, never pick by insertion order",
			r.Metrics.AmbiguousCases))
	}
	if !r.Determinism.SelectedTargetsStable {
		out = append(out, "resolver destination selection varied across identical index runs within this sample")
	}
	if !r.Determinism.MetricsStable {
		out = append(out, "aggregate edge counters are not stable across identical index runs")
	}
	if !r.Determinism.CaseOutcomesStable {
		out = append(out, "case classification is not stable across identical index runs")
	}
	if r.Incremental != nil && !r.Incremental.Agrees {
		out = append(out, "the incremental update path resolves differently than a full index of the same tree")
	}
	if len(out) == 0 {
		out = []string{"no miswired edges observed"}
	}
	return out
}

// measureIncrementalParity re-measures via the update path and diffs per-case
// destinations against the full-index observation.
func measureIncrementalParity(ctx context.Context, opts Options, full runObservation) (*Incremental, error) {
	inc, err := measureOnce(ctx, opts, 90, true)
	if err != nil {
		return nil, fmt.Errorf("audit incremental parity: %w", err)
	}
	out := &Incremental{
		FullResolveMode:        full.resolveMode,
		IncrementalResolveMode: inc.resolveMode,
	}
	for _, c := range Cases() {
		fullEdge := findCaseEdge(full, c)
		incEdge := findCaseEdge(inc, c)
		fullTarget, incTarget := "<no call edge>", "<no call edge>"
		if fullEdge != nil {
			fullTarget = fullEdge.target()
		}
		if incEdge != nil {
			incTarget = incEdge.target()
		}
		fullResolved := fullEdge != nil && fullEdge.Resolved
		incResolved := incEdge != nil && incEdge.Resolved
		switch {
		case fullResolved != incResolved:
			out.Divergences = append(out.Divergences, fmt.Sprintf(
				"resolvedness differs for case %s: full index -> %s, incremental update -> %s", c.ID, fullTarget, incTarget))
		case fullResolved && fullTarget != incTarget:
			// Both paths resolved but to different destinations. On the current
			// resolver this can also be plain tie-break noise, so it is labelled
			// separately from a resolvedness difference.
			out.Divergences = append(out.Divergences, fmt.Sprintf(
				"destination differs for case %s (may be tie-break noise): full index -> %s, incremental update -> %s", c.ID, fullTarget, incTarget))
		}
	}
	sort.Strings(out.Divergences)
	out.Agrees = len(out.Divergences) == 0
	return out, nil
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
