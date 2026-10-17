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
	"github.com/isink17/codegraph/internal/store"
)

// javaReceiverTree calls methods on locals and parameters with explicitly
// declared types. It compiles with javac 17; the expected targets below are
// javac's compile-time declarations, or "" where the resolver must refuse.
var javaReceiverTree = map[string]string{
	"app/Opt.java":    "package app;\npublic class Opt {\n  public String key() { return \"\"; }\n  public void set(int a) {}\n  public void set(int a, int b) {}\n  public void dup(String s) {}\n  public void dup(Integer i) {}\n}\n",
	"app/Shape.java":  "package app;\npublic interface Shape { void area(); }\n",
	"app/Base.java":   "package app;\npublic class Base { public void inh(String s) {} }\n",
	"app/Sub.java":    "package app;\npublic class Sub extends Base { public void inh(int i) {} public void solo() {} }\n",
	"app/Helper.java": "package app;\npublic class Helper { public static String key() { return \"\"; } }\n",
	// ArrayList declares remove(int) and remove(Object): javac binds
	// e.remove(1) to ArrayList.remove(int), which the graph never records.
	"app/Ext.java":  "package app;\npublic class Ext extends java.util.ArrayList<String> { public boolean remove(String s) { return false; } }\n",
	"app/Ser.java":  "package app;\npublic class Ser implements java.io.Serializable { public void ping() {} }\n",
	"app/Mid.java":  "package app;\npublic class Mid extends java.util.ArrayList<String> {}\n",
	"app/Leaf.java": "package app;\npublic class Leaf extends Mid { public void leaf() {} }\n",
	"app/Use.java": `package app;
import java.util.List;
public class Use {
  Sub field;
  void params(Opt p, Shape sh, Sub s, List<Opt> list) {
    p.key();
    p.set(1);
    p.set(1, 2);
    p.dup("x");
    sh.area();
    s.inh(1);
    s.solo();
    Opt Helper = p;
    Helper.key();
    var v = p;
    v.key();
    for (Opt o : list) { o.key(); }
  }
  void shadow(Opt field) { field.key(); }
  void viaField() { field.solo(); }
  <T extends Opt> void generic(T t) { t.key(); }
  void supers(Ext e, Ser s2, Leaf l) { e.remove(1); s2.ping(); l.leaf(); }
}
`,
}

