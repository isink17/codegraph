// Command retrievalbench measures CodeGraph's semantic retrieval quality
// against a frozen, source-grounded query set.
//
// It is a developer tool, not a CLI or MCP surface: it adds no command to the
// shipped binary and changes no public response. Everything it does is
// read-only, deterministic and free of model inference -- no Claude, no LLM
// API, no embedding service. Two runs of the same dataset against the same
// indexed corpora produce the same ranked identities and the same metrics;
// the latency samples differ, and so does the recorded binary path and digest
// when the tool is rebuilt (go run builds a fresh temporary binary each time).
//
// It resolves a corpus index at <root>/.codegraph/<RepoDatabaseFileName>, the
// repo-local layout only. A graph kept in the global artifacts directory is
// reported as "no index at", not searched for.
//
// It reports two stages separately. The search stage is Store.SemanticSearch's
// own candidate set, and the context stage is query.Service.ContextForTask's
// final selection. Keeping them apart is what lets a missing file be blamed on
// ranking only when it was a candidate in the first place.
//
//	go run ./tools/retrievalbench \
//	  -dataset tools/retrievalbench/datasets/v1.json \
//	  -corpora-root /path/to/corpora -out report.json
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/latency"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/tokenest"
)

// ReportSchema identifies the output format.
const ReportSchema = "codegraph.retrieval_bench/v1"

// Stage names.
const (
	StageSearch  = "semantic_search"
	StageContext = "context_for_task"
)

// RunConfig is every knob that can move a number. It is part of the report so
// a comparison between two runs can refuse a configuration change instead of
// reading it as a scoring change.
type RunConfig struct {
	SearchLimit    int  `json:"search_limit"`
	MaxFiles       int  `json:"max_files"`
	MaxSymbols     int  `json:"max_symbols"`
	MaxTokens      int  `json:"max_tokens"`
	IncludeTests   bool `json:"include_tests"`
	IncludeCallers bool `json:"include_callers"`
	Runs           int  `json:"runs"`
	Warmup         int  `json:"warmup"`
	// SelectedQueries and DatasetQueries make a -only subset visible on the
	// face of the report. They also take part in the comparison refusal, so a
	// subset is never compared against a full run.
	SelectedQueries int `json:"selected_queries"`
	DatasetQueries  int `json:"dataset_queries"`
}

// LatencyReport keeps every measured sample, not only the percentiles.
type LatencyReport struct {
	Samples []float64 `json:"samples_ms"`
	P50MS   float64   `json:"p50_ms"`
	P95MS   float64   `json:"p95_ms"`
	MaxMS   float64   `json:"max_ms"`
}

// ReturnedSymbol is one ordered result identity.
type ReturnedSymbol struct {
	Rank          int     `json:"rank"`
	QualifiedName string  `json:"qualified_name"`
	File          string  `json:"file"`
	SymbolID      int64   `json:"symbol_id"`
	Score         float64 `json:"score,omitempty"`
	Relevance     string  `json:"relevance,omitempty"`
}

// StageResult is one stage's ordered output plus its scoring.
type StageResult struct {
	Stage           string           `json:"stage"`
	Files           []string         `json:"files"`
	Symbols         []ReturnedSymbol `json:"symbols"`
	ResponseBytes   int              `json:"response_bytes"`
	EstimatedTokens int              `json:"estimated_tokens"`
	Metrics         Metrics          `json:"metrics"`
	Latency         LatencyReport    `json:"latency"`
	// MissingAttribution is populated on the context stage only.
	MissingAttribution map[string]string `json:"missing_attribution,omitempty"`
}

// QueryResult is one frozen query's full outcome.
type QueryResult struct {
	ID       string      `json:"id"`
	Corpus   string      `json:"corpus"`
	Category string      `json:"category"`
	HeldOut  bool        `json:"held_out"`
	Truth    string      `json:"truth"`
	Search   StageResult `json:"search"`
	Context  StageResult `json:"context"`
}

