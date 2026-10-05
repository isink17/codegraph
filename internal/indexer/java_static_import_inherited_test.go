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

// javaStaticImportInheritedTree pits a static import against methods the
// calling class may inherit. Oracle (javac 17, each method prints its name):
//
//	Inst.run     foo()        Base.foo       inherited instance method
//	Stat.run     foo()        SBase.foo      inherited static method
//	Wild.run     foo()        Base.foo       on-demand static import
//	Grand.run    foo()        G.foo          grandparent's method
//	Dflt.run     foo()        I.foo          interface default method
//	Arity.run    foo()        compile error  Base.foo(String) shadows by name
//	Outer.In.run foo()        Base.foo       inherited by the enclosing class
//	Obj.run      toString(1)  compile error  Object.toString shadows
//	Plain.run    foo()        Util.foo       no supertype: the import applies
//	KtCall.run   hello()      lib.hello      Kotlin facade, no supertype
//	Anon         foo()        Base.foo       anonymous class body extends Base (no call edge today)
//	Loc.m        foo()        Base.foo       local class extends Base
//	En           foo()        Util.foo       enum constant body (no call edge today)
//	Rec.run      foo()        Util.foo       record (refused: not a plain class)
//	Lam.run      foo()        Util.foo       lambda in a class with no supertype
//	Nest.S.run   foo()        Base.foo       static nested class of a class extending Base
//
// Every call but Plain's stays unresolved: supertypes are not recorded.
var javaStaticImportInheritedTree = map[string]string{
	"app/Util.java":   "package app;\npublic class Util { public static void foo() {} public static void toString(int x) {} }\n",
	"app/Base.java":   "package app;\npublic class Base { public void foo() {} }\n",
	"app/SBase.java":  "package app;\npublic class SBase { public static void foo() {} }\n",
	"app/G.java":      "package app;\npublic class G { public void foo() {} }\n",
	"app/Mid.java":    "package app;\npublic class Mid extends G {}\n",
	"app/I.java":      "package app;\npublic interface I { default void foo() {} }\n",
	"app/SB.java":     "package app;\npublic class SB { public void foo(String s) {} }\n",
	"app/Inst.java":   "package app;\nimport static app.Util.foo;\npublic class Inst extends Base { void run() { foo(); } }\n",
	"app/Stat.java":   "package app;\nimport static app.Util.foo;\npublic class Stat extends SBase { void run() { foo(); } }\n",
	"app/Wild.java":   "package app;\nimport static app.Util.*;\npublic class Wild extends Base { void run() { foo(); } }\n",
	"app/Grand.java":  "package app;\nimport static app.Util.foo;\npublic class Grand extends Mid { void run() { foo(); } }\n",
	"app/Dflt.java":   "package app;\nimport static app.Util.foo;\npublic class Dflt implements I { void run() { foo(); } }\n",
	"app/Arity.java":  "package app;\nimport static app.Util.foo;\npublic class Arity extends SB { void run() { foo(); } }\n",
	"app/Outer.java":  "package app;\nimport static app.Util.foo;\npublic class Outer extends Base { class In { void run() { foo(); } } }\n",
	"app/Obj.java":    "package app;\nimport static app.Util.toString;\npublic class Obj { void run() { toString(1); } }\n",
	"lib/Util.kt":     "package lib\nfun hello() {}\n",
	"app/KtCall.java": "package app;\nimport static lib.UtilKt.hello;\npublic class KtCall { void run() { hello(); } }\n",
	"app/Anon.java":   "package app;\nimport static app.Util.foo;\npublic class Anon { Object o = new Base() {\nvoid r() { foo(); }\n}; }\n",
	"app/Loc.java":    "package app;\nimport static app.Util.foo;\npublic class Loc { void m() { class L extends Base {\nvoid r() { foo(); }\n} } }\n",
	"app/En.java":     "package app;\nimport static app.Util.foo;\npublic enum En { A {\nvoid r() { foo(); }\n} }\n",
	"app/Rec.java":    "package app;\nimport static app.Util.foo;\npublic record Rec(int x) {\nvoid run() { foo(); }\n}\n",
	"app/Lam.java":    "package app;\nimport static app.Util.foo;\npublic class Lam {\nvoid run() { Runnable q = () -> foo(); }\n}\n",
	"app/Nest.java":   "package app;\nimport static app.Util.foo;\npublic class Nest extends Base {\nstatic class S {\nvoid run() { foo(); }\n}\n}\n",
	"app/Plain.java":  "package app;\nimport static app.Util.foo;\npublic class Plain { void run() { foo(); } }\n",
}

func assertJavaStaticImportInheritedTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	calls := javaEdgesByCallee(t, r, "calls")
	for key, want := range map[string]string{
		"app.Inst.run|foo":     "",
		"app.Stat.run|foo":     "",
		"app.Wild.run|foo":     "",
		"app.Grand.run|foo":    "",
		"app.Dflt.run|foo":     "",
		"app.Arity.run|foo":    "",
		"app.Outer.In.run|foo": "",
		"app.Obj.run|toString": "",
		"app.Plain.run|foo":    "app.Util.foo",
		"app.KtCall.run|hello": "lib.hello",
		"app.Lam.run|foo":      "app.Util.foo",
		"app.Rec.run|foo":      "",
		"app.Nest.S.run|foo":   "",
		"app.Loc.m|foo":        "",
	} {
		if g := calls[key]; len(g) != 1 || g[0] != want {
			t.Errorf("%s: %s bound %q, want [%q]", step, key, g, want)
		}
	}
}

func TestJavaStaticImportYieldsToSupertypes(t *testing.T) {
	r := newLifecycleRepo(t, javaStaticImportInheritedTree)
	assertJavaStaticImportInheritedTargets(t, r, "fresh")
	if got := r.refTarget(t, "app/Inst.java", "foo"); got != "" {
		t.Fatalf("Inst reference names %q, want none", got)
	}
	// Dropping the extends clause leaves only Object inherited: the import binds.
	r.write(t, "app/Inst.java", "package app;\nimport static app.Util.foo;\npublic class Inst { void run() { foo(); } }\n")
	r.update(t, "app/Inst.java")
	r.assertFreshParity(t, "extends removed")
	if g := javaEdgesByCallee(t, r, "calls")["app.Inst.run|foo"]; len(g) != 1 || g[0] != "app.Util.foo" {
		t.Fatalf("Inst.run without supertype bound %q, want app.Util.foo", g)
	}
	r.write(t, "app/Inst.java", "package app;\nimport static app.Util.foo;\npublic class Inst implements I { void run() { foo(); } }\n")
	r.update(t, "app/Inst.java")
	r.assertFreshParity(t, "implements added")
	assertJavaStaticImportInheritedTargets(t, r, "implements added")
	if s := r.update(t); s.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", s.FilesIndexed)
	}
	r.assertFreshParity(t, "no-op update")
}

// A graph written by the v10 Java parser bound every bare call a static
// import names, inherited methods notwithstanding. An ordinary update with no
// source change re-parses the Java files and must land where a fresh index
// does, with the wrong target and reference identity gone.
func TestJavaStaticImportInheritedProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaStaticImportInheritedTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV10(), tsparser.NewKotlin()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Inst.java"); got != "treesitter:java:v10" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	// v10 calls carry no supertype mark, so the current resolver refuses them
	// even on a v10 parse; write the binding the v10 resolver made.
	r.bind(t, "app/Inst.java", "foo", "app.Util.foo")
	if got := javaEdgesByCallee(t, r, "calls")["app.Inst.run|foo"]; len(got) != 1 || got[0] != "app.Util.foo" {
		t.Fatalf("legacy Inst.run bound %q, want the v10 wrong target app.Util.foo", got)
	}
	if got := r.refTarget(t, "app/Inst.java", "foo"); got != "app.Util.foo" {
		t.Fatalf("legacy Inst reference names %q, want app.Util.foo", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaStaticImportInheritedTargets(t, r, "update")
	if got := r.refTarget(t, "app/Inst.java", "foo"); got != "" {
		t.Fatalf("updated Inst reference names %q, want none", got)
	}
	r.assertFreshParity(t, "static import inherited profile convergence")
	summary, err = r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	r.assertFreshParity(t, "static import inherited second update")
}
