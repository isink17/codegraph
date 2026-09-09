//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// rubyV3Adapter reproduces genuine treesitter:ruby:v3 output: the current
// parser minus everything P22.48 added. The profile id is not faked on its own
// -- the singleton visibility facts are removed and every symbol's visibility
// is cleared back to v3's silence -- so a repository indexed with it really
// cannot resolve a constant receiver, really cannot know a private one, and
// really carries no constant-identity hazard.
type rubyV3Adapter struct {
	*tsparser.RubyAdapter
}

// rubyV4Adapter reproduces genuine treesitter:ruby:v4 output: it keeps
// P22.48 singleton facts and strips only P22.50 constant facts.
type rubyV4Adapter struct {
	*tsparser.RubyAdapter
}

func (rubyV4Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v4", EmitsCallEdges: true}
}

func (a rubyV4Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := a.RubyAdapter.Parse(ctx, path, content)
	if err != nil {
		return p, err
	}
	kept := p.Scope.Imports[:0]
	for _, fact := range p.Scope.Imports {
		switch fact.Kind {
		case graph.ScopeImportRubyConstantIdentityUnknown,
			graph.ScopeImportRubyConstantVisibility,
			graph.ScopeImportRubyConstantVisibilityUnknown:
			continue
		}
		kept = append(kept, fact)
	}
	p.Scope.Imports = kept
	return p, nil
}

func (rubyV3Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v3", EmitsCallEdges: true}
}

func (a rubyV3Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.RubyAdapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	kept := pf.Scope.Imports[:0]
	for _, fact := range pf.Scope.Imports {
		switch fact.Kind {
		case graph.ScopeImportRubySingletonVisibility,
			graph.ScopeImportRubySingletonVisibilityUnknown,
			graph.ScopeImportRubyConstantIdentityUnknown:
			continue
		}
		kept = append(kept, fact)
	}
	pf.Scope.Imports = kept
	for i := range pf.Symbols {
		pf.Symbols[i].Visibility = ""
	}
	return pf, nil
}

// rubyBindings renders every Ruby call edge as `dst_name@line=target`, with
// `-` for an unresolved one. It is the whole observable answer of the pass.
func rubyBindings(t *testing.T, s *profileStore, repo int64) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT e.dst_name, e.line, COALESCE(d.qualified_name, '-'), NULLIF(COALESCE(e.resolution_strategy, ''), '')
		FROM edges e JOIN files f ON f.id = e.file_id
		LEFT JOIN symbols d ON d.id = e.dst_symbol_id
		WHERE e.repo_id = ? AND f.language = 'ruby' AND e.edge_kind = 'calls'`, repo)
	if err != nil {
		t.Fatalf("read ruby bindings: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, target string
		var strategy sql.NullString
		var line int
		if err := rows.Scan(&name, &line, &target, &strategy); err != nil {
			t.Fatal(err)
		}
		provenance := "-"
		if strategy.Valid {
			provenance = strategy.String
		}
		out = append(out, fmt.Sprintf("%s@%d=%s|%s", name, line, target, provenance))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func rubyVisibilityFactRows(t *testing.T, s *profileStore, repo int64) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT import_kind, owner_module, local_name, source_specifier
		FROM scope_import_evidence WHERE repo_id = ? AND language = 'ruby'
		  AND import_kind IN (?, ?, ?, ?, ?)`,
		repo, graph.ScopeImportRubySingletonVisibility, graph.ScopeImportRubySingletonVisibilityUnknown,
		graph.ScopeImportRubyConstantIdentityUnknown, graph.ScopeImportRubyConstantVisibility,
		graph.ScopeImportRubyConstantVisibilityUnknown)
	if err != nil {
		t.Fatalf("read visibility facts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, owner, local, specifier string
		if err := rows.Scan(&kind, &owner, &local, &specifier); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind+"|"+owner+"|"+local+"|"+specifier)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// freshRubyGraph indexes the same tree from scratch under the current parser
// and returns its bindings, so an upgraded or incrementally updated graph can
// be compared against the answer a clean index gives.
func freshRubyGraph(t *testing.T, root string) string {
	t.Helper()
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewRuby()), nil).
		Index(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatalf("fresh index: %v", err)
	}
	return rubyBindings(t, fresh, repoID(t, fresh, root))
}

const rubyConstantPublicSource = `module App
  class Service
    def self.run; end
  end

  class Caller
    def f
      Service.run()
      Service::run()
    end
  end
end
`

const rubyConstantPrivateSource = `module App
  class Service
    def self.run; end
    private_class_method :run
  end

  class Caller
    def f
      Service.run()
    end
  end
end
`

