package query

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/tokenest"
)

const taskPayment = "process payment retry"

func fullOpts() ContextForTaskOptions {
	return ContextForTaskOptions{IncludeCallers: true, IncludeTests: true}
}

func mustContext(t *testing.T, fx *contextFixture, task string, opts ContextForTaskOptions) *graph.TaskContext {
	t.Helper()
	res, err := fx.svc.ContextForTask(context.Background(), fx.repoID, task, opts)
	if err != nil {
		t.Fatalf("ContextForTask(%q) error = %v", task, err)
	}
	return res
}

func allSymbols(res *graph.TaskContext) []graph.TaskContextSymbol {
	var out []graph.TaskContextSymbol
	for _, f := range res.Files {
		out = append(out, f.Symbols...)
	}
	for _, f := range res.TestFiles {
		out = append(out, f.Symbols...)
	}
	return out
}

func symbolKeys(res *graph.TaskContext) []string {
	syms := allSymbols(res)
	keys := make([]string, 0, len(syms))
	for _, s := range syms {
		keys = append(keys, s.StableKey+"|"+s.QualifiedName)
	}
	return keys
}

func TestContextForTaskReturnsIdentityForDrillDown(t *testing.T) {
	fx := newContextFixture(t)
	res := mustContext(t, fx, taskPayment, fullOpts())

	if len(res.Files) == 0 {
		t.Fatal("no files returned")
	}
	for _, sym := range allSymbols(res) {
		if sym.SymbolID == 0 {
			t.Fatalf("symbol %q carries no symbol_id", sym.QualifiedName)
		}
		if sym.StableKey == "" {
			t.Fatalf("symbol %q carries no stable_key", sym.QualifiedName)
		}
		if sym.QualifiedName == "" {
			t.Fatalf("symbol %q carries no qualified_name", sym.Name)
		}
	}
}

// The response is a selector: no field may carry source text.
func TestContextForTaskCarriesNoSource(t *testing.T) {
	fx := newContextFixture(t)
	res := mustContext(t, fx, taskPayment, fullOpts())
	payload, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, needle := range []string{"util.Log(", "return chargeCard", "package paymentsvc"} {
		if strings.Contains(string(payload), needle) {
			t.Fatalf("response embeds source (%q): %s", needle, payload)
		}
	}
}

func TestContextForTaskDefaultBudget(t *testing.T) {
	fx := newContextFixture(t)
	res := mustContext(t, fx, taskPayment, fullOpts())
	if res.MaxTokens != DefaultContextMaxTokens {
		t.Fatalf("MaxTokens = %d, want %d", res.MaxTokens, DefaultContextMaxTokens)
	}
	assertWithinBudget(t, res)
}

func TestContextForTaskExplicitBudgetIsRespected(t *testing.T) {
	fx := newContextFixture(t)
	for _, budget := range []int{120, 200, 300, 500, 1000, 4000} {
		opts := fullOpts()
		opts.MaxTokens = budget
		res, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts)
		if errors.Is(err, ErrContextBudgetTooSmall) {
			continue
		}
		if err != nil {
			t.Fatalf("ContextForTask(max_tokens=%d) error = %v", budget, err)
		}
		if res.MaxTokens != budget {
			t.Fatalf("MaxTokens = %d, want %d", res.MaxTokens, budget)
		}
		assertWithinBudget(t, res)
	}
}

// assertWithinBudget checks the two halves of the budget contract: the reported
// estimate matches the document actually serialized, and it fits max_tokens.
func assertWithinBudget(t *testing.T, res *graph.TaskContext) {
	t.Helper()
	measured, _, err := tokenest.OfJSON(res)
	if err != nil {
		t.Fatalf("OfJSON() error = %v", err)
	}
	if measured != res.EstimatedTokens {
		t.Fatalf("estimated_tokens = %d, serialized document measures %d", res.EstimatedTokens, measured)
	}
	if res.EstimatedTokens > res.MaxTokens {
		t.Fatalf("estimated_tokens = %d exceeds max_tokens = %d", res.EstimatedTokens, res.MaxTokens)
	}
}

