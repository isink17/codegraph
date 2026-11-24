package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

const goOnlyDeadCode = `{"data":{"dead_code":[{"end_line":5,"file":"lib/lib.go","kind":"function","language":"go","name":"Run","start_line":5,"symbol":"lib.Run"}]},"ok":true}`

// find_dead_code over a call-capable graph keeps its exact wire shape: the
// exported, uncalled Run is a candidate, the called Helper is not, and neither
// skipped_languages nor limitations appears.
func TestFindDeadCodeCallCapableGraphWireShape(t *testing.T) {
	server, ctx := capabilityFixture(t, false)
	if text, _ := callToolRaw(t, server, ctx, "find_dead_code", map[string]any{}); text != goOnlyDeadCode {
		t.Fatalf("find_dead_code = %s\nwant %s", text, goOnlyDeadCode)
	}
	for _, format := range []string{"compact", "compact-v2"} {
		text, isErr := callToolRaw(t, server, ctx, "find_dead_code", map[string]any{"format": format})
		if isErr {
			t.Fatal(text)
		}
		doc := decodeAnyCompact(t, format, text)
		if len(doc.Sections) != 1 || doc.Sections[0].Name != "dead_code" || len(doc.Sections[0].Rows) != 1 {
			t.Fatalf("%s sections = %+v", format, doc.Sections)
		}
	}
}

// A symbols-only language is not classified: its uncalled-looking methods are
// absent from dead_code, and the language is named in skipped_languages in
// JSON and in both compact encodings, beside the existing limitations.
func TestFindDeadCodeSkipsSymbolsOnlyLanguage(t *testing.T) {
	server, ctx := capabilityFixture(t, true)
	const want = `{"data":{"dead_code":[{"end_line":5,"file":"lib/lib.go","kind":"function","language":"go","name":"Run","start_line":5,"symbol":"lib.Run"}],` +
		`"skipped_languages":[{"language":"java","graph_capability":"symbols_only","reason":"no_call_edges"}]},` +
		`"limitations":[{"language":"java","graph_capability":"symbols_only","effect":"call edges absent: indexed symbols-only; an empty or short result is not evidence of no relationship"}],"ok":true}`
	if text, _ := callToolRaw(t, server, ctx, "find_dead_code", map[string]any{}); text != want {
		t.Fatalf("find_dead_code = %s\nwant %s", text, want)
	}
	for _, format := range []string{"compact", "compact-v2"} {
		text, isErr := callToolRaw(t, server, ctx, "find_dead_code", map[string]any{"format": format})
		if isErr {
			t.Fatal(text)
		}
		doc := decodeAnyCompact(t, format, text)
		var names []string
		for _, sec := range doc.Sections {
			names = append(names, sec.Name)
		}
		if !reflect.DeepEqual(names, []string{"dead_code", "skipped_languages", "limitations"}) {
			t.Fatalf("%s sections = %v", format, names)
		}
		skipped := doc.Sections[1]
		if !reflect.DeepEqual(skipped.Columns, []string{"language", "graph_capability", "reason"}) ||
			!reflect.DeepEqual(skipped.Rows, [][]string{{"java", "symbols_only", "no_call_edges"}}) {
			t.Fatalf("%s skipped_languages = %+v", format, skipped)
		}
		if rows := doc.Sections[0].Rows; len(rows) != 1 || rows[0][2] != "Run" {
			t.Fatalf("%s dead_code = %v", format, rows)
		}
	}
}

func decodeAnyCompact(t *testing.T, format, text string) *compactfmt.Decoded {
	t.Helper()
	if format != "compact-v2" {
		doc, err := compactfmt.Decode(text)
		if err != nil {
			t.Fatalf("%s: %v\n%s", format, err, text)
		}
		return doc
	}
	typed, err := compactfmt.DecodeV2(text)
	if err != nil {
		t.Fatalf("%s: %v\n%s", format, err, text)
	}
	doc := &compactfmt.Decoded{Version: typed.Version, Tool: typed.Tool}
	for _, sec := range typed.Sections {
		plain := compactfmt.DecodedSection{Name: sec.Name, Columns: sec.Columns}
		for _, row := range sec.Rows {
			var cells []string
			for _, cell := range row {
				cells = append(cells, cell.Scalar)
			}
			plain.Rows = append(plain.Rows, cells)
		}
		doc.Sections = append(doc.Sections, plain)
	}
	return doc
}