// The parser's fact set changed, so the profile is the compatibility boundary:
// the same bytes, with no Force, no Paths and every resolver repair already
// marked done, must reparse the file, produce the visibility facts, resolve the
// constant receivers and converge on exactly the from-scratch v4 graph.
func TestRubyProfileV3ToV4ResolvesConstantReceivers(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		wantTarget   string
	}{
		{"public target", rubyConstantPublicSource, "App.Service.run"},
		{"private target stays unresolved", rubyConstantPrivateSource, "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			path := filepath.Join(root, "app.rb")
			writeProfileFile(t, path, tc.source)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			s := newProfileStore(t)
			old := New(s.Store, parser.NewRegistry(rubyV3Adapter{tsparser.NewRuby()}), nil)
			if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("v3 index: %v", err)
			}
			repo := repoID(t, s, root)

			// The v3 fixture really is a pre-P22.48 graph: no visibility facts,
			// and every constant receiver unresolved.
			if got := rubyVisibilityFactRows(t, s, repo); got != "" {
				t.Fatalf("v3 fixture already carries visibility facts:\n%s", got)
			}
			for _, line := range strings.Split(rubyBindings(t, s, repo), "\n") {
				if strings.Contains(line, "Service.run@") || strings.Contains(line, "Service::run@") {
					if !strings.Contains(line, "=-|-") {
						t.Fatalf("v3 fixture already resolved a constant receiver: %s", line)
					}
				}
			}
			if err := s.Store.MarkResolverBindingsRepaired(ctx, repo); err != nil {
				t.Fatal(err)
			}

			upgraded := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
			summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
			if err != nil {
				t.Fatalf("v4 update: %v", err)
			}
			if summary.FilesChanged != 1 {
				t.Fatalf("FilesChanged = %d, want 1; the profile bump alone must reparse an unchanged file", summary.FilesChanged)
			}
			if got := strings.Join(summary.ParserProfileLanguages, ","); got != "ruby" {
				t.Fatalf("ParserProfileLanguages = %q, want \"ruby\"", got)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
				t.Fatalf("the test modified the source file")
			}
			groups := profilesInDB(t, s, repo)
			if len(groups) != 1 || groups[0].Profile != "treesitter:ruby:v5" || !groups[0].CallEdges {
				t.Fatalf("provenance after upgrade = %#v", groups)
			}

			bindings := rubyBindings(t, s, repo)
			if !strings.Contains(bindings, "Service.run@8="+tc.wantTarget) &&
				!strings.Contains(bindings, "Service.run@9="+tc.wantTarget) {
				t.Fatalf("constant receiver after upgrade:\n%s\nwant target %s", bindings, tc.wantTarget)
			}
			if got, want := bindings, freshRubyGraph(t, root); got != want {
				t.Fatalf("upgraded graph:\n%s\nfrom-scratch v4 graph:\n%s", got, want)
			}
		})
	}
}

// rubyIncrementalStep is one source edit and the answer the graph must give
// afterwards, both incrementally and from scratch.
type rubyIncrementalStep struct {
	name   string
	source string
	want   map[string]string
}

// runRubyIncremental writes each step's source over the same file, runs a plain
// Update with no Force and no Paths, and requires the resulting bindings to
// equal a from-scratch index of the same bytes. Every intermediate state is
// checked, so no step may be rescued by a stale binding or by a generic
// strategy, and fresh == incremental has to hold at each one.
func runRubyIncremental(t *testing.T, files map[string]string, edited string, steps []rubyIncrementalStep) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	for name, content := range files {
		writeProfileFile(t, filepath.Join(root, name), content)
	}
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	for _, step := range steps {
		if step.source == "" {
			if err := os.Remove(filepath.Join(root, edited)); err != nil {
				t.Fatalf("%s: remove: %v", step.name, err)
			}
		} else {
			writeProfileFile(t, filepath.Join(root, edited), step.source)
		}
		if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatalf("%s: update: %v", step.name, err)
		}
		bindings := rubyBindings(t, s, repo)
		for spelling, want := range step.want {
			found := false
			for _, line := range strings.Split(bindings, "\n") {
				if strings.HasPrefix(line, spelling+"@") {
					found = true
					if target := line[strings.IndexByte(line, '=')+1:]; !strings.HasPrefix(target, want) {
						t.Fatalf("%s: %s -> %s, want %s\n%s", step.name, spelling, target, want, bindings)
					}
				}
			}
			if !found {
				t.Fatalf("%s: no call edge spelled %s\n%s", step.name, spelling, bindings)
			}
		}
		if got, want := bindings, freshRubyGraph(t, root); got != want {
			t.Fatalf("%s: incremental graph:\n%s\nfresh graph:\n%s", step.name, got, want)
		}
	}
}

// The target's existence, its visibility and the receiver's spelling are each
// re-decided from the current facts, and a failed constant edge is never
// rescued by a generic strategy in any intermediate state.
func TestRubyConstantReceiverIncrementalTransitions(t *testing.T) {
	caller := `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`
	target := func(body string) string {
		return "module App\n  class Service\n" + body + "  end\nend\n"
	}
	t.Run("target lifecycle and visibility", func(t *testing.T) {
		runRubyIncremental(t, map[string]string{
			"caller.rb": caller,
			"target.rb": target("    def self.run; end\n"),
		}, "target.rb", []rubyIncrementalStep{
			{name: "initial public", source: target("    def self.run; end\n"),
				want: map[string]string{"Service.run": "App.Service.run"}},
			{name: "private_class_method", source: target("    def self.run; end\n    private_class_method :run\n"),
				want: map[string]string{"Service.run": "-"}},
			{name: "public again", source: target("    def self.run; end\n"),
				want: map[string]string{"Service.run": "App.Service.run"}},
			{name: "eigenclass private", source: target("    class << self\n      private\n      def run; end\n    end\n"),
				want: map[string]string{"Service.run": "-"}},
			{name: "eigenclass public", source: target("    class << self\n      def run; end\n    end\n"),
				want: map[string]string{"Service.run": "App.Service.run"}},
			{name: "instance only", source: target("    def run; end\n"),
				want: map[string]string{"Service.run": "-"}},
			{name: "module_function hazard", source: target("    def self.run; end\n    module_function :run\n"),
				want: map[string]string{"Service.run": "-"}},
			{name: "restored", source: target("    def self.run; end\n"),
				want: map[string]string{"Service.run": "App.Service.run"}},
			{name: "target file deleted", source: "",
				want: map[string]string{"Service.run": "-"}},
		})
	})

	// Constant identity is recomputed before any method is looked up: a
	// shadowing inner constant with no `run` withdraws the bind entirely, and
	// giving it a `run` binds the inner one, never the outer.
	t.Run("shadowing and lexical nesting", func(t *testing.T) {
		outer := "module App\n  class Service\n    def self.run; end\n  end\nend\n"
		runRubyIncremental(t, map[string]string{"outer.rb": outer, "caller.rb": caller}, "caller.rb",
			[]rubyIncrementalStep{
				{name: "outer match", source: caller,
					want: map[string]string{"Service.run": "App.Service.run"}},
				{name: "inner constant shadows", source: `module App
  class Caller
    class Service
    end

    def f
      Service.run()
    end
  end
end
`, want: map[string]string{"Service.run": "-"}},
				{name: "inner target appears", source: `module App
  class Caller
    class Service
      def self.run; end
    end

    def f
      Service.run()
    end
  end
end
`, want: map[string]string{"Service.run": "App.Caller.Service.run"}},
				{name: "shadow removed", source: caller,
					want: map[string]string{"Service.run": "App.Service.run"}},
				// `class App::Caller` pushes one lexical frame, so the outer
				// `App::Service` is no longer in scope for an unqualified name.
				{name: "qualified open", source: `class App::Caller
  def f
    Service.run()
  end
end
`, want: map[string]string{"Service.run": "-"}},
				{name: "nested again", source: caller,
					want: map[string]string{"Service.run": "App.Service.run"}},
			})
	})

	t.Run("receiver shape", func(t *testing.T) {
		outer := "module App\n  class Service\n    def self.run; end\n  end\nend\n"
		shape := func(spelling string) string {
			return "module App\n  class Caller\n    def f\n      " + spelling + "\n    end\n  end\nend\n"
		}
		runRubyIncremental(t, map[string]string{"outer.rb": outer, "caller.rb": shape("Service.run()")}, "caller.rb",
			[]rubyIncrementalStep{
				{name: "dot", source: shape("Service.run()"),
					want: map[string]string{"Service.run": "App.Service.run"}},
				{name: "multi segment", source: shape("App::Service.run()"),
					want: map[string]string{"App::Service.run": "-"}},
				{name: "absolute", source: shape("::App::Service.run()"),
					want: map[string]string{"::App::Service.run": "-"}},
				{name: "value receiver", source: shape("obj.run()"),
					want: map[string]string{"obj.run": "-"}},
				{name: "safe navigation", source: shape("Service&.run()"),
					want: map[string]string{"Service&.run": "-"}},
				{name: "chained", source: shape("Service.build.run()"),
					want: map[string]string{"Service.build.run": "-"}},
				{name: "scope operator", source: shape("Service::run()"),
					want: map[string]string{"Service::run": "App.Service.run"}},
			})
	})
}

