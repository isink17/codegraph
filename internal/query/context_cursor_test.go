package query

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
)

// pageAll walks the cursor to exhaustion and returns the concatenated symbol
// identities plus the number of pages it took.
func pageAll(t *testing.T, fx *contextFixture, task string, opts ContextForTaskOptions) (keys []string, pages int) {
	t.Helper()
	ctx := context.Background()
	cursor := ""
	for {
		call := opts
		call.Cursor = cursor
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, task, call)
		if err != nil {
			t.Fatalf("page %d error = %v", pages+1, err)
		}
		pages++
		keys = append(keys, symbolKeys(res)...)
		assertWithinBudget(t, res)
		if !res.HasMore {
			if res.NextCursor != "" {
				t.Fatalf("final page carries next_cursor %q", res.NextCursor)
			}
			if res.RemainingSymbols != 0 {
				t.Fatalf("final page reports %d remaining", res.RemainingSymbols)
			}
			return keys, pages
		}
		if res.NextCursor == "" {
			t.Fatal("has_more is set but next_cursor is empty")
		}
		if pages > 100 {
			t.Fatal("cursor did not terminate within 100 pages")
		}
		cursor = res.NextCursor
	}
}

// The pages of a budgeted walk must concatenate to exactly the unbudgeted ranked
// stream: no duplicate symbol, no gap, same order.
func TestCursorPaginationHasNoDuplicatesOrGaps(t *testing.T) {
	fx := newContextFixture(t)
	whole := fullOpts()
	whole.MaxTokens = MaxContextMaxTokens
	reference := symbolKeys(mustContext(t, fx, taskPayment, whole))
	if len(reference) < 4 {
		t.Fatalf("fixture yields only %d symbols; the pagination test needs more", len(reference))
	}

	small := fullOpts()
	small.MaxTokens = 200
	paged, pages := pageAll(t, fx, taskPayment, small)
	if pages < 2 {
		t.Fatalf("budget of 200 tokens produced %d page(s); expected several", pages)
	}
	if len(paged) != len(reference) {
		t.Fatalf("paged walk returned %d symbols, unbudgeted returned %d", len(paged), len(reference))
	}
	seen := map[string]bool{}
	for i := range reference {
		if paged[i] != reference[i] {
			t.Fatalf("position %d: paged %q, unbudgeted %q", i, paged[i], reference[i])
		}
		if seen[paged[i]] {
			t.Fatalf("symbol %q returned twice", paged[i])
		}
		seen[paged[i]] = true
	}
}

// A file whose symbols straddle a page boundary may have its envelope repeated;
// its symbols may not.
func TestPaginationRepeatsFileEnvelopesNotSymbols(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = 200
	ctx := context.Background()

	cursor := ""
	filePages := map[string]int{}
	symbolPages := map[string]int{}
	for {
		call := opts
		call.Cursor = cursor
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, call)
		if err != nil {
			t.Fatalf("ContextForTask() error = %v", err)
		}
		for _, f := range append(append([]graph.TaskContextFile{}, res.Files...), res.TestFiles...) {
			filePages[f.Path]++
			var last string
			for _, sym := range f.Symbols {
				symbolPages[sym.StableKey+"|"+sym.QualifiedName]++
				// Symbol order inside a file follows the ranked stream, so a file's
				// records are never re-ordered relative to each other; assert only
				// that the grouping is stable and non-empty.
				if sym.QualifiedName == "" {
					t.Fatalf("file %q holds a symbol without a qualified name", f.Path)
				}
				last = sym.QualifiedName
			}
			if last == "" {
				t.Fatalf("file %q was emitted with no symbols", f.Path)
			}
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	for key, count := range symbolPages {
		if count > 1 {
			t.Fatalf("symbol %q appeared on %d pages", key, count)
		}
	}
	if len(filePages) == 0 {
		t.Fatal("no files returned across the walk")
	}
}

// The budget may change between pages; the ranking, and therefore the cursor,
// must survive it.
func TestMaxTokensMayChangeBetweenPages(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	whole := fullOpts()
	whole.MaxTokens = MaxContextMaxTokens
	reference := symbolKeys(mustContext(t, fx, taskPayment, whole))

	first := fullOpts()
	first.MaxTokens = 200
	page1, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, first)
	if err != nil {
		t.Fatalf("page 1 error = %v", err)
	}
	if !page1.HasMore {
		t.Fatal("page 1 already exhausted the stream")
	}

	second := fullOpts()
	second.MaxTokens = MaxContextMaxTokens
	second.Cursor = page1.NextCursor
	page2, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, second)
	if err != nil {
		t.Fatalf("page 2 with a larger budget error = %v", err)
	}
	got := append(symbolKeys(page1), symbolKeys(page2)...)
	if len(got) != len(reference) {
		t.Fatalf("two pages returned %d symbols, want %d", len(got), len(reference))
	}
	for i := range reference {
		if got[i] != reference[i] {
			t.Fatalf("position %d: got %q, want %q", i, got[i], reference[i])
		}
	}
	if page2.HasMore {
		t.Fatalf("page 2 with a %d-token budget still reports more", second.MaxTokens)
	}
}