// Dead-code answers are a function of the persisted graph: an incremental
// update reaches the same answer as a fresh index of the same tree, through a
// symbols-only language appearing and disappearing and a call-capable one
// appearing.
func TestFindDeadCodeFreshIndexEqualsUpdateAcrossCapabilityTransitions(t *testing.T) {
	symbolsOnly := func() *parser.Registry {
		return parser.NewRegistry(goparser.New(), heuristicparser.NewJava())
	}
	pythonCalls := func() *parser.Registry {
		return parser.NewRegistry(goparser.New(), heuristicparser.NewJava(), pyparser.New())
	}
	answer := func(s *store.Store, repoRoot string) store.DeadCodeResult {
		t.Helper()
		repo, err := s.UpsertRepo(context.Background(), repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.FindDeadCodeResult(context.Background(), repo.ID, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	fresh := func(registry *parser.Registry, repoRoot string) store.DeadCodeResult {
		t.Helper()
		s := openTestStore(t)
		defer s.Close()
		if _, err := indexer.New(s, registry, nil).Index(context.Background(), indexer.Options{RepoRoot: repoRoot}); err != nil {
			t.Fatal(err)
		}
		return answer(s, repoRoot)
	}
	names := func(r store.DeadCodeResult) []string {
		var out []string
		for _, item := range r.Items {
			out = append(out, item["name"].(string))
		}
		return out
	}

	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "lib/lib.go", "package lib\n\nfunc Helper() int { return 1 }\n")
	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	if _, err := indexer.New(s, symbolsOnly(), nil).Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name     string
		registry func() *parser.Registry
		apply    func()
		dead     []string
		skipped  []store.DeadCodeSkippedLanguage
	}{
		{"go only", symbolsOnly, func() {}, []string{"Helper"}, nil},
		{"add symbols-only java", symbolsOnly, func() {
			writeRepoFile(t, repoRoot, "src/Widget.java", "package demo;\n\npublic class Widget {\n    int size() { return 1; }\n}\n")
			writeRepoFile(t, repoRoot, "lib/more.go", "package lib\n\nfunc More() int { return Helper() }\n")
		}, []string{"More"}, []store.DeadCodeSkippedLanguage{{Language: "java", Capability: store.GraphSymbolsOnly, Reason: store.DeadCodeNoCallEdges}}},
		{"add call-capable python", pythonCalls, func() {
			writeRepoFile(t, repoRoot, "py/m.py", "def used():\n    return 1\n\n\ndef caller():\n    return used()\n")
		}, []string{"More", "caller"}, []store.DeadCodeSkippedLanguage{{Language: "java", Capability: store.GraphSymbolsOnly, Reason: store.DeadCodeNoCallEdges}}},
		{"remove java", pythonCalls, func() {
			if err := os.Remove(filepath.Join(repoRoot, "src", "Widget.java")); err != nil {
				t.Fatal(err)
			}
		}, []string{"More", "caller"}, nil},
	}
	for _, step := range steps {
		step.apply()
		if _, err := indexer.New(s, step.registry(), nil).Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "update"}); err != nil {
			t.Fatalf("%s: update: %v", step.name, err)
		}
		updated := answer(s, repoRoot)
		if want := fresh(step.registry(), repoRoot); !reflect.DeepEqual(updated, want) {
			t.Fatalf("%s: update = %+v, fresh = %+v", step.name, updated, want)
		}
		if got := names(updated); !reflect.DeepEqual(got, step.dead) || !reflect.DeepEqual(updated.Skipped, step.skipped) {
			t.Fatalf("%s: dead = %v skipped = %+v, want %v %+v", step.name, got, updated.Skipped, step.dead, step.skipped)
		}
	}

}