// A production caller never reaches a constant or a singleton method that only
// a spec file declares.
func TestRubyConstantReceiverTestFileTargets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	writeProfileFile(t, filepath.Join(root, "spec", "service_spec.rb"), `module App
  class Service
    def self.run; end
  end
end
`)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	bindings := rubyBindings(t, s, repoID(t, s, root))
	if !strings.Contains(bindings, "Service.run@4=-|-") {
		t.Fatalf("a production caller reached a spec-only target:\n%s", bindings)
	}
}

// Query and traversal see the new edges through the same code paths as every
// other resolved call: nothing about them is Ruby-specific.
func TestRubyConstantReceiverIsVisibleToTraversal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "app.rb"), rubyConstantPublicSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	callers, err := s.FindCallers(ctx, repo, "App.Service.run", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers: %v", err)
	}
	found := false
	for _, c := range callers {
		found = found || c.QualifiedName == "App.Caller.f"
	}
	if !found {
		t.Fatalf("FindCallers(App.Service.run) = %#v, want App.Caller.f", callers)
	}
	callees, err := s.FindCallees(ctx, repo, "App.Caller.f", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees: %v", err)
	}
	found = false
	for _, c := range callees {
		found = found || c.QualifiedName == "App.Service.run"
	}
	if !found {
		t.Fatalf("FindCallees(App.Caller.f) = %#v, want App.Service.run", callees)
	}
}

// Visibility facts are file-owned evidence: re-parsing the file replaces them,
// so removing the override removes the row rather than leaving a stale private.
func TestRubySingletonVisibilityFactsAreReplacedOnReparse(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "service.rb")
	writeProfileFile(t, path, rubyConstantPrivateSource)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	want := graph.ScopeImportRubySingletonVisibility + "|App.Service|run|private"
	if got := rubyVisibilityFactRows(t, s, repo); got != want {
		t.Fatalf("facts = %q, want %q", got, want)
	}
	writeProfileFile(t, path, rubyConstantPublicSource)
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := rubyVisibilityFactRows(t, s, repo); got != "" {
		t.Fatalf("stale visibility facts survived the reparse: %q", got)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "=App.Service.run|"+"ruby_lexical_constant") {
		t.Fatalf("call did not rebind after the override was removed:\n%s", bindings)
	}
}

// Ruby lets another file make the override, so the resolver reads them per
// owner across the repository -- and a call inside `class << self` uses the
// lexical scope of the class body that encloses the eigenclass.
func TestRubyConstantReceiverCrossFileVisibilityAndEigenclassSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"), `module App
  class Service
    def self.run; end
  end
end
`)
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    class << self
      def f
        Service.run()
      end
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=App.Service.run|ruby_lexical_constant") {
		t.Fatalf("eigenclass source did not use its container's lexical scope:\n%s", bindings)
	}
	// A third file reopens the class and makes the method private. The
	// definition never mentions it, so only a repo-wide read of the owner's
	// facts can see it.
	writeProfileFile(t, filepath.Join(root, "private.rb"), `module App
  class Service
    private_class_method :run
  end
