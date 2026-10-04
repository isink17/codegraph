//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// javaSameLineTree declares several methods on one line. Edges carry the
// column they start at, so each construction and call is credited to the
// method whose body holds it rather than dropped as ambiguous. Oracle (javac
// 17, `javap -c -p`):
//
//	Sub.gen   new Box<>()  app/Box."<init>"
//	Sub.x     new Box()    app/Box."<init>"
//	Calls.a   b()          app/Calls.b
//	Field.m   (none; `new Box()` initializes the field f in Field's constructor)
var javaSameLineTree = tree{
	"app/Box.java":   `package app; public class Box<T> { public Box() {} }`,
	"app/Sub.java":   `package app; public class Sub { void gen() { new Box<>(); } void x() { new Box(); } }`,
	"app/Calls.java": `package app; public class Calls { void a() { b(); } void b() {} }`,
	"app/Field.java": `package app; public class Field { Object f = new Box(); void m() {} }`,
}

func javaEdgesBySource(t *testing.T, r *lifecycleRepo) map[string][]string {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range edges {
		out[e.SrcQualifiedName] = append(out[e.SrcQualifiedName], e.Kind+" "+e.DstName+" => "+e.DstQualifiedName)
	}
	return out
}

func assertJavaSameLineEdges(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	got := javaEdgesBySource(t, r)
	for src, want := range map[string]string{
		"app.Sub.gen": "constructs Box => app.Box.Box",
		"app.Sub.x":   "constructs Box => app.Box.Box",
		"app.Calls.a": "calls b => app.Calls.b",
		// The field initializer is not in m's body.
		"app.Field.m": "",
	} {
		if g := strings.Join(got[src], "; "); g != want {
			t.Errorf("%s: %s edges %q, want %q", step, src, g, want)
		}
	}
}

func TestJavaSameLineMethodsKeepTheirEdges(t *testing.T) {
	r := newLifecycleRepo(t, javaSameLineTree)
	assertJavaSameLineEdges(t, r, "fresh")
	sub := javaSameLineTree["app/Sub.java"]
	r.write(t, "app/Sub.java", strings.Replace(sub, "void x() { new Box(); }", "void x() {}", 1))
	r.update(t, "app/Sub.java")
	r.assertFreshParity(t, "second construction removed")
	r.write(t, "app/Sub.java", sub)
	r.update(t, "app/Sub.java")
	r.assertFreshParity(t, "second construction restored")
	assertJavaSameLineEdges(t, r, "restored")
}

// A graph written by the v7 Java parser carries no columns, so same-line
// methods lose their edges. Updating it with the current parser re-parses the
// Java files and must land exactly where a fresh index lands; a second update
// changes nothing.
func TestJavaSameLineProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaSameLineTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV7()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Sub.java"); got != "treesitter:java:v7" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := javaEdgesBySource(t, r)["app.Sub.gen"]; len(got) != 0 {
		t.Fatalf("legacy Sub.gen edges %q, want none", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaSameLineEdges(t, r, "update")
	r.assertFreshParity(t, "java same line profile convergence")
	summary, err = r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	r.assertFreshParity(t, "java same line second update")
}