// CorpusCheck records the identity validation of one corpus.
type CorpusCheck struct {
	Name     string `json:"name"`
	Root     string `json:"root"`
	Expected Corpus `json:"expected"`
	Observed struct {
		Commit     string `json:"commit"`
		Tree       string `json:"tree"`
		Files      int64  `json:"files"`
		Symbols    int64  `json:"symbols"`
		References int64  `json:"references"`
		Edges      int64  `json:"edges"`
	} `json:"observed"`
	Match bool `json:"match"`
}

// Report is the full machine-readable result.
type Report struct {
	Schema  string `json:"schema"`
	Dataset struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		Frozen string `json:"frozen"`
	} `json:"dataset"`
	Binary struct {
		Path      string `json:"path"`
		SHA256    string `json:"sha256"`
		GoVersion string `json:"go_version"`
		OSArch    string `json:"os_arch"`
	} `json:"binary"`
	Config  RunConfig     `json:"config"`
	Corpora []CorpusCheck `json:"corpora"`
	Queries []QueryResult `json:"queries"`
	Summary Summary       `json:"summary"`
}

// Summary rolls the per-query numbers up by stage and by category. Held-out
// queries are summarised separately and never folded into the tuning set.
type Summary struct {
	ByStage    map[string]Aggregate `json:"by_stage"`
	ByCategory map[string]Aggregate `json:"by_category_context_stage"`
	HeldOut    Aggregate            `json:"held_out_context_stage"`
	Tuning     Aggregate            `json:"tuning_context_stage"`
}