end
`)
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=-|-") {
		t.Fatalf("a cross-file private_class_method did not withdraw the target:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("incremental:\n%s\nfresh:\n%s", got, want)
	}
	// Removing the reopening removes the override, and the call comes back.
	if err := os.Remove(filepath.Join(root, "private.rb")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update after removal: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=App.Service.run|ruby_lexical_constant") {
		t.Fatalf("removing the override did not restore the bind:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("after removal incremental:\n%s\nfresh:\n%s", got, want)
	}
}

// End-to-end wrong-edge controls for the visibility spellings that are easy to
// miss: a `self` receiver really does make the method private, and a form this
// parser cannot attribute withdraws the owner rather than binding.
func TestRubyConstantReceiverVisibilitySpellings(t *testing.T) {
	body := func(lines string) map[string]string {
		return map[string]string{
			"service.rb": "module App\n  class Service\n" + lines + "  end\nend\n",
			"caller.rb": `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`,
		}
	}
	for name, tc := range map[string]struct {
		lines string
		want  string
	}{
		"bare private_class_method": {"    def self.run; end\n    private_class_method :run\n", "-"},
		"self private_class_method": {"    def self.run; end\n    self.private_class_method :run\n", "-"},
		"string argument":           {"    def self.run; end\n    private_class_method \"run\"\n", "-"},
		"eigenclass send":           {"    def self.run; end\n    singleton_class.send(:private, :run)\n", "-"},
		"own constant receiver":     {"    def self.run; end\n    Service.private_class_method :run\n", "-"},
		"class body private":        {"    private\n    def self.run; end\n", "App.Service.run"},
		"public_class_method":       {"    def self.run; end\n    public_class_method :run\n", "App.Service.run"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			for file, content := range body(tc.lines) {
				writeProfileFile(t, filepath.Join(root, file), content)
			}
			s := newProfileStore(t)
			if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			bindings := rubyBindings(t, s, repoID(t, s, root))
			want := "Service.run@4=" + tc.want
			if !strings.Contains(bindings, want) {
				t.Fatalf("%s:\n%s\nwant %s", name, bindings, want)
			}
		})
	}
}

// -- P22.48-F1: constant identity reassignment ---------------------------------

// The P22.48-F1 wrong edge, end to end. Ruby binds `App::Service` to `Other`,
// so `App::Caller.new.f` returns `Other.run` -- the declaration's own singleton
// method is not the target, and no target this graph can prove is.
const rubyConstantReassignedSource = `class Other
  def self.run
    :other
  end
end

module App
  class Service
    def self.run
      :service
    end
  end

  Service = Other

  class Caller
    def f
      Service.run()
    end
  end
