package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// relationshipToolArgs is one representative call per relationship tool.
var relationshipToolArgs = map[string]map[string]any{
	"find_callers":       {"symbol": "Helper"},
	"find_callees":       {"symbol": "Run"},
	"get_impact_radius":  {"symbols": []string{"Helper"}},
	"trace_dependencies": {"symbol": "Run"},
	"find_related_tests": {"symbol": "Helper"},
	"find_dead_code":     {},
	"graph_analytics":    {"analysis": "pagerank"},
	"context_for_task":   {"task": "Helper"},
}

// capabilityFixture indexes a Go file (call-capable go/ast parser) and, when
// withJava, a Java file through the symbols-only heuristic adapter -- the
// adapter the non-cgo build uses. The registry is explicit, so the persisted
// state is the same in either build mode.
func capabilityFixture(t *testing.T, withJava bool) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "lib/lib.go", `package lib

func Helper(n int) int { return n * 2 }

func Run() int { return Helper(1) }
`)
	adapters := []parser.Adapter{goparser.New()}
	if withJava {
		writeRepoFile(t, repoRoot, "src/Widget.java", `package demo;

public class Widget {
    public int size() { return helper(); }
    private int helper() { return 1; }
}
`)
		adapters = append(adapters, heuristicparser.NewJava())
	}
	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	idx := indexer.New(s, parser.NewRegistry(adapters...), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	return NewServer(repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard), ctx
}

// On a call-capable graph every relationship tool's response is byte-for-byte
// what its handler produced before capability disclosure existed: the
// `limitations` key is absent, not empty, and the compact document carries no
// limitations section.
func TestRichGraphRelationshipResponsesAreByteStable(t *testing.T) {
	server, ctx := capabilityFixture(t, false)
	for name, args := range relationshipToolArgs {
		text, isErr := callToolRaw(t, server, ctx, name, args)
		if isErr {
			t.Fatalf("%s: error %s", name, text)
		}
		raw, _ := json.Marshal(args)
		desc, ok := dispatchableTool(name)
		if !ok {
			t.Fatalf("%s: not dispatchable", name)
		}
		handlerOnly, err := desc.handler(server, ctx, raw)
		if err != nil {
			t.Fatalf("%s handler: %v", name, err)
		}
		want, _ := json.Marshal(handlerOnly)
		if text != string(want) {
			t.Fatalf("%s: response differs from the bare handler\n got %s\nwant %s", name, text, want)
		}
		if strings.Contains(text, "limitations") {
			t.Fatalf("%s: rich graph discloses limitations: %s", name, text)
		}
		if compactCapableTools[name] {
			doc := callToolCompactDoc(t, server, ctx, name, args)
			if doc.Section("limitations") != nil {
				t.Fatalf("%s: compact carries a limitations section on a rich graph", name)
			}
		}
	}
	// A pinned literal for one tool, so a change to the bare handler shape is
	// caught too, not only a change in what wraps it.
	text, _ := callToolRaw(t, server, ctx, "find_callees", map[string]any{"symbol": "Helper"})
	if text != `{"data":{"callees":[],"target_found":true},"ok":true}` {
		t.Fatalf("find_callees(Helper) = %s", text)
	}
}

// A graph holding a symbols-only language discloses it on every relationship
// tool, in JSON and compact alike, with the same rows. An empty Java answer is
// therefore never presented as proof that nothing calls the method.
func TestSymbolsOnlyLanguageDisclosedOnRelationshipTools(t *testing.T) {
	server, ctx := capabilityFixture(t, true)
	want := []store.GraphLimitation{{Language: "java", Capability: store.GraphSymbolsOnly}}
	for name, args := range relationshipToolArgs {
		text, isErr := callToolRaw(t, server, ctx, name, args)
		if isErr {
			t.Fatalf("%s: error %s", name, text)
		}
		var envelope struct {
			OK          bool                    `json:"ok"`
			Data        json.RawMessage         `json:"data"`
			Limitations []store.GraphLimitation `json:"limitations"`
		}
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !envelope.OK || len(envelope.Data) == 0 {
			t.Fatalf("%s: envelope changed: %s", name, text)
		}
		if len(envelope.Limitations) != 1 || envelope.Limitations[0].Language != want[0].Language ||
			envelope.Limitations[0].Capability != want[0].Capability || envelope.Limitations[0].Effect == "" {
			t.Fatalf("%s: limitations = %+v, want java/symbols_only", name, envelope.Limitations)
		}
		if compactCapableTools[name] {
			doc := callToolCompactDoc(t, server, ctx, name, args)
			rows := doc.Section("limitations")
			if rows == nil || len(rows.Rows) != 1 {
				t.Fatalf("%s: compact limitations = %+v", name, rows)
			}
			l := envelope.Limitations[0]
			if got := rows.Rows[0]; got[0] != l.Language || got[1] != l.Capability || got[2] != l.Effect {
				t.Fatalf("%s: compact row %v != json %+v", name, got, l)
			}
		}
	}
	// context_for_task answers from callers only when it expands them.
	text, _ := callToolRaw(t, server, ctx, "context_for_task", map[string]any{"task": "Helper", "include_callers": false})
	if strings.Contains(text, "limitations") {
		t.Fatalf("context_for_task without callers discloses limitations: %s", text)
	}
	// The Java target itself: found, no callers, and the answer says why that
	// emptiness proves nothing.
	text, _ = callToolRaw(t, server, ctx, "find_callers", map[string]any{"symbol": "helper"})
	if !strings.Contains(text, `"callers":[]`) || !strings.Contains(text, `"target_found":true`) ||
		!strings.Contains(text, `"limitations":[{"language":"java","graph_capability":"symbols_only"`) {
		t.Fatalf("find_callers(helper) = %s", text)
	}
}