func firstPageCursor(t *testing.T, fx *contextFixture, task string) (string, ContextForTaskOptions) {
	t.Helper()
	opts := fullOpts()
	opts.MaxTokens = 200
	res, err := fx.svc.ContextForTask(context.Background(), fx.repoID, task, opts)
	if err != nil {
		t.Fatalf("ContextForTask() error = %v", err)
	}
	if res.NextCursor == "" {
		t.Fatal("first page produced no cursor")
	}
	return res.NextCursor, opts
}

func TestCursorRejectsTaskMismatch(t *testing.T) {
	fx := newContextFixture(t)
	cursor, opts := firstPageCursor(t, fx, taskPayment)
	opts.Cursor = cursor
	_, err := fx.svc.ContextForTask(context.Background(), fx.repoID, "a completely different task", opts)
	if !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
}

func TestCursorRejectsOptionMismatch(t *testing.T) {
	fx := newContextFixture(t)
	cursor, opts := firstPageCursor(t, fx, taskPayment)
	for name, mutate := range map[string]func(*ContextForTaskOptions){
		"include_callers": func(o *ContextForTaskOptions) { o.IncludeCallers = false },
		"include_tests":   func(o *ContextForTaskOptions) { o.IncludeTests = false },
		"max_files":       func(o *ContextForTaskOptions) { o.MaxFiles = 2 },
		"max_symbols":     func(o *ContextForTaskOptions) { o.MaxSymbols = 3 },
	} {
		call := opts
		call.Cursor = cursor
		mutate(&call)
		_, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, call)
		if !errors.Is(err, ErrContextCursorStale) {
			t.Fatalf("changing %s: error = %v, want ErrContextCursorStale", name, err)
		}
	}
}

func TestCursorRejectsWrongRepo(t *testing.T) {
	fx := newContextFixture(t)
	cursor, opts := firstPageCursor(t, fx, taskPayment)
	state, err := decodeContextCursor(cursor)
	if err != nil {
		t.Fatalf("decodeContextCursor() error = %v", err)
	}
	state.Repo = fx.repoID + 1
	tampered, err := encodeContextCursor(state)
	if err != nil {
		t.Fatalf("encodeContextCursor() error = %v", err)
	}
	opts.Cursor = tampered
	_, err = fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts)
	if !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
}