end
`

func TestRubyConstantReassignmentFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "app.rb"), rubyConstantReassignedSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	bindings := rubyBindings(t, s, repo)
	if !strings.Contains(bindings, "Service.run@18=-|-") {
		t.Fatalf("a reassigned constant still bound its declaration:\n%s", bindings)
	}
	want := graph.ScopeImportRubyConstantIdentityUnknown + "|App.Service|Service|"
	if got := rubyVisibilityFactRows(t, s, repo); !strings.Contains(got, want) {
		t.Fatalf("facts = %q, want one containing %q", got, want)
	}
}

// The shapes that matter end to end: a reassignment withdraws the identity
// wherever it sits, and nothing that leaves the identity alone does.
func TestRubyConstantIdentityEndToEnd(t *testing.T) {
	caller := `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`
	for name, tc := range map[string]struct {
		target string
		want   string
	}{
		"declaration only": {
			"module App\n  class Service\n    def self.run; end\n  end\nend\n", "App.Service.run"},
		"reassigned after declaration": {
			"module App\n  class Service\n    def self.run; end\n  end\n  Service = Other\nend\n", "-"},
		"reassigned before declaration": {
			"module App\n  Service = Other\n  class Service\n    def self.run; end\n  end\nend\n", "-"},
		"reassigned under a conditional": {
			"module App\n  class Service\n    def self.run; end\n  end\n  if cond\n    Service = Other\n  end\nend\n", "-"},
		"const_set": {
			"module App\n  class Service\n    def self.run; end\n  end\n  const_set(:Service, Other)\nend\n", "-"},
		"remove_const": {
			"module App\n  class Service\n    def self.run; end\n  end\n  remove_const(:Service)\nend\n", "-"},
		// A reassignment inside the class's own body targets a constant nested
		// under it, not the class constant itself.
		"assignment inside the class body": {
			"module App\n  class Service\n    Inner = Other\n    def self.run; end\n  end\nend\n", "App.Service.run"},
		// Ruby raises SyntaxError for a constant assignment in a method, so it
		// is not evidence even though tree-sitter parses it.
		"assignment inside a method": {
			"module App\n  class Service\n    def self.run; end\n  end\n  class Boot\n    def go\n      Service = Other\n    end\n  end\nend\n", "App.Service.run"},
		// The eigenclass is a different cref.
		"assignment inside class << self": {
			"module App\n  class Service\n    def self.run; end\n  end\n  class << self\n    Service = Other\n  end\nend\n", "App.Service.run"},
		"local variable of the same name": {
			"module App\n  class Service\n    def self.run; end\n  end\n  service = Other\nend\n", "App.Service.run"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeProfileFile(t, filepath.Join(root, "caller.rb"), caller)
			writeProfileFile(t, filepath.Join(root, "target.rb"), tc.target)
			s := newProfileStore(t)
			if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			bindings := rubyBindings(t, s, repoID(t, s, root))
			if !strings.Contains(bindings, "Service.run@4="+tc.want) {
				t.Fatalf("%s:\n%s\nwant Service.run@4=%s", name, bindings, tc.want)
			}
		})
	}
}

// A production caller keeps its production constant identity when only spec
// code reassigns it, and loses it when production code does.
func TestRubyConstantIdentityRespectsTestFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	writeProfileFile(t, filepath.Join(root, "spec", "patch_spec.rb"), "module App\n  Service = Fake\nend\n")
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=App.Service.run|ruby_lexical_constant") {
		t.Fatalf("spec-only reassignment erased the production identity:\n%s", bindings)
	}
	// The same reassignment in production code does withdraw it.
	writeProfileFile(t, filepath.Join(root, "patch.rb"), "module App\n  Service = Fake\nend\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=-|-") {
		t.Fatalf("production reassignment did not withdraw the identity:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("incremental:\n%s\nfresh:\n%s", got, want)
	}
}

// Scope evidence can change with no destination method and no caller edit at
// all: a new file holding only `module App; Service = Other; end` declares
// nothing named `Service` or `run`. The bind must still be re-decided, and come
// back when the reassignment is removed and when its file is deleted outright.
func TestRubyConstantIdentityIncrementalHazardLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	patch := filepath.Join(root, "patch.rb")
	step := func(name, want string) {
		t.Helper()
		if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatalf("%s: update: %v", name, err)
		}
		bindings := rubyBindings(t, s, repo)
		if !strings.Contains(bindings, "Service.run@4="+want) {
			t.Fatalf("%s:\n%s\nwant Service.run@4=%s", name, bindings, want)
		}
		if fresh := freshRubyGraph(t, root); bindings != fresh {
			t.Fatalf("%s: incremental:\n%s\nfresh:\n%s", name, bindings, fresh)
		}
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=App.Service.run|ruby_lexical_constant") {
		t.Fatalf("initial bind missing:\n%s", bindings)
	}
	// Add the hazard in a file that names neither the constant nor the method.
	writeProfileFile(t, patch, "module App\n  Service = Other\nend\n")
	step("hazard added", "-")
	// Remove the assignment but keep the file: still no symbol named Service.
	writeProfileFile(t, patch, "module App\n  Unrelated = Other\nend\n")
	step("hazard removed", "App.Service.run")
	writeProfileFile(t, patch, "module App\n  Service = Other\nend\n")
	step("hazard added again", "-")
	// A pure delete has to reconsider the unresolved call.
	if err := os.Remove(patch); err != nil {
		t.Fatal(err)
	}
	step("hazard file deleted", "App.Service.run")
}

// References follow the identity veto exactly as they follow every other one.
func TestRubyConstantIdentityReferencesFollow(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	bound := func() bool {
		t.Helper()
		var n int
		if err := s.raw(t).QueryRowContext(ctx, `
			SELECT COUNT(*) FROM references_tbl
			WHERE repo_id = ? AND name = 'Service.run' AND symbol_id IS NOT NULL`, repo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if !bound() {
		t.Fatal("reference has no destination while the call is bound")
	}
	writeProfileFile(t, filepath.Join(root, "patch.rb"), "module App\n  Service = Other\nend\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bound() {
		t.Fatal("reference kept a destination after the constant identity was withdrawn")
	}
	if err := os.Remove(filepath.Join(root, "patch.rb")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !bound() {
		t.Fatal("reference did not follow the call back")
	}
}

// The identity hazard is a v4 parser fact, so a genuine v3 graph carries none
// and the profile bump alone -- unchanged bytes, no Force, no Paths, every
// repair pre-marked -- must reparse, produce it, keep the call unresolved, and
// land on exactly the from-scratch v4 graph.
func TestRubyProfileV3ToV4RecordsConstantIdentityHazards(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		line   string
	}{
		"reassignment":              {rubyConstantReassignedSource, "Service.run@18"},
		"literal receiver mutation": {rubyConstantMutatedSource, "Service.run@16"},
	} {
		t.Run(name, func(t *testing.T) { rubyProfileV3ToV4Hazard(t, tc.source, tc.line) })
	}
}

func rubyProfileV3ToV4Hazard(t *testing.T, source, line string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "app.rb")
	writeProfileFile(t, path, source)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(rubyV3Adapter{tsparser.NewRuby()}), nil).
		Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v3 index: %v", err)
	}
	repo := repoID(t, s, root)
	if got := rubyVisibilityFactRows(t, s, repo); got != "" {
		t.Fatalf("v3 fixture already carries P22.48 facts:\n%s", got)
	}
	if err := s.Store.MarkResolverBindingsRepaired(ctx, repo); err != nil {
		t.Fatal(err)
	}

	summary, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("v4 update: %v", err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "ruby" {
		t.Fatalf("changed=%d languages=%v; the profile bump alone must reparse", summary.FilesChanged, summary.ParserProfileLanguages)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("the test modified the source file")
	}
	if groups := profilesInDB(t, s, repo); len(groups) != 1 || groups[0].Profile != "treesitter:ruby:v5" || !groups[0].CallEdges {
		t.Fatalf("provenance after upgrade = %#v", groups)
	}
	want := graph.ScopeImportRubyConstantIdentityUnknown + "|App.Service|Service|"
	if got := rubyVisibilityFactRows(t, s, repo); !strings.Contains(got, want) {
		t.Fatalf("facts after upgrade = %q, want one containing %q", got, want)
	}
	bindings := rubyBindings(t, s, repo)
	if !strings.Contains(bindings, line+"=-|-") {
		t.Fatalf("the mutated constant bound after the upgrade:\n%s", bindings)
	}
	if fresh := freshRubyGraph(t, root); bindings != fresh {
		t.Fatalf("upgraded graph:\n%s\nfrom-scratch v4 graph:\n%s", bindings, fresh)
	}
}

func TestRubyProfileV4ToV5ConvergesConstantFacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := `class Service
  def self.run; end
end
Service = Other
module App
  class Service; end
  private_constant :Service
