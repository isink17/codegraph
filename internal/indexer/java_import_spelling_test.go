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

// javaImportSpellingTree imports packages whose names begin with "static" and
// spells `static` with a tab, a newline or comments around it. Oracle (javac
// 17, `javap -c -p`):
//
//	Prefix.make          new Bag()  staticpkg/Bag   (`import staticpkg.Bag;`)
//	PrefixWild.make      new Wid()  staticw/Wid     (`import staticw.*;`)
//	Spaced.make          new Bag()  staticpkg/Bag   (`import staticpkg /*c*/ . Bag;`)
//	TabStatic.go         run()      x/Util.run      (`import static<TAB>x.Util.run;`)
//	CommentStatic.go     run()      x/Util.run      (`import /*c*/ static /*d*/ x.Util.run;`)
//	NewlineStatic.go     run()      x/Util.run      (`import static<NL>x.Util.*;`)
//	Ambig.make           new Wid()  javac: "reference to Wid is ambiguous"
//
// pkg.Bag and w.Wid exist so that dropping the "static" from a package name
// finds a real, wrong type.
var javaImportSpellingTree = tree{
	"pkg/Bag.java":           `package pkg; public class Bag { public Bag() {} }`,
	"staticpkg/Bag.java":     `package staticpkg; public class Bag { public Bag() {} }`,
	"w/Wid.java":             `package w; public class Wid { public Wid() {} }`,
	"staticw/Wid.java":       `package staticw; public class Wid { public Wid() {} }`,
	"x/Util.java":            `package x; public class Util { public static void run() {} }`,
	"app/Prefix.java":        "package app;\nimport staticpkg.Bag;\npublic class Prefix {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/PrefixWild.java":    "package app;\nimport staticw.*;\npublic class PrefixWild {\n    void make() {\n        new Wid();\n    }\n}\n",
	"app/Spaced.java":        "package app;\nimport staticpkg /*c*/ . Bag;\npublic class Spaced {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/TabStatic.java":     "package app;\nimport static\tx.Util.run;\npublic class TabStatic {\n    void go() {\n        run();\n    }\n}\n",
	"app/CommentStatic.java": "package app;\nimport /*c*/ static /*d*/ x.Util.run;\npublic class CommentStatic {\n    void go() {\n        run();\n    }\n}\n",
	"app/NewlineStatic.java": "package app;\nimport static\nx.Util.*;\npublic class NewlineStatic {\n    void go() {\n        run();\n    }\n}\n",
	"ambig/Ambig.java":       "package ambig;\nimport staticw.*;\nimport w.*;\npublic class Ambig {\n    void make() {\n        new Wid();\n    }\n}\n",
}

// javaEdgesByCallee maps "caller|name" to the qualified names every edge of
// kind bound ("" when unresolved), in source order.
func javaEdgesByCallee(t *testing.T, r *lifecycleRepo, kind string) map[string][]string {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range edges {
		if e.Kind == kind {
			k := e.SrcQualifiedName + "|" + e.DstName
			out[k] = append(out[k], e.DstQualifiedName)
		}
	}
	return out
}

func assertJavaImportSpellingTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	constructs := javaEdgesByCallee(t, r, "constructs")
	calls := javaEdgesByCallee(t, r, "calls")
	for _, c := range []struct {
		got  map[string][]string
		key  string
		want string
	}{
		{constructs, "app.Prefix.make|Bag", "staticpkg.Bag.Bag"},
		{constructs, "app.PrefixWild.make|Wid", "staticw.Wid.Wid"},
		{constructs, "app.Spaced.make|Bag", "staticpkg.Bag.Bag"},
		{constructs, "ambig.Ambig.make|Wid", ""},
		{calls, "app.TabStatic.go|run", "x.Util.run"},
		{calls, "app.CommentStatic.go|run", "x.Util.run"},
		{calls, "app.NewlineStatic.go|run", "x.Util.run"},
	} {
		if g := c.got[c.key]; len(g) != 1 || g[0] != c.want {
			t.Errorf("%s: %s bound %q, want [%q]", step, c.key, g, c.want)
		}
	}
}

func TestJavaImportSpellingBindsDeclaredName(t *testing.T) {
	r := newLifecycleRepo(t, javaImportSpellingTree)
	assertJavaImportSpellingTargets(t, r, "fresh")
	// Removing staticpkg.Bag leaves the import naming nothing in the graph;
	// pkg.Bag must not answer for it.
	r.remove(t, "staticpkg/Bag.java")
	r.update(t, "staticpkg/Bag.java")
	r.assertFreshParity(t, "imported type removed")
	if got := javaEdgesByCallee(t, r, "constructs")["app.Prefix.make|Bag"]; len(got) != 1 || got[0] != "" {
		t.Fatalf("Prefix.make bound %q without staticpkg.Bag, want unresolved", got)
	}
}

// A graph written by the v9 Java parser recorded `import staticpkg.Bag;` as
// pkg.Bag and missed static imports spelled without a following space.
// Updating it with the current parser re-parses the Java files and must land
// exactly where a fresh index does.
func TestJavaImportSpellingProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaImportSpellingTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV9()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Prefix.java"); got != "treesitter:java:v9" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := javaEdgesByCallee(t, r, "constructs")["app.Prefix.make|Bag"]; len(got) != 1 || got[0] != "pkg.Bag.Bag" {
		t.Fatalf("legacy Prefix.make bound %q, want the v9 wrong target pkg.Bag.Bag", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaImportSpellingTargets(t, r, "update")
	r.assertFreshParity(t, "java import spelling profile convergence")
	summary, err = r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	r.assertFreshParity(t, "java import spelling second update")
}
