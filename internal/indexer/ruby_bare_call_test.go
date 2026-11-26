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

const rubyBareCallRepoSource = `class Widget
  def helper; end

  def bare_call
    helper
  end

  def shadowed
    helper = 1
    helper
  end

  def parameter(helper)
    helper
  end
end
`

// rubyV6BareAdapter stands in for treesitter:ruby:v6, which emitted no edge for
// an argument-less bare identifier.
type rubyV6BareAdapter struct{ *tsparser.RubyAdapter }

func (rubyV6BareAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v6", EmitsCallEdges: true}
}

func (a rubyV6BareAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.RubyAdapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	edges := pf.Edges[:0]
	for _, e := range pf.Edges {
		if !(e.DstName == "helper" && e.Line == 5) {
			edges = append(edges, e)
		}
	}
	pf.Edges = edges
	refs := pf.References[:0]
	for _, r := range pf.References {
		if !(r.Name == "helper" && r.Range.StartLine == 5) {
			refs = append(refs, r)
		}
	}
	pf.References = refs
	return pf, nil
}

func rubyBareCallGraph(t *testing.T, s *profileStore, repo int64) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT src.qualified_name, e.dst_name, e.line, COALESCE(dst.qualified_name, '-'), COALESCE(e.resolution_strategy, '')
		FROM edges e JOIN symbols src ON src.id = e.src_symbol_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
		WHERE e.repo_id = ? AND e.edge_kind = 'calls'
		ORDER BY e.line, e.dst_name`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, dst, target, strategy string
		var line int
		if err := rows.Scan(&src, &dst, &line, &target, &strategy); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{src, dst, target, strategy}, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

func TestRubyBareCallResolvesAndLocalsDoNot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "widget.rb"), rubyBareCallRepoSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got, want := rubyBareCallGraph(t, s, repoID(t, s, root)), "Widget.bare_call|helper|Widget.helper|ruby_implicit_self"; got != want {
		t.Fatalf("call graph:\n%s\nwant:\n%s", got, want)
	}
}

// Editing a method so that the bare name becomes a local must drop its edge,
// and the incremental graph must equal a fresh index of the same bytes.
func TestRubyBareCallIncrementalMatchesFresh(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "widget.rb")
	writeProfileFile(t, path, rubyBareCallRepoSource)
	s := newProfileStore(t)
	ix := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := ix.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []string{
		strings.Replace(rubyBareCallRepoSource, "  def bare_call\n    helper\n", "  def bare_call\n    helper = 2\n    helper\n", 1),
		rubyBareCallRepoSource,
	} {
		writeProfileFile(t, path, edit)
		if _, err := ix.Update(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatal(err)
		}
		fresh := newProfileStore(t)
		if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
			t.Fatal(err)
		}
		got, want := rubyBareCallGraph(t, s, repoID(t, s, root)), rubyBareCallGraph(t, fresh, repoID(t, fresh, root))
		if got != want {
			t.Fatalf("incremental graph:\n%s\nfresh graph:\n%s", got, want)
		}
		if strings.Contains(edit, "helper = 2") && got != "" {
			t.Fatalf("a local read kept a call edge:\n%s", got)
		}
	}
}

// The profile bump alone must reparse an unchanged Ruby file and add the edge.
func TestRubyBareCallProfileBumpReparsesUnchangedFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "widget.rb"), rubyBareCallRepoSource)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(rubyV6BareAdapter{tsparser.NewRuby()}), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := rubyBareCallGraph(t, s, repo); got != "" {
		t.Fatalf("v6 stand-in graph holds calls: %s", got)
	}
	upgraded := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "ruby" {
		t.Fatalf("update changed=%d languages=%v; the profile bump must reparse the unchanged file",
			summary.FilesChanged, summary.ParserProfileLanguages)
	}
	if got, want := rubyBareCallGraph(t, s, repo), "Widget.bare_call|helper|Widget.helper|ruby_implicit_self"; got != want {
		t.Fatalf("call graph after upgrade:\n%s\nwant:\n%s", got, want)
	}
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reparsed: changed=%d languages=%v", again.FilesChanged, again.ParserProfileLanguages)
	}
}
