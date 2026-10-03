//go:build cgo

package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// rubyModifierSource puts a definition under each modifier that can wrap one.
// Ruby 4.0 runs it as: Subscriber#blocked -> #emit_event -> #format_event;
// Subscriber.make -> .build -> .new_instance; App::Subscriber.build from
// Caller raises NoMethodError (private); Tools#util -> #helper.
const rubyModifierSource = `module App
  class Subscriber
    def blocked(event)
      emit_event("blocked", event)
    end

    private def emit_event(name, payload)
      format_event(name)
    end

    def format_event(name)
      name
    end

    private_class_method def self.build(x)
      new_instance(x)
    end

    def self.new_instance(x)
      x
    end

    def self.make(x)
      build(x)
    end
  end

  class Caller
    def run
      Subscriber.build(1)
      Subscriber.make(1)
    end
  end

  module Tools
    module_function def util(x)
      helper(x)
    end

    def helper(x)
      x
    end
  end
end
`

// rubyModifierBindings is the whole call graph the current parser and resolver
// produce for rubyModifierSource. The private singleton binds from its own
// owner's implicit self and stays unresolved through a constant receiver.
const rubyModifierBindings = `Subscriber.build@30=-|-
Subscriber.make@31=App.Subscriber.make|ruby_lexical_constant
build@24=App.Subscriber.build|ruby_implicit_self
emit_event@4=App.Subscriber.emit_event|ruby_implicit_self
format_event@8=App.Subscriber.format_event|ruby_implicit_self
helper@37=App.Tools.helper|ruby_implicit_self
new_instance@16=App.Subscriber.new_instance|ruby_implicit_self`

func TestRubyModifierWrappedDefinitionsBindEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "subscriber.rb"), rubyModifierSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got := rubyBindings(t, s, repoID(t, s, root)); got != rubyModifierBindings {
		t.Fatalf("bindings:\n%s\nwant:\n%s", got, rubyModifierBindings)
	}
}

// rubyV5ModifierAdapter models treesitter:ruby:v5 for rubyModifierSource: it
// drops every method symbol a modifier wraps, restores the wrapper call edge
// v5 emitted (its line then has no method, so the store drops it), and turns
// the class-method wrapper's override back into the owner hazard v5 recorded.
type rubyV5ModifierAdapter struct {
	*tsparser.RubyAdapter
}

func (rubyV5ModifierAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v5", EmitsCallEdges: true}
}

func (a rubyV5ModifierAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := a.RubyAdapter.Parse(ctx, path, content)
	if err != nil {
		return p, err
	}
	lines := strings.Split(string(content), "\n")
	symbols := p.Symbols[:0]
	for _, sym := range p.Symbols {
		prefix := strings.TrimSpace(lines[sym.Range.StartLine-1][:sym.Range.StartCol-1]) // StartCol is 1-based
		if sym.Kind == "function" && prefix != "" {
			p.Edges = append(p.Edges, graph.Edge{DstName: prefix, Kind: "calls", Evidence: "ruby:implicit_receiver", Line: sym.Range.StartLine})
			continue
		}
		symbols = append(symbols, sym)
	}
	p.Symbols = symbols
	for i, fact := range p.Scope.Imports {
		if fact.Kind == graph.ScopeImportRubySingletonVisibility && fact.LocalName == "build" {
			p.Scope.Imports[i] = graph.ScopeImport{Kind: graph.ScopeImportRubySingletonVisibilityUnknown, OwnerModule: fact.OwnerModule, Static: true}
		}
	}
	return p, nil
}

// The profile bump alone must replace a v5 graph's missing definitions: same
// bytes, no Force, no touched file, and the result equals a fresh index.
func TestRubyProfileV5ToV6RecordsModifierWrappedDefinitions(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "subscriber.rb"), rubyModifierSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(rubyV5ModifierAdapter{tsparser.NewRuby()}), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v5 index: %v", err)
	}
	repo := repoID(t, s, root)
	v5 := rubyBindings(t, s, repo)
	if !strings.Contains(v5, "emit_event@4=-|-") || strings.Contains(v5, "format_event@8") {
		t.Fatalf("v5 fixture does not model the missing definitions:\n%s", v5)
	}

	upgraded := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("v6 update: %v", err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "ruby" {
		t.Fatalf("changed=%d languages=%v; the profile bump alone must reparse", summary.FilesChanged, summary.ParserProfileLanguages)
	}
	if groups := profilesInDB(t, s, repo); len(groups) != 1 || groups[0].Profile != tsparser.NewRuby().Profile().ID || !groups[0].CallEdges {
		t.Fatalf("provenance after upgrade = %#v", groups)
	}
	if got := rubyBindings(t, s, repo); got != rubyModifierBindings {
		t.Fatalf("upgraded bindings:\n%s\nwant (fresh):\n%s", got, rubyModifierBindings)
	}
	if got, want := rubyVisibilityFactRows(t, s, repo), freshRubyVisibilityFactRows(t, root); got != want {
		t.Fatalf("upgraded visibility facts:\n%s\nfresh:\n%s", got, want)
	}
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reparsed: changed=%d languages=%v", again.FilesChanged, again.ParserProfileLanguages)
	}
}

func freshRubyVisibilityFactRows(t *testing.T, root string) string {
	t.Helper()
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	return rubyVisibilityFactRows(t, fresh, repoID(t, fresh, root))
}