func TestContextForTaskRejectsNegativeBudget(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = -1
	if _, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts); err == nil {
		t.Fatal("expected an error for max_tokens = -1")
	}
}

func TestContextForTaskClampsAbsurdBudget(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = 1_000_000_000
	res := mustContext(t, fx, taskPayment, opts)
	if res.MaxTokens != MaxContextMaxTokens {
		t.Fatalf("MaxTokens = %d, want the clamp %d", res.MaxTokens, MaxContextMaxTokens)
	}
}

func TestContextForTaskTinyBudgetFailsClosed(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = 1
	_, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts)
	if !errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("error = %v, want ErrContextBudgetTooSmall", err)
	}
}

// A budget that pays for the envelope but not for one symbol must fail rather
// than return an empty page that advertises more.
func TestContextForTaskEnvelopeOnlyBudgetFailsClosed(t *testing.T) {
	fx := newContextFixture(t)
	res := mustContext(t, fx, taskPayment, fullOpts())
	envelopeOnly := &graph.TaskContext{Task: res.Task, MaxTokens: res.MaxTokens, EstimatedTokens: res.MaxTokens}
	envTokens, _, err := tokenest.OfJSON(envelopeOnly)
	if err != nil {
		t.Fatalf("OfJSON() error = %v", err)
	}
	opts := fullOpts()
	opts.MaxTokens = envTokens + 40 // room for the cursor and counters, not a symbol
	_, err = fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts)
	if !errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("error = %v, want ErrContextBudgetTooSmall", err)
	}
}

// Boundary: at the exact budget of a one-symbol page the page holds one symbol;
// one estimator unit below it, the page cannot be built at all.
func TestContextForTaskBudgetBoundaryIsExact(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()

	var fits *graph.TaskContext
	for budget := 60; budget <= 400; budget++ {
		opts := fullOpts()
		opts.MaxTokens = budget
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
		if err != nil {
			continue
		}
		if res.ReturnedSymbols == 1 {
			fits = res
			break
		}
	}
	if fits == nil {
		t.Fatal("no budget produced a one-symbol page")
	}
	assertWithinBudget(t, fits)

	opts := fullOpts()
	opts.MaxTokens = fits.EstimatedTokens - 1
	res, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
	if err == nil && res.ReturnedSymbols >= 1 {
		t.Fatalf("budget %d returned %d symbols; the same page needed %d tokens",
			opts.MaxTokens, res.ReturnedSymbols, fits.EstimatedTokens)
	}
	if err != nil && !errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("error = %v, want ErrContextBudgetTooSmall", err)
	}
}

func TestContextForTaskIncludeCallersFalseDropsGraphContext(t *testing.T) {
	fx := newContextFixture(t)
	opts := ContextForTaskOptions{IncludeTests: true}
	res := mustContext(t, fx, taskPayment, opts)
	for _, sym := range allSymbols(res) {
		if sym.Relevance == relevanceCaller || sym.Relevance == relevanceCallee {
			t.Fatalf("IncludeCallers=false still returned %s (%s)", sym.QualifiedName, sym.Relevance)
		}
	}
	if len(res.Files) == 0 {
		t.Fatal("direct matches disappeared with IncludeCallers=false")
	}
}

func TestContextForTaskIncludeTestsFalseDropsTestSection(t *testing.T) {
	fx := newContextFixture(t)
	opts := ContextForTaskOptions{IncludeCallers: true}
	res := mustContext(t, fx, taskPayment, opts)
	if len(res.TestFiles) != 0 {
		t.Fatalf("IncludeTests=false returned test files: %+v", res.TestFiles)
	}
	for _, sym := range allSymbols(res) {
		if sym.Relevance == relevanceTest {
			t.Fatalf("IncludeTests=false returned test-relevance symbol %s", sym.QualifiedName)
		}
	}
}

