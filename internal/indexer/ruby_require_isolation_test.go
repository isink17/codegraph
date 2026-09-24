//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// Ruby require strings stay in file_imports as metadata, but no generic import
// consumer may read them: this is the end-to-end shape of the A0 wrong edge, a
// Ruby `require_relative` bridged to a Python file Ruby can never load.

func rubyRequireRegistry() *parser.Registry {
	return parser.NewRegistry(tsparser.NewRuby(), tsparser.NewPython(), tsparser.NewTypeScript())
}

func newRubyRequireRepo(t *testing.T, files tree) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "codegraph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: dbPath, store: s, idx: New(s, rubyRequireRegistry(), nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *lifecycleRepo) assertRubyFreshParity(t *testing.T, step string) {
	t.Helper()
	fresh := newRubyRequireRepo(t, r.currentTree(t))
	got, want := strings.Join(r.projection(t), "\n"), strings.Join(fresh.projection(t), "\n")
	if got != want {
		t.Fatalf("%s: incremental/fresh mismatch\nincremental:\n%s\nfresh:\n%s", step, got, want)
	}
}

func (r *lifecycleRepo) raw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func rubySourceCrossLanguageEdges(t *testing.T, r *lifecycleRepo) int {
	t.Helper()
	var n int
	if err := r.raw(t).QueryRowContext(r.ctx, `
		SELECT COUNT(*) FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND e.edge_kind = ? AND f.language = 'ruby'`,
		r.repoID, store.EdgeKindCrossLanguageRef).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rubyImportRows(t *testing.T, r *lifecycleRepo, path string) []string {
	t.Helper()
	rows, err := r.raw(t).QueryContext(r.ctx, `
		SELECT fi.import_path FROM file_imports fi JOIN files f ON f.id = fi.file_id
		WHERE f.repo_id = ? AND f.path = ? AND f.is_deleted = 0 ORDER BY fi.import_path`, r.repoID, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			t.Fatal(err)
		}
		out = append(out, spec)
	}
	return out
}

const rubyRequireCaller = "require \"foo\"\nrequire_relative \"../tools/report\"\nmodule App\n  def self.render_report; end\nend\n"

func rubyRequireTree() tree {
	return tree{
		"app/caller.rb":   rubyRequireCaller,
		"tools/report.py": "def render_report():\n    pass\n",
	}
}

func TestRubyRequireRowsPersistButBridgeNothing(t *testing.T) {
	r := newRubyRequireRepo(t, rubyRequireTree())
	if got := strings.Join(rubyImportRows(t, r, "app/caller.rb"), ","); got != "../tools/report,foo" {
		t.Fatalf("Ruby file_imports = %q, want both require rows persisted", got)
	}
	if got := rubySourceCrossLanguageEdges(t, r); got != 0 {
		t.Fatalf("fresh index: Ruby-source cross_language_ref = %d, want 0", got)
	}
	current, err := r.store.CrossLanguageLinksCurrent(r.ctx, r.repoID)
	if err != nil || !current {
		t.Fatalf("fresh index v2 marker current = %v, %v", current, err)
	}

	// Changing the require to another path-shaped spelling, including the
	// explicit foreign extension, still bridges nothing.
	r.write(t, "app/caller.rb", strings.Replace(rubyRequireCaller, `"../tools/report"`, `"../tools/report.py"`, 1))
	r.update(t)
	if got := strings.Join(rubyImportRows(t, r, "app/caller.rb"), ","); got != "../tools/report.py,foo" {
		t.Fatalf("changed require rows = %q", got)
	}
	if got := rubySourceCrossLanguageEdges(t, r); got != 0 {
		t.Fatalf("changed require: Ruby-source cross_language_ref = %d, want 0", got)
	}
	r.assertRubyFreshParity(t, "changed require")

	// Deleting the require retires its row through ordinary replacement.
	r.write(t, "app/caller.rb", "require \"foo\"\nmodule App\n  def self.render_report; end\nend\n")
	r.update(t)
	if got := strings.Join(rubyImportRows(t, r, "app/caller.rb"), ","); got != "foo" {
		t.Fatalf("deleted require rows = %q, want foo only", got)
	}
	if got := rubySourceCrossLanguageEdges(t, r); got != 0 {
		t.Fatalf("deleted require: Ruby-source cross_language_ref = %d, want 0", got)
	}
	r.assertRubyFreshParity(t, "deleted require")

	// Deleting the importer retires everything it held.
	r.remove(t, "app/caller.rb")
	r.update(t)
	if got := rubyImportRows(t, r, "app/caller.rb"); len(got) != 0 {
		t.Fatalf("deleted file still has import rows %v", got)
	}
	if got := rubySourceCrossLanguageEdges(t, r); got != 0 {
		t.Fatalf("deleted file: Ruby-source cross_language_ref = %d, want 0", got)
	}
	r.assertRubyFreshParity(t, "deleted file")
}

// A database written before B1 holds the false Ruby -> Python link under a
// current v1 marker. One no-byte update must rebuild it away under v2, and the
// next no-op update must trust v2 and do nothing.
func TestRubyRequireFalseCrossLanguageLinkSelfHeals(t *testing.T) {
	r := newRubyRequireRepo(t, rubyRequireTree())
	want := r.projection(t)
	db := r.raw(t)
	repo := strconv.FormatInt(r.repoID, 10)
	if _, err := db.ExecContext(r.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		                  resolution_strategy, resolution_confidence)
		SELECT src.repo_id, src.id, dst.id, 'render_report', ?, 'shared_name:ruby→python', src.file_id, src.start_line,
		       'cross_language_shared_name', 'low'
		FROM symbols src JOIN files sf ON sf.id = src.file_id, symbols dst JOIN files df ON df.id = dst.file_id
		WHERE src.repo_id = ? AND sf.path = 'app/caller.rb' AND src.name = 'render_report'
		  AND df.path = 'tools/report.py' AND dst.name = 'render_report'`,
		store.EdgeKindCrossLanguageRef, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(r.ctx, `DELETE FROM settings WHERE key = ?`, "derived.cross_language_links_current.v2."+repo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(r.ctx, `INSERT OR REPLACE INTO settings(key, value) VALUES(?, '1')`, "derived.cross_language_links_current.v1."+repo); err != nil {
		t.Fatal(err)
	}
	if got := rubySourceCrossLanguageEdges(t, r); got != 1 {
		t.Fatalf("simulated pre-B1 database holds %d false links, want 1", got)
	}
	if current, err := r.store.CrossLanguageLinksCurrent(r.ctx, r.repoID); err != nil || current {
		t.Fatalf("v1 marker read as current = %v, %v", current, err)
	}

	r.update(t)
	if got := rubySourceCrossLanguageEdges(t, r); got != 0 {
		t.Fatalf("no-byte update kept %d false Ruby-source links", got)
	}
	if current, err := r.store.CrossLanguageLinksCurrent(r.ctx, r.repoID); err != nil || !current {
		t.Fatalf("v2 marker after heal = %v, %v", current, err)
	}
	if got := r.projection(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("healed projection differs from fresh\nhealed:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	r.assertRubyFreshParity(t, "healed")

	if summary := r.update(t); summary.ResolveMode != "none" {
		t.Fatalf("second no-op update ResolveMode = %q, want none", summary.ResolveMode)
	}
	if got := r.projection(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("second no-op update changed the graph")
	}
}
