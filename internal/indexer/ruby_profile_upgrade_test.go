//go:build cgo

package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// rubyUpgradeSource holds one instance of every call shape treesitter:ruby:v3
// refuses to emit, plus the direct implicit/self control that must survive.
const rubyUpgradeSource = `class Service
  attr_accessor :name

  def run; end

  def rename
    self.name = "x"
  end

  def rebound(other)
    other.instance_eval do
      run()
    end
  end

  def build
    Class.new do
      def go; run(); end
    end
  end

  def direct
    run()
    self.run()
  end
end
`

// rubyV2Adapter reproduces the treesitter:ruby:v2 call-edge set for
// rubyUpgradeSource: everything v3 emits, plus the three shapes v3 now refuses.
// It stamps the v2 profile id, which is the only thing planParserProfiles
// compares, so a repository indexed with it is a genuine pre-P22.47 graph.
type rubyV2Adapter struct {
	*tsparser.RubyAdapter
}

func (rubyV2Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:ruby:v2", EmitsCallEdges: true}
}

// rubyV2OnlyEdges are the call edges v2 emitted and v3 does not, keyed by the
// name the old parser gave them. Lines match rubyUpgradeSource.
var rubyV2OnlyEdges = []graph.Edge{
	// `self.name = "x"` invokes the writer `name=`, but v2 named the reader.
	{DstName: "self.name", Kind: "calls", Evidence: "ruby:self_receiver", Line: 7},
	// `self` inside instance_eval is `other`, not the Service instance.
	{DstName: "run", Kind: "calls", Evidence: "ruby:implicit_receiver", Line: 12},
	// `def go` lives in the anonymous class, not in Service#build.
	{DstName: "run", Kind: "calls", Evidence: "ruby:implicit_receiver", Line: 18},
}

func (a rubyV2Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.RubyAdapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	pf.Edges = append(pf.Edges, rubyV2OnlyEdges...)
	return pf, nil
}

type rubyCallRow struct {
	dstName  string
	evidence string
	line     int
	resolved bool
}