end
Object.const_set(:Root, Other)
`
	writeProfileFile(t, filepath.Join(root, "app.rb"), source)
	s := newProfileStore(t)
	old := New(s.Store, parser.NewRegistry(rubyV4Adapter{tsparser.NewRuby()}), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := rubyVisibilityFactRows(t, s, repo); got != "" {
		t.Fatalf("v4 fixture carries v5 facts: %s", got)
	}
	if err := s.Store.MarkResolverBindingsRepaired(ctx, repo); err != nil {
		t.Fatal(err)
	}
	upgraded := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 1 || summary.FilesIndexed != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "ruby" {
		t.Fatalf("summary = %+v", summary)
	}
	if got := rubyVisibilityFactRows(t, s, repo); !strings.Contains(got, "ruby_constant_identity_unknown|Service|Service|") || !strings.Contains(got, "ruby_constant_visibility|App|Service|private") {
		t.Fatalf("v5 facts = %q", got)
	}
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got, want := rubyVisibilityFactRows(t, s, repo), rubyVisibilityFactRows(t, fresh, repoID(t, fresh, root)); got != want {
		t.Fatalf("upgraded facts:\n%s\nfresh facts:\n%s", got, want)
	}
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
}

// A reassignment written inside a wrapper class or module, and one inside a
// class nested under control flow, are the shapes a direct-child walk or a
// single-reading qname would miss -- and both are the F1 wrong edge.
func TestRubyConstantIdentityReachesEveryOwnerShape(t *testing.T) {
	caller := `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`
	for name, patch := range map[string]string{
		"qualified target inside a class":  "class Boot\n  App::Service = Other\nend\n",
		"qualified target inside a module": "module Boot\n  App::Service = Other\nend\n",
		"absolute target inside a class":   "class Boot\n  ::App::Service = Other\nend\n",
		"qualified target at root":         "App::Service = Other\n",
		"reassignment under a conditional": "module App\n  if cond\n    Service = Other\n  end\nend\n",
		"reassignment inside a block":      "module App\n  [1].each do\n    Service = Other\n  end\nend\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
			writeProfileFile(t, filepath.Join(root, "caller.rb"), caller)
			writeProfileFile(t, filepath.Join(root, "patch.rb"), patch)
			s := newProfileStore(t)
			if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			if bindings := rubyBindings(t, s, repoID(t, s, root)); !strings.Contains(bindings, "Service.run@4=-|-") {
				t.Fatalf("%s did not withdraw the identity:\n%s", name, bindings)
			}
		})
	}
}

// A class nested under control flow is a lexical owner the symbol walk never
// sees, so an inner reassignment there has to be found by the assignment scan.
func TestRubyConstantIdentityInsideControlFlowNestedClass(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"),
		"module App\n  module Inner\n    class Service\n      def self.run; end\n    end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  module Inner
    class Caller
      def f
        Service.run()
      end
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=App.Inner.Service.run|ruby_lexical_constant") {
		t.Fatalf("initial bind missing:\n%s", bindings)
	}
	writeProfileFile(t, filepath.Join(root, "patch.rb"),
		"module App\n  if cond\n    module Inner\n      Service = Other\n    end\n  end\nend\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=-|-") {
		t.Fatalf("a reassignment in a control-flow-nested module did not withdraw the identity:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("incremental:\n%s\nfresh:\n%s", got, want)
	}
}

// The invalidation cannot key on declared symbol names alone: a hazard file may
// declare none at all, or none that shares a segment with the hazard qname.
func TestRubyConstantIdentityIncrementalWithoutMatchingSymbolNames(t *testing.T) {
	for name, patch := range map[string]string{
		"file declares no symbol":      "App::Service = Other\n",
		"file declares unrelated name": "class Boot\n  App::Service = Other\nend\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
			writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
			s := newProfileStore(t)
			idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
			if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			repo := repoID(t, s, root)
			if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=App.Service.run|ruby_lexical_constant") {
				t.Fatalf("initial bind missing:\n%s", bindings)
			}
			patchPath := filepath.Join(root, "patch.rb")
			writeProfileFile(t, patchPath, patch)
			if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("update: %v", err)
			}
			bindings := rubyBindings(t, s, repo)
			if !strings.Contains(bindings, "Service.run@4=-|-") {
				t.Fatalf("adding the hazard did not invalidate the bind:\n%s", bindings)
			}
			if fresh := freshRubyGraph(t, root); bindings != fresh {
				t.Fatalf("incremental:\n%s\nfresh:\n%s", bindings, fresh)
			}
			if err := os.Remove(patchPath); err != nil {
				t.Fatal(err)
			}
			if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("update after removal: %v", err)
			}
			bindings = rubyBindings(t, s, repo)
			if !strings.Contains(bindings, "Service.run@4=App.Service.run|ruby_lexical_constant") {
				t.Fatalf("removing the hazard did not restore the bind:\n%s", bindings)
			}
			if fresh := freshRubyGraph(t, root); bindings != fresh {
				t.Fatalf("after removal incremental:\n%s\nfresh:\n%s", bindings, fresh)
			}
		})
	}
}

// -- P22.48-F2: literal constant receiver mutations ---------------------------

// The F2 wrong edge, end to end. `App.const_set(:Service, Other)` makes
// `App::Service == Other`, so `App::Caller.new.f` returns `Other.run` -- and a
// mutation whose receiver is a literal constant path is exactly as
// syntax-proven as a bare assignment inside the module body.
const rubyConstantMutatedSource = `class Other
  def self.run
    :other
  end
end

module App
  class Service
    def self.run
      :service
    end
  end

  class Caller
    def f
      Service.run()
    end
  end
end

