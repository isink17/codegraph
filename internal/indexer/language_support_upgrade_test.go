//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// languageSupportTree has a file every registry parses (Go), one only the
// newer registry parses (Svelte), one neither parses, and one Svelte file that
// is deleted between the two binaries' scans.
var languageSupportTree = map[string]string{
	"main.go":     "package main\n\nfunc helper() {}\n\nfunc main() { helper() }\n",
	"Tick.svelte": "<script>\n  import { tick } from 'svelte';\n  async function handleKeydown(event) { await tick(); }\n</script>\n<textarea on:keydown={handleKeydown} />\n",
	"Gone.svelte": "<script>\n  function gone() {}\n</script>\n",
	"notes.zzz":   "not a supported language\n",
}

func languageSupportRegistries() (withoutSvelte, withSvelte func() *parser.Registry) {
	return func() *parser.Registry { return parser.NewRegistry(tsparser.NewGo()) },
		func() *parser.Registry { return parser.NewRegistry(tsparser.NewGo(), tsparser.NewSvelte()) }
}

type languageSupportDB struct {
	t     *testing.T
	ctx   context.Context
	root  string
	store *store.Store
}

func openLanguageSupportDB(t *testing.T, root string) *languageSupportDB {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &languageSupportDB{t: t, ctx: context.Background(), root: root, store: s}
}

func (d *languageSupportDB) run(reg *parser.Registry, kind string) store.ScanSummary {
	d.t.Helper()
	idx := New(d.store, reg, nil)
	opts := Options{RepoRoot: d.root, NoHistory: true}
	var (
		summary store.ScanSummary
		err     error
	)
	if kind == "index" {
		summary, err = idx.Index(d.ctx, opts)
	} else {
		summary, err = idx.Update(d.ctx, opts)
	}
	if err != nil {
		d.t.Fatalf("%s: %v", kind, err)
	}
	return summary
}

type languageSupportSnapshot struct {
	Graph   store.SemanticGraph
	Imports map[string][]string
}

func (d *languageSupportDB) snapshot() languageSupportSnapshot {
	d.t.Helper()
	repo, err := d.store.UpsertRepo(d.ctx, d.root)
	if err != nil {
		d.t.Fatal(err)
	}
	g, err := d.store.SemanticGraph(d.ctx, repo.ID)
	if err != nil {
		d.t.Fatal(err)
	}
	imports, err := d.store.AllImports(d.ctx, repo.ID)
	if err != nil {
		d.t.Fatal(err)
	}
	return languageSupportSnapshot{Graph: g, Imports: imports}
}

func freshLanguageSupportSnapshot(t *testing.T, root string, reg *parser.Registry) languageSupportSnapshot {
	t.Helper()
	d := openLanguageSupportDB(t, root)
	d.run(reg, "index")
	return d.snapshot()
}

func languageSupportFiles(s languageSupportSnapshot) map[string]store.SemanticFile {
	out := map[string]store.SemanticFile{}
	for _, f := range s.Graph.Files {
		out[f.Path] = f
	}
	return out
}

func writeLanguageSupportTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range languageSupportTree {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A repository indexed by a binary without an adapter for a language, then
// updated by one that has it, must converge to the newer binary's fresh index
// even though no file changed on disk.
func TestUpdateParsesUnchangedFilesOfANewlySupportedLanguage(t *testing.T) {
	without, with := languageSupportRegistries()
	root := writeLanguageSupportTree(t)
	db := openLanguageSupportDB(t, root)
	db.run(without(), "index")

	before := languageSupportFiles(db.snapshot())
	if f := before["Tick.svelte"]; f.Language != "" || f.ParserProfile != "" {
		t.Fatalf("Svelte row under the older registry = %+v, want unsupported", f)
	}
	mainBefore := before["main.go"]

	if err := os.Remove(filepath.Join(root, "Gone.svelte")); err != nil {
		t.Fatal(err)
	}
	summary := db.run(with(), "update")
	if summary.FilesIndexed != 1 || summary.FilesDeleted != 1 {
		t.Fatalf("upgrade update summary: indexed=%d deleted=%d, want only Tick.svelte parsed and Gone.svelte retired", summary.FilesIndexed, summary.FilesDeleted)
	}
	upgraded := db.snapshot()
	files := languageSupportFiles(upgraded)
	if f := files["Tick.svelte"]; f.Language != "svelte" || f.ParserProfile != "treesitter:svelte:v1" || f.ParseState != store.ParseStateIndexed {
		t.Fatalf("Svelte row after upgrade = %+v", f)
	}
	if _, ok := files["Gone.svelte"]; ok {
		t.Fatal("deleted Svelte file survived the upgrade update")
	}
	if f := files["notes.zzz"]; f.Language != "" || f.ParserProfile != "" {
		t.Fatalf("unsupported file after upgrade = %+v, want still unsupported", f)
	}
	if !reflect.DeepEqual(files["main.go"], mainBefore) {
		t.Fatalf("unchanged Go row moved: %+v -> %+v", mainBefore, files["main.go"])
	}
	foundSymbol, foundEdge := false, false
	for _, decl := range upgraded.Graph.Declarations {
		if decl.Path == "Tick.svelte" && decl.QualifiedName == "Tick.svelte#instance.handleKeydown" {
			foundSymbol = true
		}
	}
	for _, edge := range upgraded.Graph.Edges {
		if edge.Path == "main.go" && edge.Kind == "calls" && edge.Target.QualifiedName == "main.helper" && edge.Target.State == "resolved" {
			foundEdge = true
		}
	}
	if !foundSymbol || !foundEdge {
		t.Fatalf("upgraded graph: Svelte symbol=%v, Go call edge=%v\n%+v", foundSymbol, foundEdge, upgraded.Graph)
	}
	if !reflect.DeepEqual(upgraded.Imports["Tick.svelte"], []string{"svelte"}) {
		t.Fatalf("Svelte imports after upgrade = %v", upgraded.Imports["Tick.svelte"])
	}

	if fresh := freshLanguageSupportSnapshot(t, root, with()); !reflect.DeepEqual(upgraded, fresh) {
		t.Fatalf("upgraded index differs from a fresh index\n got %+v\nwant %+v", upgraded, fresh)
	}

	for i := range 2 {
		if s := db.run(with(), "update"); s.FilesIndexed != 0 || s.FilesDeleted != 0 {
			t.Fatalf("no-op update %d after upgrade: indexed=%d deleted=%d", i, s.FilesIndexed, s.FilesDeleted)
		}
		if got := db.snapshot(); !reflect.DeepEqual(got, upgraded) {
			t.Fatalf("no-op update %d changed the graph", i)
		}
	}
}

// The touch path (same bytes, moved mtime) must not relabel a file with the
// new language while keeping the graph of a binary that could not parse it.
func TestUpdateParsesNewlySupportedFileWhoseMtimeMoved(t *testing.T) {
	without, with := languageSupportRegistries()
	root := writeLanguageSupportTree(t)
	db := openLanguageSupportDB(t, root)
	db.run(without(), "index")
	path := filepath.Join(root, "Tick.svelte")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(1e9)
	if err := os.Chtimes(path, moved, moved); err != nil {
		t.Fatal(err)
	}
	db.run(with(), "update")
	if got, want := db.snapshot(), freshLanguageSupportSnapshot(t, root, with()); !reflect.DeepEqual(got, want) {
		t.Fatalf("upgraded index differs from a fresh index\n got %+v\nwant %+v", got, want)
	}
}

// A run restricted to other languages leaves the newly supported file alone;
// the first unrestricted run then parses it.
func TestUpdateLanguageFilterDefersNewlySupportedFile(t *testing.T) {
	without, with := languageSupportRegistries()
	root := writeLanguageSupportTree(t)
	db := openLanguageSupportDB(t, root)
	db.run(without(), "index")

	idx := New(db.store, with(), nil)
	if _, err := idx.Update(db.ctx, Options{RepoRoot: root, NoHistory: true, Languages: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	if f := languageSupportFiles(db.snapshot())["Tick.svelte"]; f.Language != "" {
		t.Fatalf("Go-only update touched the Svelte file: %+v", f)
	}
	db.run(with(), "update")
	if got, want := db.snapshot(), freshLanguageSupportSnapshot(t, root, with()); !reflect.DeepEqual(got, want) {
		t.Fatalf("upgraded index differs from a fresh index\n got %+v\nwant %+v", got, want)
	}
}

// A binary without the adapter (a no-CGO build, for Svelte) leaves the rows a
// more capable binary recorded alone; returning to that binary converges to its
// fresh index without churn.
func TestUpdateAdapterRoundTripConverges(t *testing.T) {
	without, with := languageSupportRegistries()
	root := writeLanguageSupportTree(t)
	db := openLanguageSupportDB(t, root)
	db.run(with(), "index")
	indexed := db.snapshot()

	if s := db.run(without(), "update"); s.FilesIndexed != 0 {
		t.Fatalf("update without the adapter parsed %d files, want 0", s.FilesIndexed)
	}
	if f := languageSupportFiles(db.snapshot())["Tick.svelte"]; f.Language != "svelte" || f.ParserProfile != "treesitter:svelte:v1" {
		t.Fatalf("Svelte row after an update without the adapter = %+v", f)
	}

	if s := db.run(with(), "update"); s.FilesIndexed != 0 {
		t.Fatalf("update back with the adapter parsed %d files, want 0", s.FilesIndexed)
	}
	if got := db.snapshot(); !reflect.DeepEqual(got, indexed) {
		t.Fatalf("round trip changed the graph\n got %+v\nwant %+v", got, indexed)
	}
}

// A path-scoped update (watcher flush, `update` with paths) reaches the same
// result for the paths it names.
func TestPathScopedUpdateParsesNewlySupportedFile(t *testing.T) {
	without, with := languageSupportRegistries()
	root := writeLanguageSupportTree(t)
	db := openLanguageSupportDB(t, root)
	db.run(without(), "index")

	idx := New(db.store, with(), nil)
	opts := Options{RepoRoot: root, NoHistory: true, ScanKind: "update", Paths: []string{"Tick.svelte", "Gone.svelte"}}
	if _, err := idx.Update(db.ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got, want := db.snapshot(), freshLanguageSupportSnapshot(t, root, with()); !reflect.DeepEqual(got, want) {
		t.Fatalf("path-scoped upgrade differs from a fresh index\n got %+v\nwant %+v", got, want)
	}
}

// cppWithoutCUDA is the C++ adapter as a build that did not yet claim `.cu`.
type cppWithoutCUDA struct{ *tsparser.CppAdapter }

func (a cppWithoutCUDA) Extensions() []string {
	return slices.DeleteFunc(a.CppAdapter.Extensions(), func(ext string) bool { return ext == ".cu" })
}

func (a cppWithoutCUDA) Supports(path string) bool {
	return !strings.EqualFold(filepath.Ext(path), ".cu") && a.CppAdapter.Supports(path)
}

// An extension newly mapped to a language that is already indexed: the new
// file joins the existing files' graph exactly as in a fresh index.
func TestUpdateParsesFileOfNewlyClaimedExtension(t *testing.T) {
	root := t.TempDir()
	for rel, src := range map[string]string{
		"kernels.h":  "void scale(float *v);\n",
		"main.cpp":   "#include \"kernels.h\"\nint main() { float v = 1; scale(&v); return 0; }\n",
		"kernels.cu": "#include \"kernels.h\"\nvoid scale(float *v) { *v *= 2; }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := openLanguageSupportDB(t, root)
	db.run(parser.NewRegistry(cppWithoutCUDA{tsparser.NewCpp()}), "index")
	if f := languageSupportFiles(db.snapshot())["kernels.cu"]; f.Language != "" {
		t.Fatalf(".cu row under the older registry = %+v, want unsupported", f)
	}
	current := func() *parser.Registry { return parser.NewRegistry(tsparser.NewCpp()) }
	if s := db.run(current(), "update"); s.FilesIndexed != 1 {
		t.Fatalf("upgrade update parsed %d files, want only kernels.cu", s.FilesIndexed)
	}
	got, want := db.snapshot(), freshLanguageSupportSnapshot(t, root, current())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upgraded index differs from a fresh index\n got %+v\nwant %+v", got, want)
	}
	if f := languageSupportFiles(got)["kernels.cu"]; f.Language != "cpp" || f.ParseState != store.ParseStateIndexed {
		t.Fatalf(".cu row after upgrade = %+v", f)
	}
	if s := db.run(current(), "update"); s.FilesIndexed != 0 {
		t.Fatalf("no-op update after upgrade parsed %d files", s.FilesIndexed)
	}
}