func assertJavaReceiverTargets(t *testing.T, r *lifecycleRepo, step string, keyTarget string) {
	t.Helper()
	calls := javaEdgesByCallee(t, r, "calls")
	for key, want := range map[string][]string{
		"app.Use.params|p.key":        {keyTarget},
		"app.Use.params|p.set":        {"app.Opt.set", "app.Opt.set"},
		"app.Use.params|p.dup":        {""},
		"app.Use.params|sh.area":      {""},
		"app.Use.params|s.inh":        {""},
		"app.Use.params|s.solo":       {"app.Sub.solo"},
		"app.Use.params|Helper.key":   {keyTarget},
		"app.Use.params|v.key":        {""},
		"app.Use.params|o.key":        {keyTarget},
		"app.Use.shadow|field.key":    {keyTarget},
		"app.Use.viaField|field.solo": {"app.Sub.solo"},
		"app.Use.generic|t.key":       {""},
		"app.Use.supers|e.remove":     {""},
		"app.Use.supers|s2.ping":      {"app.Ser.ping"},
		"app.Use.supers|l.leaf":       {""},
	} {
		if strings.Join(calls[key], ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s bound %q, want %q", step, key, calls[key], want)
		}
	}
	// The two set overloads differ by arity: each call binds its own.
	var ids []int64
	for _, e := range javaReceiverEdges(t, r) {
		if e.DstName == "p.set" && e.DstSymbolID != nil {
			ids = append(ids, *e.DstSymbolID)
		}
		if e.DstName == "p.key" && keyTarget != "" && e.ResolutionStrategy != store.ResolutionStrategyJavaReceiverScope {
			t.Errorf("%s: p.key strategy %q", step, e.ResolutionStrategy)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("%s: p.set targets %v, want the two distinct overloads", step, ids)
	}
}

func javaReceiverEdges(t *testing.T, r *lifecycleRepo) []store.ExportEdge {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func TestJavaTypedLocalReceivers(t *testing.T) {
	r := newLifecycleRepo(t, javaReceiverTree)
	assertJavaReceiverTargets(t, r, "fresh", "app.Opt.key")
	if s := r.update(t); s.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", s.FilesIndexed)
	}
	assertJavaReceiverTargets(t, r, "no-op update", "app.Opt.key")
	r.assertFreshParity(t, "no-op update")

	// Removing the method returns its callers to unresolved; never Helper.key.
	r.write(t, "app/Opt.java", strings.Replace(javaReceiverTree["app/Opt.java"], "  public String key() { return \"\"; }\n", "", 1))
	r.update(t, "app/Opt.java")
	assertJavaReceiverTargets(t, r, "key removed", "")
	r.assertFreshParity(t, "key removed")

	r.write(t, "app/Opt.java", javaReceiverTree["app/Opt.java"])
	r.update(t, "app/Opt.java")
	assertJavaReceiverTargets(t, r, "key restored", "app.Opt.key")
	r.assertFreshParity(t, "key restored")

	// Dropping the extends clause leaves no inherited overload: s.inh binds.
	r.write(t, "app/Sub.java", "package app;\npublic class Sub { public void inh(int i) {} public void solo() {} }\n")
	r.update(t, "app/Sub.java")
	r.assertFreshParity(t, "extends removed")
	if g := javaEdgesByCallee(t, r, "calls")["app.Use.params|s.inh"]; len(g) != 1 || g[0] != "app.Sub.inh" {
		t.Fatalf("s.inh without supertype bound %q, want app.Sub.inh", g)
	}

	// Reopening the database keeps every binding.
	before := javaEdgesByCallee(t, r, "calls")
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.store, r.idx = s, New(s, lifecycleRegistry(), nil)
	after := javaEdgesByCallee(t, r, "calls")
	for k, v := range before {
		if strings.Join(after[k], ",") != strings.Join(v, ",") {
			t.Errorf("reopen: %s = %q, want %q", k, after[k], v)
		}
	}
	if s := r.update(t); s.FilesIndexed != 0 {
		t.Fatalf("no-op update after reopen indexed %d files", s.FilesIndexed)
	}
}

// A graph written by the v12 Java parser carries no receiver types; an
// ordinary update re-parses the Java files and lands where a fresh index does.
func TestJavaTypedLocalReceiverProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaReceiverTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV12(), tsparser.NewKotlin()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Use.java"); got != "treesitter:java:v12" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if g := javaEdgesByCallee(t, r, "calls")["app.Use.params|p.key"]; len(g) != 1 || g[0] != "" {
		t.Fatalf("legacy p.key bound %q, want unresolved", g)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaReceiverTargets(t, r, "update", "app.Opt.key")
	r.assertFreshParity(t, "receiver profile convergence")
}

// A single-type import shadows a same-package type of the same simple name
// (JLS 6.4.1). C's supertype is the library's lib.Base, whose m(int) javac
// picks for c.m(1); the unrelated project p.Base must not stand in for it.
func TestJavaReceiverSupertypeThroughImport(t *testing.T) {
	r := newLifecycleRepo(t, map[string]string{
		"p/Base.java": "package p;\npublic class Base { }\n",
		"p/C.java":    "package p;\nimport lib.Base;\npublic class C extends Base { public void m(String s) {} }\n",
		"p/D.java":    "package p;\npublic class D extends Base { public void n(String s) {} }\n",
		"p/Use.java":  "package p;\nclass Use {\n  void run() { Base b = new Base(); C c = new C(); c.m(1); D d = new D(); d.n(\"\"); }\n}\n",
	})
	calls := javaEdgesByCallee(t, r, "calls")
	if got := strings.Join(calls["p.Use.run|c.m"], ","); got != "" {
		t.Fatalf("c.m bound %q through an imported library supertype", got)
	}
	// Without the import the same-package Base is the supertype: d.n binds.
	if got := strings.Join(calls["p.Use.run|d.n"], ","); got != "p.D.n" {
		t.Fatalf("d.n bound %q, want p.D.n", got)
	}
	r.assertFreshParity(t, "import shadows package type")
}