App.const_set(:Service, Other)
`

func TestRubyLiteralConstantMutationFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "app.rb"), rubyConstantMutatedSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@16=-|-") {
		t.Fatalf("a const_set through a literal constant receiver still bound:\n%s", bindings)
	}
	want := graph.ScopeImportRubyConstantIdentityUnknown + "|App.Service|Service|"
	if got := rubyVisibilityFactRows(t, s, repo); !strings.Contains(got, want) {
		t.Fatalf("facts = %q, want one containing %q", got, want)
	}
}

// Every receiver shape, decided end to end against the same caller.
func TestRubyLiteralConstantMutationReceiverShapes(t *testing.T) {
	caller := `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`
	for name, tc := range map[string]struct {
		patch string
		want  string
	}{
		"root const_set":            {"App.const_set(:Service, Other)\n", "-"},
		"root remove_const":         {"App.remove_const(:Service)\n", "-"},
		"root autoload":             {"App.autoload(:Service, \"service\")\n", "-"},
		"root string name":          {"App.const_set(\"Service\", Other)\n", "-"},
		"absolute receiver":         {"::App.const_set(:Service, Other)\n", "-"},
		"absolute inside a wrapper": {"module Boot\n  ::App.const_set(:Service, Other)\nend\n", "-"},
		"relative inside a wrapper": {"module Boot\n  App.const_set(:Service, Other)\nend\n", "-"},
		"self inside the module":    {"module App\n  self.const_set(:Service, Other)\nend\n", "-"},
		"bare inside the module":    {"module App\n  const_set(:Service, Other)\nend\n", "-"},
		// A qualified receiver moves a constant under that path, so
		// `App::Service` is untouched and the call still binds.
		"qualified receiver elsewhere": {"App::Nested.const_set(:Service, Other)\n", "App.Service.run"},
		// Deferred shapes prove nothing and must not veto.
		"value receiver":   {"target.const_set(:Service, Other)\n", "App.Service.run"},
		"chained receiver": {"factory.module.const_set(:Service, Other)\n", "App.Service.run"},
		"send indirection": {"App.send(:const_set, :Service, Other)\n", "App.Service.run"},
		"dynamic name":     {"App.const_set(name, Other)\n", "App.Service.run"},
		"unrelated method": {"App.configure(:Service)\n", "App.Service.run"},
		// The shape an installer actually takes: the receiver proves the target
		// whether or not the surrounding cref is nameable.
		"receiver mutation in a singleton method": {"module Boot\n  def self.install\n    App.const_set(:Service, Other)\n  end\nend\n", "-"},
		"receiver mutation in an instance method": {"module Boot\n  def install\n    App.const_set(:Service, Other)\n  end\nend\n", "-"},
		"receiver mutation in an eigenclass":      {"module Boot\n  class << self\n    App.const_set(:Service, Other)\n  end\nend\n", "-"},
		"receiver mutation in a relative open":    {"module Boot\n  class Outer::Wrap\n    App.const_set(:Service, Other)\n  end\nend\n", "-"},
		"cref mutation in a singleton method":     {"module App\n  def self.install\n    const_set(:Service, Other)\n  end\nend\n", "-"},
		// `self` in an instance method has no const_set at all.
		"cref mutation in an instance method": {"module App\n  def install\n    const_set(:Service, Other)\n  end\nend\n", "App.Service.run"},
		// A root-object receiver names its own constant, so `App::Service` is
		// untouched and the call still binds. The hazard it does record
		// (`Object.Service`) is proven by TestRubyRootObjectMutationFailsClosed.
		"Object receiver": {"Object.const_set(:Service, Other)\n", "App.Service.run"},
		"Kernel receiver": {"Kernel.const_set(:Service, Other)\n", "App.Service.run"},
		// Paren-less and safe-navigation spellings still count.
		"paren-less":      {"App.const_set :Service, Other\n", "-"},
		"safe navigation": {"App&.const_set(:Service, Other)\n", "-"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
			writeProfileFile(t, filepath.Join(root, "caller.rb"), caller)
			writeProfileFile(t, filepath.Join(root, "patch.rb"), tc.patch)
			s := newProfileStore(t)
			if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			if bindings := rubyBindings(t, s, repoID(t, s, root)); !strings.Contains(bindings, "Service.run@4="+tc.want) {
				t.Fatalf("%s:\n%s\nwant Service.run@4=%s", name, bindings, tc.want)
			}
		})
	}
}

// A qualified receiver names a constant under its own path, and a caller nested
// there loses exactly that one.
func TestRubyLiteralConstantMutationQualifiedReceiverTargetsItsOwnPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"),
		"module App\n  module Nested\n    class Service\n      def self.run; end\n    end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  module Nested
    class Caller
      def f
        Service.run()
      end
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=App.Nested.Service.run|ruby_lexical_constant") {
		t.Fatalf("initial bind missing:\n%s", bindings)
	}
	writeProfileFile(t, filepath.Join(root, "patch.rb"), "App::Nested.const_set(:Service, Other)\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@5=-|-") {
		t.Fatalf("a qualified-receiver mutation did not withdraw its own path:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("incremental:\n%s\nfresh:\n%s", got, want)
	}
}

// P7: a root-level literal mutation in a spec file must not veto a production
// caller, and the same mutation in production code must.
func TestRubyLiteralConstantMutationRespectsTestFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	writeProfileFile(t, filepath.Join(root, "spec", "patch_spec.rb"), "App.const_set(:Service, Fake)\n")
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=App.Service.run|ruby_lexical_constant") {
		t.Fatalf("a spec-only const_set erased the production identity:\n%s", bindings)
	}
	writeProfileFile(t, filepath.Join(root, "patch.rb"), "App.const_set(:Service, Fake)\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@4=-|-") {
		t.Fatalf("a production const_set did not withdraw the identity:\n%s", bindings)
	}
	if got, want := rubyBindings(t, s, repo), freshRubyGraph(t, root); got != want {
		t.Fatalf("incremental:\n%s\nfresh:\n%s", got, want)
	}
}

// A patch file holding nothing but `App.const_set(:Service, Other)` declares no
// symbol at all, so the invalidation has to come from the hazard itself. The
// reference follows the call in both directions.
func TestRubyLiteralConstantMutationIncrementalLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"), "module App\n  class Service\n    def self.run; end\n  end\nend\n")
	writeProfileFile(t, filepath.Join(root, "caller.rb"), `module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	patch := filepath.Join(root, "patch.rb")
	referenceBound := func() bool {
		t.Helper()
		var n int
		if err := s.raw(t).QueryRowContext(ctx, `
			SELECT COUNT(*) FROM references_tbl
			WHERE repo_id = ? AND name = 'Service.run' AND symbol_id IS NOT NULL`, repo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	step := func(name, want string) {
		t.Helper()
		if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatalf("%s: update: %v", name, err)
		}
		bindings := rubyBindings(t, s, repo)
		if !strings.Contains(bindings, "Service.run@4="+want) {
			t.Fatalf("%s:\n%s\nwant Service.run@4=%s", name, bindings, want)
		}
		if fresh := freshRubyGraph(t, root); bindings != fresh {
			t.Fatalf("%s: incremental:\n%s\nfresh:\n%s", name, bindings, fresh)
		}
		if got := referenceBound(); got != (want != "-") {
			t.Fatalf("%s: reference bound = %v, want %v", name, got, want != "-")
		}
	}
	if !referenceBound() {
		t.Fatal("reference has no destination while the call is bound")
	}
	writeProfileFile(t, patch, "App.const_set(:Service, Other)\n")
	step("mutation added", "-")
	if err := os.Remove(patch); err != nil {
		t.Fatal(err)
	}
	step("mutation file deleted", "App.Service.run")
	writeProfileFile(t, patch, "App.const_set(:Service, Other)\n")
	step("mutation restored", "-")
	writeProfileFile(t, patch, "App.const_set(name, Other)\n")
	step("mutation became dynamic", "App.Service.run")
}

