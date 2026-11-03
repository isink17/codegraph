package query

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

func TestExtractNamedReferences(t *testing.T) {
	tests := []struct {
		name string
		task string
		want []string
	}{
		{
			name: "dotted references and camel-case identifiers",
			task: "Trace a call from OkHttpClient.newCall through RealCall.execute and the chain",
			want: []string{"OkHttpClient.newCall", "RealCall.execute"},
		},
		{
			name: "one-hump capitalized prose is not a reference",
			task: "Trace Exchange. Explain Where the Request goes.",
			want: nil,
		},
		{
			name: "snake case qualifies",
			task: "explain process_payment end to end",
			want: []string{"process_payment"},
		},
		{
			name: "a non-ascii identifier is rejected whole, not split into a fragment",
			task: "fix the café_handler and Zählerstand_x paths",
			want: nil,
		},
		{
			name: "typographic punctuation is still a boundary",
			task: "the \u201cCacheInterceptor\u201d class, then RealCall.execute\u2192done",
			want: []string{"CacheInterceptor", "RealCall.execute"},
		},
		{
			name: "abbreviated prose is not a dotted reference",
			task: "e.g. the handler, i.e. the U.S. one",
			want: nil,
		},
		{
			name: "no identifier in the task",
			task: "explain how the cache decides to use a stored response",
			want: nil,
		},
		{
			name: "deduplicated in first-appearance order",
			task: "RealCall.execute then CacheInterceptor then RealCall.execute again",
			want: []string{"RealCall.execute", "CacheInterceptor"},
		},
		{
			name: "trailing sentence punctuation is not part of the name",
			task: "Start at RealCall.execute. Then stop.",
			want: []string{"RealCall.execute"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractNamedReferences(tc.task)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("extractNamedReferences(%q) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}

func TestExtractNamedReferencesIsCapped(t *testing.T) {
	var parts []string
	for i := 0; i < maxNamedReferences*3; i++ {
		parts = append(parts, "someName"+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	got := extractNamedReferences(strings.Join(parts, " "))
	if len(got) != maxNamedReferences {
		t.Fatalf("extractNamedReferences() returned %d references, want the cap %d", len(got), maxNamedReferences)
	}
}

// namedFixture is an indexed repository shaped around the retrieval failure the
// OkHttp A/B exposed: a long test method whose body repeats the task's common
// words outscores, on summed token overlap alone, the small implementation the
// task named outright.
type namedFixture struct {
	repoID int64
	svc    *Service
	store  *store.Store
}

func newNamedFixture(t *testing.T) *namedFixture {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()

	writeFixtureFile(t, repoRoot, "go.mod", "module example.test/named\n\ngo 1.22\n")
	writeFixtureFile(t, repoRoot, "gateway/gateway.go", `package gateway

// RequestGateway dispatches a request to a handler.
type RequestGateway struct{}

// Dispatch sends one request.
func (g *RequestGateway) Dispatch(id string) error { return nil }
`)
	writeFixtureFile(t, repoRoot, "billing/renew.go", `package billing

// Renew renews a billing record.
func Renew(id string) error { return nil }
`)
	writeFixtureFile(t, repoRoot, "subscription/renew.go", `package subscription

// Renew renews a subscription record.
func Renew(id string) error { return nil }
`)
	// Every interceptor declares intercept, so the bare name is ambiguous
	// beyond the fanout and must contribute nothing.
	for _, pkg := range []string{"alpha", "beta", "gamma", "delta"} {
		writeFixtureFile(t, repoRoot, pkg+"/interceptor.go", "package "+pkg+`

// intercept handles one hop.
func intercept(id string) error { return nil }
`)
	}
	// The decoy: a test whose documentation repeats the task's common words
	// many times. Summed token overlap has no length normalization, so sheer
	// repetition of generic words outscores the small implementation the task
	// named -- which is the shape of the OkHttp failure, where the winning hit
	// was a long test method.
	var body strings.Builder
	for i := 0; i < 60; i++ {
		body.WriteString("// request response dispatch gateway request response dispatch gateway\n")
	}
	decoy := "package decoy\n\nimport \"testing\"\n\n" + body.String() +
		"func TestRequestResponseDispatch(t *testing.T) { _ = t }\n"
	writeFixtureFile(t, repoRoot, "decoy/decoy_test.go", decoy)

	st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	idx := indexer.New(st, parser.NewRegistry(goparser.New(), pyparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	writeFixtureFile(t, repoRoot, "pylib/transport.py", `class HttpTransport:
    """Sends one request."""

    def send_request(self, body):
        return body
`)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return &namedFixture{repoID: repo.ID, svc: New(st, nil), store: st}
}

func (fx *namedFixture) paths(t *testing.T, task string, opts ContextForTaskOptions) []string {
	t.Helper()
	res, err := fx.svc.ContextForTask(context.Background(), fx.repoID, task, opts)
	if err != nil {
		t.Fatalf("ContextForTask(%q) error = %v", task, err)
	}
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

func rankOf(paths []string, want string) int {
	for i, p := range paths {
		if p == want {
			return i
		}
	}
	return -1
}

// TestNamedReferenceOutranksLongDecoy is the regression for the OkHttp
// retrieval failure: the task names the implementation, so the implementation
// leads and the long test that merely repeats the task's words does not.
func TestNamedReferenceOutranksLongDecoy(t *testing.T) {
	fx := newNamedFixture(t)
	task := "Trace a request through RequestGateway.Dispatch: request, response, dispatch, gateway"

	paths := fx.paths(t, task, ContextForTaskOptions{})
	impl := rankOf(paths, "gateway/gateway.go")
	decoy := rankOf(paths, "decoy/decoy_test.go")
	if impl != 0 {
		t.Fatalf("named implementation ranked %d, want 0; files = %v", impl, paths)
	}
	if decoy >= 0 && decoy < impl {
		t.Fatalf("decoy test ranked %d, above the named implementation at %d; files = %v", decoy, impl, paths)
	}
}

// TestNamedReferenceSurfacesBareSuffix pins the leg the OkHttp failure turned
// on: the task writes `RequestGateway.Dispatch`, the index stores a longer
// qualified name, and neither the `name` column nor the qualified name equals
// what the task wrote.
func TestNamedReferenceSurfacesBareSuffix(t *testing.T) {
	ctx := context.Background()
	fx := newNamedFixture(t)

	got, err := fx.store.SymbolsForNamedReferences(ctx, fx.repoID, []string{"RequestGateway.Dispatch"}, namedReferenceFanout)
	if err != nil {
		t.Fatalf("SymbolsForNamedReferences() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SymbolsForNamedReferences() returned %d matches, want 1: %+v", len(got), got)
	}
	if !strings.HasSuffix(got[0].QualifiedName, "RequestGateway.Dispatch") {
		t.Fatalf("resolved %q, want a qualified name ending in RequestGateway.Dispatch", got[0].QualifiedName)
	}
	if got[0].FilePath != "gateway/gateway.go" {
		t.Fatalf("resolved file %q, want gateway/gateway.go", got[0].FilePath)
	}
}

func TestNamedReferenceNegativeCases(t *testing.T) {
	ctx := context.Background()
	fx := newNamedFixture(t)

	t.Run("same name in unrelated packages resolves to both, never to one guess", func(t *testing.T) {
		got, err := fx.store.SymbolsForNamedReferences(ctx, fx.repoID, []string{"Renew"}, namedReferenceFanout)
		if err != nil {
			t.Fatalf("SymbolsForNamedReferences() error = %v", err)
		}
		files := map[string]bool{}
		for _, m := range got {
			files[m.FilePath] = true
		}
		if !files["billing/renew.go"] || !files["subscription/renew.go"] || len(files) != 2 {
			t.Fatalf("Renew resolved to %v, want exactly both package declarations", files)
		}
	})

	t.Run("a name beyond the fanout is ambiguous evidence and contributes nothing", func(t *testing.T) {
		got, err := fx.store.SymbolsForNamedReferences(ctx, fx.repoID, []string{"intercept"}, namedReferenceFanout)
		if err != nil {
			t.Fatalf("SymbolsForNamedReferences() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ambiguous name resolved to %d matches, want 0: %+v", len(got), got)
		}
	})

	t.Run("a name no symbol carries resolves to nothing", func(t *testing.T) {
		got, err := fx.store.SymbolsForNamedReferences(ctx, fx.repoID, []string{"NoSuchType.noSuchMethod"}, namedReferenceFanout)
		if err != nil {
			t.Fatalf("SymbolsForNamedReferences() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("missing name resolved to %+v, want nothing", got)
		}
	})

	t.Run("a task that names a test gets the test", func(t *testing.T) {
		paths := fx.paths(t, "explain what TestRequestResponseDispatch covers", ContextForTaskOptions{IncludeTests: true})
		if rankOf(paths, "decoy/decoy_test.go") < 0 {
			t.Fatalf("explicitly named test absent from %v", paths)
		}
	})

	t.Run("a task naming nothing still answers", func(t *testing.T) {
		if paths := fx.paths(t, "how does a request reach a handler", ContextForTaskOptions{}); len(paths) == 0 {
			t.Fatal("ContextForTask() returned no files for an identifier-free task")
		}
	})

	t.Run("another parser language resolves by the same legs", func(t *testing.T) {
		got, err := fx.store.SymbolsForNamedReferences(ctx, fx.repoID, []string{"HttpTransport.send_request"}, namedReferenceFanout)
		if err != nil {
			t.Fatalf("SymbolsForNamedReferences() error = %v", err)
		}
		if len(got) != 1 || got[0].Language != "python" {
			t.Fatalf("python reference resolved to %+v, want one python symbol", got)
		}
	})
}

func TestNamedReferenceRespectsFileCap(t *testing.T) {
	fx := newNamedFixture(t)
	task := "RequestGateway.Dispatch and Renew and HttpTransport.send_request"

	for _, maxFiles := range []int{1, 2, 50} {
		paths := fx.paths(t, task, ContextForTaskOptions{MaxFiles: maxFiles})
		if len(paths) > maxFiles {
			t.Fatalf("MaxFiles=%d returned %d files: %v", maxFiles, len(paths), paths)
		}
		if len(paths) == 0 {
			t.Fatalf("MaxFiles=%d returned no files", maxFiles)
		}
		if rankOf(paths, "gateway/gateway.go") != 0 {
			t.Fatalf("MaxFiles=%d: named implementation is not first: %v", maxFiles, paths)
		}
	}
}