func rubyCallRows(t *testing.T, s *profileStore, repo int64) []rubyCallRow {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT e.dst_name, e.evidence, e.line, e.dst_symbol_id IS NOT NULL
		FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND f.language = 'ruby' AND e.edge_kind = 'calls'
		ORDER BY e.line, e.dst_name`, repo)
	if err != nil {
		t.Fatalf("read ruby calls: %v", err)
	}
	defer rows.Close()
	var out []rubyCallRow
	for rows.Next() {
		var r rubyCallRow
		if err := rows.Scan(&r.dstName, &r.evidence, &r.line, &r.resolved); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func rubyCallSet(rows []rubyCallRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.dstName+"@"+strconv.Itoa(r.line)] = r.resolved
	}
	return out
}

// A parser profile is the compatibility boundary for parser semantics, so
// bumping Ruby to v3 must be enough on its own -- no resolver repair, no
// --force, no touched file -- to replace a v2 graph's call edges with what the
// current parser actually emits. The three shapes v3 refuses must be gone from
// the graph, not merely unbound, and the direct implicit/self calls must come
// back correctly resolved.
func TestRubyProfileV2ToV3ReplacesUnsafeCallEdges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "service.rb")
	writeProfileFile(t, path, rubyUpgradeSource)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	s := newProfileStore(t)
	old := New(s.Store, parser.NewRegistry(rubyV2Adapter{tsparser.NewRuby()}), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v2 index: %v", err)
	}
	repo := repoID(t, s, root)

	// The v2 graph really does hold the unsafe edges.
	v2 := rubyCallSet(rubyCallRows(t, s, repo))
	for _, want := range []string{"self.name@7", "run@12", "run@18"} {
		if _, ok := v2[want]; !ok {
			t.Fatalf("v2 fixture is missing %s; it cannot prove the upgrade removes it (%v)", want, v2)
		}
	}
	// Give the assignment-target edge the kind of destination an older resolver
	// left behind, so the upgrade has a non-NULL stale target to erase.
	if _, err := s.raw(t).ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = (SELECT id FROM symbols WHERE qualified_name = 'Service.run' LIMIT 1),
			resolution_strategy = 'exact_name', resolution_confidence = 'high'
		WHERE repo_id = ? AND dst_name = 'self.name'`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.raw(t).ExecContext(ctx, `
		UPDATE references_tbl SET symbol_id = (SELECT id FROM symbols WHERE qualified_name = 'Service.run' LIMIT 1)
		WHERE repo_id = ? AND name = 'self.name'`, repo); err != nil {
		t.Fatal(err)
	}
	// Every resolver repair is already marked done, as it would be in a
	// database this old (the first index marks them all). Convergence must not
	// depend on clearing or re-running any of them.
	if err := s.Store.MarkResolverBindingsRepaired(ctx, repo); err != nil {
		t.Fatal(err)
	}

	// The upgrade: same bytes, same store, no Force, no Paths.
	upgraded := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("v3 update: %v", err)
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
		t.Fatalf("the test modified the source file: %v/%d -> %v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	groups := profilesInDB(t, s, repo)
	if len(groups) != 1 || groups[0].Profile != tsparser.NewRuby().Profile().ID || !groups[0].CallEdges {
		t.Fatalf("provenance after upgrade = %#v", groups)
	}

	// The unsafe edges are gone from the graph, not merely unresolved.
	v3 := rubyCallSet(rubyCallRows(t, s, repo))
	for _, gone := range []string{"self.name@7", "run@12", "run@18"} {
		if _, ok := v3[gone]; ok {
			t.Fatalf("%s survived the v2 -> v3 upgrade: %v", gone, v3)
		}
	}
	// The direct control binds.
	for _, want := range []string{"run@23", "self.run@24"} {
		resolved, ok := v3[want]
		if !ok {
			t.Fatalf("%s is missing after the upgrade: %v", want, v3)
		}
		if !resolved {
			t.Fatalf("%s is unresolved after the upgrade", want)
		}
	}

	// No reference keeps the destination the removed edge used to carry.
	// The v2 graph bound references for the instance_eval and block-local `def`
	// calls outright, and the assignment target was given one above. None of
	// the three may keep a destination whose edge no longer exists.
	var stale int
	if err := s.raw(t).QueryRowContext(ctx, `
		SELECT COUNT(*) FROM references_tbl
		WHERE repo_id = ? AND start_line IN (7, 12, 18) AND symbol_id IS NOT NULL`, repo).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d references kept the destination of an edge v3 no longer emits", stale)
	}

	// The upgraded graph is exactly what a from-scratch v3 index produces.
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewRuby()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("fresh v3 index: %v", err)
	}
	freshRepo := repoID(t, fresh, root)
	if got, want := renderRubyCalls(rubyCallRows(t, s, repo)), renderRubyCalls(rubyCallRows(t, fresh, freshRepo)); got != want {
		t.Fatalf("upgraded graph:\n%s\nfrom-scratch v3 graph:\n%s", got, want)
	}

	// Idempotent: the bump drove the reparse, nothing else does it again.
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reparsed: changed=%d indexed=%d languages=%v",
			again.FilesChanged, again.FilesIndexed, again.ParserProfileLanguages)
	}
}

func renderRubyCalls(rows []rubyCallRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.dstName+"|"+r.evidence+"|"+strconv.Itoa(r.line)+"|"+strconv.FormatBool(r.resolved))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// P22.29: a call-capable Ruby graph never degrades to the call-less heuristic
// parser, and a refused scan changes nothing. The stored side is the real v3
// profile rather than a literal, so the check cannot rot with the next bump.
func TestRubyProfileNoCgoDowngradeStillRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "service.rb"), rubyUpgradeSource)
	s := newProfileStore(t)
	capable := New(s.Store, parser.NewRegistry(tsparser.NewRuby()), nil)
	if _, err := capable.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	groups := profilesInDB(t, s, repoID(t, s, root))
	if len(groups) != 1 || groups[0].Profile != tsparser.NewRuby().Profile().ID || !groups[0].CallEdges {
		t.Fatalf("stored provenance = %#v", groups)
	}
	before := graphSnapshot(t, s)
	degraded := New(s.Store, parser.NewRegistry(symbolsOnly("ruby", ".rb", "heuristic:ruby:v1")), nil)
	for _, opts := range []Options{
		{RepoRoot: root},
		{RepoRoot: root, Force: true},
		{RepoRoot: root, Paths: []string{"service.rb"}},
	} {
		if _, err := degraded.Update(ctx, opts); !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("Update(%+v) err = %v, want ErrParserDowngradeRefused", opts, err)
		}
		if _, err := degraded.Index(ctx, opts); !errors.Is(err, ErrParserDowngradeRefused) {
			t.Fatalf("Index(%+v) err = %v, want ErrParserDowngradeRefused", opts, err)
		}
	}
	if after := graphSnapshot(t, s); before != after {
		t.Fatalf("graph mutated by a refused downgrade")
	}
}