// Aggregate is the mean of the metrics that are defined for every member, plus
// counts for the ones that are not. A negative query has no recall and no
// reciprocal rank, so it is counted in Queries and in NegativeQueries but
// contributes to neither mean.
type Aggregate struct {
	Queries           int     `json:"queries"`
	ScoredQueries     int     `json:"queries_with_recall"`
	NegativeQueries   int     `json:"negative_queries"`
	MeanRecallAt5     float64 `json:"mean_recall_at_5"`
	MeanRecallAt10    float64 `json:"mean_recall_at_10"`
	MeanReciprocal    float64 `json:"mean_reciprocal_rank"`
	QueriesWithNoHit  int     `json:"queries_with_no_relevant_result"`
	ScoredPrecision   int     `json:"queries_with_precision"`
	MeanPrecisionAt10 float64 `json:"mean_precision_at_10_where_defined"`
	ScoredNDCG        int     `json:"queries_with_ndcg"`
	MeanNDCGAt10      float64 `json:"mean_ndcg_at_10_where_defined"`
	TestFiles         int     `json:"test_files_returned"`
	ProductionFiles   int     `json:"production_files_returned"`
	EstimatedTokens   int     `json:"estimated_tokens_total"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "retrievalbench:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("retrievalbench", flag.ContinueOnError)
	datasetPath := fs.String("dataset", "tools/retrievalbench/datasets/v1.json", "frozen dataset JSON")
	corporaRoot := fs.String("corpora-root", "", "directory holding one checked-out, indexed corpus per dataset corpus name")
	out := fs.String("out", "", "write the JSON report here instead of stdout")
	expectBinary := fs.String("expect-binary-sha256", "", "refuse to run unless this tool's own binary has this digest")
	only := fs.String("only", "", "comma-separated query ids to run")
	runs := fs.Int("runs", 10, "measured samples per stage per query")
	warmup := fs.Int("warmup", 2, "unmeasured samples per stage per query")
	maxTokens := fs.Int("max-tokens", 9000, "context_for_task token budget")
	compare := fs.String("compare", "", "after the run, emit a comparison against this earlier report instead of the report itself")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return nil
		}
		return err
	}
	if *corporaRoot == "" {
		return fmt.Errorf("-corpora-root is required")
	}
	if *runs < 1 {
		return fmt.Errorf("-runs must be >= 1, got %d", *runs)
	}
	if *warmup < 0 {
		return fmt.Errorf("-warmup must be >= 0, got %d", *warmup)
	}

	ds, err := LoadDataset(*datasetPath)
	if err != nil {
		return err
	}
	selected, err := selectQueries(ds, *only)
	if err != nil {
		return err
	}

	report := Report{Schema: ReportSchema}
	report.Dataset.ID = ds.ID
	report.Dataset.SHA256 = ds.SHA256
	report.Dataset.Frozen = ds.Frozen
	report.Binary.GoVersion = runtime.Version()
	report.Binary.OSArch = runtime.GOOS + "/" + runtime.GOARCH
	// A report whose own binary identity is unknown cannot be compared, and
	// this tool exists to refuse exactly that, so an unreadable executable is
	// a failure rather than an empty field.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this tool's own binary: %w", err)
	}
	report.Binary.Path = exe
	if report.Binary.SHA256, err = fileSHA256(exe); err != nil {
		return fmt.Errorf("digest this tool's own binary: %w", err)
	}
	if *expectBinary != "" && !strings.EqualFold(*expectBinary, report.Binary.SHA256) {
		return fmt.Errorf("binary identity mismatch: running %s, expected %s; refusing to produce comparable numbers from a different build",
			report.Binary.SHA256, *expectBinary)
	}
	report.Config = RunConfig{
		SearchLimit: 30, MaxFiles: 10, MaxSymbols: 30, MaxTokens: *maxTokens,
		IncludeTests: true, IncludeCallers: true, Runs: *runs, Warmup: *warmup,
		SelectedQueries: len(selected), DatasetQueries: len(ds.Queries),
	}

	ctx := context.Background()
	for _, name := range corporaUsedBy(ds, selected) {
		corpus, _ := ds.CorpusByName(name)
		root := filepath.Join(*corporaRoot, name)
		opened, check, err := openCorpus(ctx, corpus, root)
		if err != nil {
			return err
		}
		report.Corpora = append(report.Corpora, check)
		if !check.Match {
			opened.close()
			return fmt.Errorf("corpus %q identity mismatch at %s: expected commit %s tree %s and %d files / %d symbols / %d references / %d edges, observed commit %s tree %s and %d / %d / %d / %d; refusing to score against a different corpus",
				name, root, corpus.Commit, corpus.Tree, corpus.Files, corpus.Symbols, corpus.References, corpus.Edges,
				check.Observed.Commit, check.Observed.Tree, check.Observed.Files, check.Observed.Symbols,
				check.Observed.References, check.Observed.Edges)
		}
		if err := verifyLabelledFiles(ctx, opened, name, selected); err != nil {
			opened.close()
			return err
		}
		for _, q := range selected {
			if q.Corpus != name {
				continue
			}
			res, err := runQuery(ctx, opened, q, report.Config)
			if err != nil {
				opened.close()
				return fmt.Errorf("query %s: %w", q.ID, err)
			}
			report.Queries = append(report.Queries, res)
		}
		opened.close()
	}
	sort.Slice(report.Queries, func(i, j int) bool { return report.Queries[i].ID < report.Queries[j].ID })
	report.Summary = summarize(report.Queries)

	var payload any = report
	if *compare != "" {
		base, err := loadReport(*compare)
		if err != nil {
			return err
		}
		cmp, err := CompareReports(base, report)
		if err != nil {
			return err
		}
		payload = cmp
	}
	enc, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	enc = append(enc, '\n')
	if *out == "" {
		_, err = stdout.Write(enc)
		return err
	}
	return os.WriteFile(*out, enc, 0o644)
}

func selectQueries(ds *Dataset, only string) ([]Query, error) {
	if strings.TrimSpace(only) == "" {
		return ds.Queries, nil
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = true
		}
	}
	var out []Query
	for _, q := range ds.Queries {
		if want[q.ID] {
			out = append(out, q)
			delete(want, q.ID)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("unknown query id(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func corporaUsedBy(ds *Dataset, queries []Query) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range ds.CorpusNames() {
		for _, q := range queries {
			if q.Corpus == name && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// openedCorpus is one indexed corpus held open read-only for the run.
type openedCorpus struct {
	store  *store.Store
	svc    *query.Service
	repoID int64
}

func (o *openedCorpus) close() {
	if o != nil && o.store != nil {
		_ = o.store.Close()
	}
}

func openCorpus(ctx context.Context, corpus Corpus, root string) (*openedCorpus, CorpusCheck, error) {
	check := CorpusCheck{Name: corpus.Name, Root: root, Expected: corpus}
	dbPath := filepath.Join(root, ".codegraph", store.RepoDatabaseFileName)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, check, fmt.Errorf("corpus %q: no index at %s (run codegraph index %s --rebuild): %w", corpus.Name, dbPath, root, err)
	}
	s, err := store.OpenReadOnly(dbPath, store.OpenOptions{})
	if err != nil {
		return nil, check, fmt.Errorf("corpus %q: %w", corpus.Name, err)
	}
	repo, found, err := s.FindRepo(ctx, root)
	if err != nil {
		_ = s.Close()
		return nil, check, err
	}
	if !found {
		_ = s.Close()
		return nil, check, fmt.Errorf("corpus %q: %s holds no graph for %s", corpus.Name, dbPath, root)
	}
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		_ = s.Close()
		return nil, check, err
	}
	check.Observed.Commit = gitRevision(root, "HEAD")
	check.Observed.Tree = gitRevision(root, "HEAD^{tree}")
	check.Observed.Files = stats.Files
	check.Observed.Symbols = stats.Symbols
	check.Observed.References = stats.References
	check.Observed.Edges = stats.Edges
	check.Match = check.matches()
	return &openedCorpus{store: s, svc: query.New(s, nil), repoID: repo.ID}, check, nil
}

// matches reports whether the indexed corpus is the one the dataset labels
// were read from. Every field participates: a graph with the right revision
// but a different symbol count was produced by a different indexer and its
// ranks are not comparable, and an empty observed revision (no git, or not a
// checkout) can never equal a pinned one.
func (c CorpusCheck) matches() bool {
	return c.Observed.Commit == c.Expected.Commit &&
		c.Observed.Tree == c.Expected.Tree &&
		c.Observed.Files == c.Expected.Files &&
		c.Observed.Symbols == c.Expected.Symbols &&
		c.Observed.References == c.Expected.References &&
		c.Observed.Edges == c.Expected.Edges
}

// verifyLabelledFiles refuses a dataset whose labels name files this corpus
// does not hold.
//
// A mistyped relevant path can never match a returned file, so it sits in
// missing_relevant_files forever and silently caps recall below 1 for every
// run and every A/B that follows. That is a denominator the corpus does not
// support, which is the one thing this tool must not produce quietly. An
// irrelevant label is checked too: a verified-irrelevant file that is not in
// the index is evidence about something else.
func verifyLabelledFiles(ctx context.Context, c *openedCorpus, corpus string, queries []Query) error {
	indexed, err := c.indexedFiles(ctx)
	if err != nil {
		return fmt.Errorf("corpus %q: list indexed files: %w", corpus, err)
	}
	var bad []string
	for _, q := range queries {
		if q.Corpus != corpus {
			continue
		}
		for _, l := range append(append([]Label{}, q.Relevant...), q.Irrelevant...) {
			if !indexed[l.File] {
				bad = append(bad, q.ID+": "+l.File)
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("corpus %q: %d labelled file(s) are not in the index, so they can never be matched: %s",
			corpus, len(bad), strings.Join(bad, "; "))
	}
	return nil
}

// indexedFiles reads every live file path in the corpus, paging because
// ListFiles caps a page at the shared public page size.
func (o *openedCorpus) indexedFiles(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	const page = 500
	for offset := 0; ; offset += page {
		rows, err := o.store.ListFiles(ctx, o.repoID, "", page, offset)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if path, ok := r["path"].(string); ok {
				out[path] = true
			}
		}
		if len(rows) < page {
			return out, nil
		}
	}
}

// gitRevision resolves one revision in a corpus checkout. An empty string is
// an identity that cannot match a pinned non-empty one, so a checkout without
// git is refused by the caller rather than silently scored.
func gitRevision(root, rev string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", rev)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runQuery(ctx context.Context, c *openedCorpus, q Query, cfg RunConfig) (QueryResult, error) {
	res := QueryResult{ID: q.ID, Corpus: q.Corpus, Category: q.Category, HeldOut: q.HeldOut, Truth: q.Truth}

	searchCall := func() error {
		_, err := c.svc.SemanticSearch(ctx, c.repoID, q.QueryText, cfg.SearchLimit, 0)
		return err
	}
	ctxOpts := query.ContextForTaskOptions{
		MaxFiles: cfg.MaxFiles, MaxSymbols: cfg.MaxSymbols,
		IncludeTests: cfg.IncludeTests, IncludeCallers: cfg.IncludeCallers,
		MaxTokens: cfg.MaxTokens,
	}
	contextCall := func() error {
		_, err := c.svc.ContextForTask(ctx, c.repoID, q.QueryText, ctxOpts)
		return err
	}

	hits, err := c.svc.SemanticSearch(ctx, c.repoID, q.QueryText, cfg.SearchLimit, 0)
	if err != nil {
		return res, fmt.Errorf("semantic search: %w", err)
	}
	if res.Search, err = searchStageResult(q, hits); err != nil {
		return res, err
	}
	if res.Search.Latency, err = measure(searchCall, cfg); err != nil {
		return res, err
	}

	tc, err := c.svc.ContextForTask(ctx, c.repoID, q.QueryText, ctxOpts)
	if err != nil {
		return res, fmt.Errorf("context for task: %w", err)
	}
	if res.Context, err = contextStageResult(q, tc, res.Search.Files); err != nil {
		return res, err
	}
	if res.Context.Latency, err = measure(contextCall, cfg); err != nil {
		return res, err
	}
	return res, nil
}

func searchStageResult(q Query, hits []map[string]any) (StageResult, error) {
	out := StageResult{Stage: StageSearch, Files: []string{}, Symbols: []ReturnedSymbol{}}
	seenFile := map[string]bool{}
	symbols := map[string]bool{}
	for i, h := range hits {
		file, _ := h["file"].(string)
		name, _ := h["symbol"].(string)
		sym := ReturnedSymbol{Rank: i, QualifiedName: name, File: file}
		switch id := h["symbol_id"].(type) {
		case int64:
			sym.SymbolID = id
		case int:
			sym.SymbolID = int64(id)
		}
		if score, ok := h["score"].(float64); ok {
			sym.Score = score
		}
		out.Symbols = append(out.Symbols, sym)
		symbols[name] = true
		if file != "" && !seenFile[file] {
			seenFile[file] = true
			out.Files = append(out.Files, file)
		}
	}
	tokens, bytes, err := tokenest.OfJSON(hits)
	if err != nil {
		return out, fmt.Errorf("estimate search response size: %w", err)
	}
	out.EstimatedTokens, out.ResponseBytes = tokens, bytes
	out.Metrics = Evaluate(q, out.Files, symbols)
	return out, nil
}

func contextStageResult(q Query, tc *graph.TaskContext, searchFiles []string) (StageResult, error) {
	out := StageResult{Stage: StageContext, Files: []string{}, Symbols: []ReturnedSymbol{}}
	symbols := map[string]bool{}
	rank := 0
	// Production files first, then test files: that is the order the response
	// presents them in, and the order an agent reads them in.
	for _, group := range [][]graph.TaskContextFile{tc.Files, tc.TestFiles} {
		for _, f := range group {
			out.Files = append(out.Files, f.Path)
			for _, s := range f.Symbols {
				out.Symbols = append(out.Symbols, ReturnedSymbol{
					Rank: rank, QualifiedName: s.QualifiedName, File: f.Path,
					SymbolID: s.SymbolID, Score: f.RelevanceScore, Relevance: s.Relevance,
				})
				symbols[s.QualifiedName] = true
				rank++
			}
		}
	}
	tokens, bytes, err := tokenest.OfJSON(tc)
	if err != nil {
		return out, fmt.Errorf("estimate context response size: %w", err)
	}
	out.EstimatedTokens, out.ResponseBytes = tokens, bytes
	out.Metrics = Evaluate(q, out.Files, symbols)
	out.MissingAttribution = AttributeMissing(out.Metrics.MissingRelevantFiles, searchFiles)
	return out, nil
}

func measure(call func() error, cfg RunConfig) (LatencyReport, error) {
	for i := 0; i < cfg.Warmup; i++ {
		if err := call(); err != nil {
			return LatencyReport{}, err
		}
	}
	raw := make(latency.Samples, 0, cfg.Runs)
	samples := make([]float64, 0, cfg.Runs)
	for i := 0; i < cfg.Runs; i++ {
		start := time.Now()
		if err := call(); err != nil {
			return LatencyReport{}, err
		}
		d := time.Since(start)
		raw = append(raw, d)
		samples = append(samples, latency.Millis(d))
	}
	sorted := raw.Sorted()
	return LatencyReport{
		Samples: samples,
		P50MS:   latency.Millis(sorted.SortedPercentile(50)),
		P95MS:   latency.Millis(sorted.SortedPercentile(95)),
		MaxMS:   latency.Millis(sorted.Max()),
	}, nil
}

func summarize(queries []QueryResult) Summary {
	s := Summary{ByStage: map[string]Aggregate{}, ByCategory: map[string]Aggregate{}}
	var search, contextStage []Metrics
	var heldOut, tuning []Metrics
	byCategory := map[string][]Metrics{}
	searchTokens, contextTokens := 0, 0
	for _, q := range queries {
		search = append(search, q.Search.Metrics)
		contextStage = append(contextStage, q.Context.Metrics)
		searchTokens += q.Search.EstimatedTokens
		contextTokens += q.Context.EstimatedTokens
		byCategory[q.Category] = append(byCategory[q.Category], q.Context.Metrics)
		if q.HeldOut {
			heldOut = append(heldOut, q.Context.Metrics)
			continue
		}
		tuning = append(tuning, q.Context.Metrics)
	}
	s.ByStage[StageSearch] = aggregate(search, searchTokens)
	s.ByStage[StageContext] = aggregate(contextStage, contextTokens)
	for cat, ms := range byCategory {
		s.ByCategory[cat] = aggregate(ms, 0)
	}
	s.HeldOut = aggregate(heldOut, 0)
	s.Tuning = aggregate(tuning, 0)
	return s
}

func aggregate(ms []Metrics, tokens int) Aggregate {
	a := Aggregate{Queries: len(ms), EstimatedTokens: tokens}
	if len(ms) == 0 {
		return a
	}
	var r5, r10, rr, p10, ndcg float64
	for _, m := range ms {
		if m.RecallAt5 == nil {
			a.NegativeQueries++
		} else {
			a.ScoredQueries++
			r5 += *m.RecallAt5
			r10 += *m.RecallAt10
			if m.ReciprocalRank != nil {
				rr += *m.ReciprocalRank
			}
			if m.FirstRelevantRank == nil {
				a.QueriesWithNoHit++
			}
		}
		if m.PrecisionAt10 != nil {
			a.ScoredPrecision++
			p10 += *m.PrecisionAt10
		}
		if m.NDCGAt10 != nil {
			a.ScoredNDCG++
			ndcg += *m.NDCGAt10
		}
		a.TestFiles += m.TestFiles
		a.ProductionFiles += m.ProductionFiles
	}
	if a.ScoredQueries > 0 {
		n := float64(a.ScoredQueries)
		a.MeanRecallAt5 = r5 / n
		a.MeanRecallAt10 = r10 / n
		a.MeanReciprocal = rr / n
	}
	if a.ScoredPrecision > 0 {
		a.MeanPrecisionAt10 = p10 / float64(a.ScoredPrecision)
	}
	if a.ScoredNDCG > 0 {
		a.MeanNDCGAt10 = ndcg / float64(a.ScoredNDCG)
	}
	return a
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