func TestContextForTaskIncludeTestsProducesTestSection(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	res := mustContext(t, fx, "renew billing payment", opts)
	if len(res.TestFiles) == 0 {
		t.Fatalf("IncludeTests=true produced no test files: %+v", res)
	}
	for _, f := range res.TestFiles {
		if !strings.HasSuffix(f.Path, "_test.go") {
			t.Fatalf("test section holds a non-test file %q", f.Path)
		}
		for _, sym := range f.Symbols {
			if sym.Relevance != relevanceTest {
				t.Fatalf("test section holds %s with relevance %q", sym.QualifiedName, sym.Relevance)
			}
		}
	}
}

func TestContextForTaskMaxFilesIsPreserved(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxFiles = 1
	opts.MaxTokens = MaxContextMaxTokens
	res := mustContext(t, fx, taskPayment, opts)
	if len(res.Files) != 1 {
		t.Fatalf("MaxFiles=1 returned %d files: %+v", len(res.Files), res.Files)
	}
}

func TestContextForTaskMaxSymbolsBoundsSeeds(t *testing.T) {
	fx := newContextFixture(t)
	opts := ContextForTaskOptions{MaxSymbols: 1, MaxTokens: MaxContextMaxTokens}
	res := mustContext(t, fx, taskPayment, opts)
	direct := 0
	for _, sym := range allSymbols(res) {
		if sym.Relevance == relevanceDirectMatch {
			direct++
		}
	}
	if direct != 1 {
		t.Fatalf("MaxSymbols=1 produced %d direct matches, want 1", direct)
	}
}

// No filesystem or network side effects: the tool reads the graph only. The
// fixture repository is left byte-identical, and the store is opened read-write
// but never written by this path.
func TestContextForTaskHasNoSideEffects(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	before, err := fx.store.LastScanID(ctx, fx.repoID)
	if err != nil {
		t.Fatalf("LastScanID() error = %v", err)
	}
	mustContext(t, fx, taskPayment, fullOpts())
	after, err := fx.store.LastScanID(ctx, fx.repoID)
	if err != nil {
		t.Fatalf("LastScanID() error = %v", err)
	}
	if before != after {
		t.Fatalf("scan id changed from %d to %d", before, after)
	}
}

func TestContextForTaskIsRepeatable(t *testing.T) {
	fx := newContextFixture(t)
	first, err := json.Marshal(mustContext(t, fx, taskPayment, fullOpts()))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := json.Marshal(mustContext(t, fx, taskPayment, fullOpts()))
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("call %d differs:\n%s\n%s", i, first, next)
		}
	}
}

func TestCardFieldsAreBounded(t *testing.T) {
	long := strings.Repeat("a", 1000)
	if got := truncateCardField(long, maxContextDocChars); len([]rune(got)) != maxContextDocChars {
		t.Fatalf("truncated length = %d, want %d", len([]rune(got)), maxContextDocChars)
	}
	if got := truncateCardField(long, maxContextDocChars); !strings.HasSuffix(got, contextTruncationMarker) {
		t.Fatalf("truncated value %q does not mark the cut", got)
	}
	if got := truncateCardField("short", maxContextDocChars); got != "short" {
		t.Fatalf("short value was altered: %q", got)
	}
	// Multi-byte input must be cut on a rune boundary, not mid-sequence.
	unicodeDoc := strings.Repeat("ä", 400)
	got := truncateCardField(unicodeDoc, maxContextDocChars)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}

	fx := newContextFixture(t)
	res := mustContext(t, fx, taskPayment, fullOpts())
	for _, sym := range allSymbols(res) {
		if len([]rune(sym.DocSummary)) > maxContextDocChars {
			t.Fatalf("doc_summary of %s is %d runes", sym.QualifiedName, len([]rune(sym.DocSummary)))
		}
		if len([]rune(sym.Signature)) > maxContextSignatureChars {
			t.Fatalf("signature of %s is %d runes", sym.QualifiedName, len([]rune(sym.Signature)))
		}
	}
}