// Re-indexing the repository advances the scan id, and every outstanding cursor
// must stop working: its offset points into a stream that no longer exists.
func TestCursorRejectsStaleScan(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	cursor, opts := firstPageCursor(t, fx, taskPayment)

	writeFixtureFile(t, fx.repoRoot, "paymentsvc/extra.go", `package paymentsvc

// RetryPayment retries a failed payment charge.
func RetryPayment(id string) error { return ProcessPayment(id) }
`)
	if _, err := fx.indexer.Index(ctx, indexer.Options{RepoRoot: fx.repoRoot}); err != nil {
		t.Fatalf("re-Index() error = %v", err)
	}

	opts.Cursor = cursor
	_, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
	if !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
	if !strings.Contains(err.Error(), "re-indexed") {
		t.Fatalf("error %q should name the re-index", err)
	}
}

// Same graph, same task, but the candidate stream itself re-ranked: the cursor
// must refuse rather than skip or repeat context.
func TestCursorRejectsRankingDrift(t *testing.T) {
	fx := newContextFixture(t)
	cursor, opts := firstPageCursor(t, fx, taskPayment)
	state, err := decodeContextCursor(cursor)
	if err != nil {
		t.Fatalf("decodeContextCursor() error = %v", err)
	}
	state.Rank = "0000000000000000"
	drifted, err := encodeContextCursor(state)
	if err != nil {
		t.Fatalf("encodeContextCursor() error = %v", err)
	}
	opts.Cursor = drifted
	_, err = fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, opts)
	if !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
}

func TestCursorRejectsMalformedInput(t *testing.T) {
	fx := newContextFixture(t)
	valid, opts := firstPageCursor(t, fx, taskPayment)
	state, err := decodeContextCursor(valid)
	if err != nil {
		t.Fatalf("decodeContextCursor() error = %v", err)
	}

	negative := state
	negative.Offset = -5
	negativeCursor, _ := encodeContextCursor(negative)

	huge := state
	huge.Offset = 1 << 40
	hugeCursor, _ := encodeContextCursor(huge)

	badVersion := state
	badVersion.V = 99
	badVersionCursor, _ := encodeContextCursor(badVersion)

	cases := map[string]struct {
		cursor string
		want   error
	}{
		"not base64":      {"!!!not base64!!!", ErrContextCursorInvalid},
		"not json":        {base64.RawURLEncoding.EncodeToString([]byte("not json")), ErrContextCursorInvalid},
		"unknown field":   {base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"r":1,"s":1,"t":"x","o":"y","f":"z","n":0,"evil":1}`)), ErrContextCursorInvalid},
		"incomplete":      {base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"n":0}`)), ErrContextCursorInvalid},
		"negative offset": {negativeCursor, ErrContextCursorInvalid},
		"bad version":     {badVersionCursor, ErrContextCursorInvalid},
		"huge offset":     {hugeCursor, ErrContextCursorStale},
		"oversized":       {strings.Repeat("A", maxContextCursorInput+1), ErrContextCursorInvalid},
	}
	for name, tc := range cases {
		call := opts
		call.Cursor = tc.cursor
		_, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, call)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
}

func TestCursorDoesNotCarryTaskText(t *testing.T) {
	fx := newContextFixture(t)
	cursor, _ := firstPageCursor(t, fx, taskPayment)
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	if strings.Contains(string(decoded), "payment") {
		t.Fatalf("cursor embeds the task text: %s", decoded)
	}
	if len(cursor) > maxContextCursorInput {
		t.Fatalf("cursor length %d exceeds the decoder bound %d", len(cursor), maxContextCursorInput)
	}
}

func TestOptionsFingerprintIgnoresMaxTokens(t *testing.T) {
	a := ContextForTaskOptions{MaxFiles: 10, MaxSymbols: 30, IncludeTests: true, IncludeCallers: true, MaxTokens: 500}
	b := a
	b.MaxTokens = 40000
	b.Cursor = "irrelevant"
	if optionsFingerprint(a) != optionsFingerprint(b) {
		t.Fatal("max_tokens or cursor leaked into the options fingerprint")
	}
	c := a
	c.MaxFiles = 3
	if optionsFingerprint(a) == optionsFingerprint(c) {
		t.Fatal("max_files did not affect the options fingerprint")
	}
}