// -- P22.48-F3: root-object receivers -----------------------------------------

// `Object`, `Kernel` and `BasicObject` are ordinary constant receivers. Each
// names its OWN constant, and this parser's semantic qnames already make
// `Kernel.Service` a lexical candidate for a caller inside `module Kernel`, so
// skipping those receivers left a confidently wrong edge:
//
//	Kernel::Service == Other  and  Kernel::Caller.new.f == :other
//
// verified against the real interpreter for all three.
func TestRubyRootObjectMutationFailsClosed(t *testing.T) {
	for _, owner := range []struct{ keyword, name string }{
		{"module", "Kernel"},
		{"class", "BasicObject"},
		{"class", "Object"},
	} {
		t.Run(owner.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeProfileFile(t, filepath.Join(root, "app.rb"), rubyRootObjectSource(owner.keyword, owner.name, owner.name+".const_set(:Service, Other)\n"))
			s := newProfileStore(t)
			if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
				t.Fatalf("index: %v", err)
			}
			repo := repoID(t, s, root)
			if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@16=-|-") {
				t.Fatalf("%s.const_set did not withdraw %s::Service:\n%s", owner.name, owner.name, bindings)
			}
			want := graph.ScopeImportRubyConstantIdentityUnknown + "|" + owner.name + ".Service|Service|"
			if got := rubyVisibilityFactRows(t, s, repo); !strings.Contains(got, want) {
				t.Fatalf("facts = %q, want one containing %q", got, want)
			}
		})
	}
}

// rubyRootObjectSource is the F3 fixture: a caller nested in a root-object
// owner reaching that owner's own `Service`, plus one trailing patch line.
func rubyRootObjectSource(keyword, name, patch string) string {
	return `class Other
  def self.run
    :other
  end
end

` + keyword + ` ` + name + `
  class Service
    def self.run
      :service
    end
  end

  class Caller
    def f
      Service.run()
    end
  end
end

` + patch
}

// The hazard is exact: mutating one root object's constant says nothing about
// another's, and it must not turn into a root fallback that lets an unrelated
// caller start resolving a global constant.
func TestRubyRootObjectMutationIsExact(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// A caller inside `module Kernel`, and an unrelated caller inside `module
	// App` whose only tempting target is a ROOT `Service`.
	writeProfileFile(t, filepath.Join(root, "kernel.rb"), rubyRootObjectSource("module", "Kernel", ""))
	writeProfileFile(t, filepath.Join(root, "global.rb"), `class Service
  def self.run; end
end

module App
  class Caller
    def f
      Service.run()
    end
  end
end
`)
	writeProfileFile(t, filepath.Join(root, "patch.rb"), "Object.const_set(:Service, Other)\n")
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	// The whole answer, asserted exactly: rubyBindings renders no file path, so
	// two Contains probes would be satisfiable by the wrong file's edge.
	// Object's mutation is not Kernel's, so Kernel::Service still binds; and no
	// root fallback appeared, so `App::Caller` still reaches nothing.
	const want = "Service.run@16=Kernel.Service.run|ruby_lexical_constant\nService.run@8=-|-"
	if got := rubyBindings(t, s, repoID(t, s, root)); got != want {
		t.Fatalf("bindings:\n%s\nwant:\n%s", got, want)
	}
}

// The root-object hazards follow the same lifecycle as every other one:
// production/test origin, soft delete, add/delete/restore, references.
func TestRubyRootObjectMutationLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "kernel.rb"), rubyRootObjectSource("module", "Kernel", ""))
	// A spec-only mutation must not veto the production caller.
	writeProfileFile(t, filepath.Join(root, "spec", "patch_spec.rb"), "Kernel.const_set(:Service, Other)\n")
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo := repoID(t, s, root)
	patch := filepath.Join(root, "patch.rb")
	referenceBound := func() bool {
		t.Helper()
		var n int
		if err := s.raw(t).QueryRowContext(ctx, `
			SELECT COUNT(*) FROM references_tbl
			WHERE repo_id = ? AND name = 'Service.run' AND symbol_id IS NOT NULL`, repo).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	step := func(name, want string) {
		t.Helper()
		if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatalf("%s: update: %v", name, err)
		}
		bindings := rubyBindings(t, s, repo)
		if !strings.Contains(bindings, "Service.run@16="+want) {
			t.Fatalf("%s:\n%s\nwant Service.run@16=%s", name, bindings, want)
		}
		if fresh := freshRubyGraph(t, root); bindings != fresh {
			t.Fatalf("%s: incremental:\n%s\nfresh:\n%s", name, bindings, fresh)
		}
		if got := referenceBound(); got != (want != "-") {
			t.Fatalf("%s: reference bound = %v, want %v", name, got, want != "-")
		}
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@16=Kernel.Service.run|ruby_lexical_constant") {
		t.Fatalf("a spec-only Kernel.const_set erased the production identity:\n%s", bindings)
	}
	writeProfileFile(t, patch, "Kernel.const_set(:Service, Other)\n")
	step("production mutation added", "-")
	if err := os.Remove(patch); err != nil {
		t.Fatal(err)
	}
	step("mutation file deleted", "Kernel.Service.run")
	writeProfileFile(t, patch, "Kernel.const_set(:Service, Other)\n")
	step("mutation restored", "-")
	// A soft-deleted patch states nothing: same end state as the hard delete.
	if _, err := s.raw(t).ExecContext(ctx,
		`UPDATE files SET is_deleted = 1 WHERE repo_id = ? AND path = 'patch.rb'`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ResolveEdges(ctx, repo); err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if bindings := rubyBindings(t, s, repo); !strings.Contains(bindings, "Service.run@16=Kernel.Service.run|ruby_lexical_constant") {
		t.Fatalf("a soft-deleted patch kept vetoing:\n%s", bindings)
	}
}