// The reported estimate is part of the document it measures, so a page whose
// estimate crosses a digit boundary is the interesting case: sweep budgets and
// require exact agreement at every one.
func TestEstimatedTokensAgreesWithDocumentAcrossBudgets(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	checked := 0
	for budget := 100; budget <= 1200; budget += 7 {
		opts := fullOpts()
		opts.MaxTokens = budget
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
		if errors.Is(err, ErrContextBudgetTooSmall) {
			continue
		}
		if err != nil {
			t.Fatalf("ContextForTask(max_tokens=%d) error = %v", budget, err)
		}
		assertWithinBudget(t, res)
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d budgets produced a page; the sweep proved little", checked)
	}
}

// relevance_score must describe a file's place in the whole ranked stream, not
// in the page it happened to land on.
func TestFileRelevanceScoreIsAbsoluteAcrossPages(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	whole := fullOpts()
	whole.MaxTokens = MaxContextMaxTokens
	reference := map[string]float64{}
	for _, f := range mustContext(t, fx, taskPayment, whole).Files {
		reference[f.Path] = f.RelevanceScore
	}

	opts := fullOpts()
	opts.MaxTokens = 200
	cursor := ""
	pages := 0
	for {
		call := opts
		call.Cursor = cursor
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, call)
		if err != nil {
			t.Fatalf("page %d error = %v", pages+1, err)
		}
		pages++
		for _, f := range res.Files {
			if want, ok := reference[f.Path]; ok && f.RelevanceScore != want {
				t.Fatalf("page %d reports relevance_score %v for %s, unbudgeted call reports %v",
					pages, f.RelevanceScore, f.Path, want)
			}
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	if pages < 2 {
		t.Fatalf("walk took %d page(s); the assertion needs at least two", pages)
	}
}

// relevance_score is a public JSON field: it must not carry binary
// floating-point noise, and the budget must be able to price its width.
func TestFileRelevanceScoreSerializesCompactly(t *testing.T) {
	widest := 0
	for position := 0; position < 40; position++ {
		payload, err := json.Marshal(fileRelevanceScore(position))
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if len(payload) > widest {
			widest = len(payload)
		}
		if len(payload) > 6 {
			t.Fatalf("position %d serializes as %s", position, payload)
		}
	}
	reserved, err := json.Marshal(widestFileRelevanceScore)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if len(reserved) < widest {
		t.Fatalf("the estimate reserves %d bytes for relevance_score, the widest value needs %d", len(reserved), widest)
	}
}

// Every budget at which a page can be built must be walkable to exhaustion: a
// cursor an under-charged estimate turns into a permanent error is worse than a
// page that carries one symbol.
func TestEveryWalkableBudgetTerminates(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	walked := 0
	for budget := 120; budget <= 600; budget += 3 {
		opts := fullOpts()
		opts.MaxTokens = budget
		if _, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts); err != nil {
			if errors.Is(err, ErrContextBudgetTooSmall) {
				continue
			}
			t.Fatalf("first page at max_tokens=%d error = %v", budget, err)
		}
		keys, _ := pageAll(t, fx, taskPayment, opts)
		if len(keys) == 0 {
			t.Fatalf("walk at max_tokens=%d returned nothing", budget)
		}
		walked++
	}
	if walked < 100 {
		t.Fatalf("only %d budgets were walkable; the sweep proved little", walked)
	}
}

// A file whose highest-ranked symbol has no recorded language must still report
// the language of the file.
func TestFileLanguageFallsBackToANonEmptyCandidate(t *testing.T) {
	selected := []contextCandidate{
		{sym: graph.Symbol{ID: 1, Name: "A", QualifiedName: "pkg.A", FilePath: "a.go", StableKey: "k1"}, relevance: relevanceDirectMatch},
		{sym: graph.Symbol{ID: 2, Name: "B", QualifiedName: "pkg.B", FilePath: "a.go", StableKey: "k2", Language: "go"}, relevance: relevanceDirectMatch},
	}
	files, _ := projectCandidates(selected, fileRanks(selected))
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if files[0].Language != "go" {
		t.Fatalf("language = %q, want go", files[0].Language)
	}
}