// supported_languages reports what the persisted graph holds next to what this
// binary parses: Go call-capable plus Java symbols-only is a mixed graph.
func TestSupportedLanguagesReportsPersistedCapability(t *testing.T) {
	server, ctx := capabilityFixture(t, true)
	text, isErr := callToolRaw(t, server, ctx, "supported_languages", map[string]any{})
	if isErr {
		t.Fatal(text)
	}
	var envelope struct {
		Data struct {
			Capability store.GraphCapability `json:"graph_capability"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatal(err)
	}
	got := envelope.Data.Capability
	if got.State != store.GraphMixed || len(got.Languages) != 2 ||
		got.Languages[0].Capability != store.GraphCallCapable || got.Languages[1].Capability != store.GraphSymbolsOnly {
		t.Fatalf("graph_capability = %+v", got)
	}

	rich, ctx := capabilityFixture(t, false)
	text, _ = callToolRaw(t, rich, ctx, "supported_languages", map[string]any{})
	if !strings.Contains(text, `"graph_capability":{"state":"call_capable","languages":[{"language":"go","graph_capability":"call_capable","parser_profiles":["go-ast:go:v1"]}]}`) {
		t.Fatalf("rich supported_languages = %s", text)
	}
}

// The capability report is a function of the persisted provenance, so a fresh
// index and an incremental update of the same tree agree on it, after adding
// and after removing a file of a call-capable and of a symbols-only language.
func TestCapabilityReportFreshIndexEqualsUpdate(t *testing.T) {
	registry := func() *parser.Registry { return parser.NewRegistry(goparser.New(), heuristicparser.NewJava()) }
	capabilityOf := func(s *store.Store, repoRoot string) store.GraphCapability {
		t.Helper()
		repo, err := s.UpsertRepo(context.Background(), repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GraphCapability(context.Background(), repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		limitations, err := s.GraphLimitations(context.Background(), repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (len(limitations) > 0) != (got.State != store.GraphCallCapable && got.State != "") {
			t.Fatalf("limitations %+v disagree with state %q", limitations, got.State)
		}
		return got
	}
	fresh := func(repoRoot string) store.GraphCapability {
		t.Helper()
		s := openTestStore(t)
		defer s.Close()
		if _, err := indexer.New(s, registry(), nil).Index(context.Background(), indexer.Options{RepoRoot: repoRoot}); err != nil {
			t.Fatal(err)
		}
		return capabilityOf(s, repoRoot)
	}

	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "lib/lib.go", "package lib\n\nfunc Helper() int { return 1 }\n")
	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	idx := indexer.New(s, registry(), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name  string
		apply func()
		state string
	}{
		{"add java and go", func() {
			writeRepoFile(t, repoRoot, "src/Widget.java", "package demo;\n\npublic class Widget {\n    int size() { return 1; }\n}\n")
			writeRepoFile(t, repoRoot, "lib/more.go", "package lib\n\nfunc More() int { return Helper() }\n")
		}, store.GraphMixed},
		{"remove go file", func() {
			if err := os.Remove(filepath.Join(repoRoot, "lib/more.go")); err != nil {
				t.Fatal(err)
			}
		}, store.GraphMixed},
		{"remove java file", func() {
			if err := os.Remove(filepath.Join(repoRoot, "src/Widget.java")); err != nil {
				t.Fatal(err)
			}
		}, store.GraphCallCapable},
	}
	for _, step := range steps {
		step.apply()
		if _, err := idx.Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "update"}); err != nil {
			t.Fatalf("%s: update: %v", step.name, err)
		}
		updated := capabilityOf(s, repoRoot)
		if want := fresh(repoRoot); !reflect.DeepEqual(updated, want) {
			t.Fatalf("%s: update = %+v, fresh = %+v", step.name, updated, want)
		}
		if updated.State != step.state {
			t.Fatalf("%s: state = %q, want %q", step.name, updated.State, step.state)
		}
	}
}
