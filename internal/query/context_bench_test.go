package query

import (
	"context"
	"testing"
)

// The benchmarks split the request into its phases, so a regression can be
// attributed rather than guessed at: search+expansion+ranking on one side,
// projection+budgeting+cursor on the other.
func BenchmarkContextForTaskDefault(b *testing.B) {
	fx := newContextFixtureForBench(b)
	ctx := context.Background()
	opts := ContextForTaskOptions{IncludeCallers: true, IncludeTests: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContextForTaskSeedsOnly(b *testing.B) {
	fx := newContextFixtureForBench(b)
	ctx := context.Background()
	opts := ContextForTaskOptions{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContextRanking(b *testing.B) {
	fx := newContextFixtureForBench(b)
	ctx := context.Background()
	opts := ContextForTaskOptions{MaxFiles: defaultContextMaxFiles, MaxSymbols: defaultContextMaxSymbols, IncludeCallers: true, IncludeTests: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := fx.svc.rankContextCandidates(ctx, fx.repoID, taskPayment, opts); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContextBudgetFill(b *testing.B) {
	fx := newContextFixtureForBench(b)
	ctx := context.Background()
	opts := ContextForTaskOptions{MaxFiles: defaultContextMaxFiles, MaxSymbols: defaultContextMaxSymbols, IncludeCallers: true, IncludeTests: true}
	candidates, err := fx.svc.rankContextCandidates(ctx, fx.repoID, taskPayment, opts)
	if err != nil {
		b.Fatal(err)
	}
	in := budgetInput{
		task:       taskPayment,
		candidates: candidates,
		maxTokens:  DefaultContextMaxTokens,
		cursor:     contextCursorState{V: contextCursorVersion, Repo: fx.repoID, Scan: 1, Task: "t", Opts: "o", Rank: "r"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := buildBudgetedContext(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContextCursorRoundTrip(b *testing.B) {
	state := contextCursorState{V: contextCursorVersion, Repo: 7, Scan: 42, Task: "0123456789abcdef", Opts: "fedcba9876543210", Rank: "abcdefabcdef0000", Offset: 31}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		encoded, err := encodeContextCursor(state)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := decodeContextCursor(encoded); err != nil {
			b.Fatal(err)
		}
	}
}
