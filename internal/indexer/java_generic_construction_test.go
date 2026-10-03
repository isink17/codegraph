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

// javaGenericTree constructs generic classes with diamond and explicit type
// arguments. Java selects the constructor by the class and the argument count,
// never by the type arguments, so each call must bind as its raw twin does.
var javaGenericTree = tree{
	"app/Box.java":   `package app; public class Box<T> { public Box() {} public Box(T a) {} public Box(T a, T b) {} }`,
	"app/Outer.java": `package app; public class Outer { public static class Inner<U> { public Inner(U u) {} } }`,
	"a/b/Deep.java":  `package a.b; public class Deep<T> { public Deep() {} }`,
	// A top-level class named like Outer's inner class: `o.new Inner<>()`
	// constructs Outer.Inner (javac), never this one.
	"app/Inner.java":  `package app; public class Inner<V> { public Inner() {} }`,
	"app/Holder.java": `package app; public class Holder<T> { public class Inner<U> { public Inner() {} } }`,
	"app/Caller.java": `package app;

import java.util.List;
import java.util.Map;

public class Caller {
    void diamond0() { new Box<>(); }
    void diamond1() { new Box<>(1); }
    void explicit2() { new Box<String>("a", "b"); }
    void nested() { new Box<Map<String, List<Integer>>>(null); }
    void qualified() { new a.b.Deep<>(); }
    void qualifiedRaw() { new a.b.Deep(); }
    void anon() { new Box<>(1) { }; }
    void raw1() { new Box(1); }
    void member() { new Outer.Inner<>(1); }
    void memberRaw() { new Outer.Inner(1); }
    void comment() { new Box /* c */ <>(); }
    void arity3() { new Box<>(1, 2, 3); }
    void qualifiedCreation(Holder<String> o) { o.new Inner<>(); }
}
`,
}

var javaGenericWant = map[string]string{
	"app.Caller.diamond0":  "public Box()",
	"app.Caller.diamond1":  "public Box(T a)",
	"app.Caller.explicit2": "public Box(T a, T b)",
	"app.Caller.nested":    "public Box(T a)",
	"app.Caller.anon":      "public Box(T a)",
	"app.Caller.raw1":      "public Box(T a)",
	"app.Caller.comment":   "public Box()",
	"app.Caller.arity3":    "", // no three-argument constructor
	// A bare lookup of Inner finds app.Inner, not Holder.Inner.
	"app.Caller.qualifiedCreation": "",
}

func assertJavaGenericTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	got := constructorTargets(t, r)
	for caller, want := range javaGenericWant {
		if got[caller] != want {
			t.Errorf("%s: %s bound %q, want %q", step, caller, got[caller], want)
		}
	}
	// Type arguments never change which class a qualified or member name
	// denotes.
	for generic, raw := range map[string]string{"app.Caller.qualified": "app.Caller.qualifiedRaw", "app.Caller.member": "app.Caller.memberRaw"} {
		if got[generic] != got[raw] {
			t.Errorf("%s: %s bound %q, its raw twin %q", step, generic, got[generic], got[raw])
		}
	}
	if got["app.Caller.qualifiedRaw"] != "public Deep()" {
		t.Errorf("%s: qualified raw construction bound %q", step, got["app.Caller.qualifiedRaw"])
	}
}

func TestJavaGenericConstructionBindsLikeRaw(t *testing.T) {
	r := newLifecycleRepo(t, javaGenericTree)
	assertJavaGenericTargets(t, r, "fresh")
}

// A graph written by the previous Java parser spells generic constructions
// with their type arguments. Updating it with the current parser re-parses
// the Java files and must land exactly where a fresh index lands.
func TestJavaGenericConstructionProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaGenericTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV5()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v5" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := constructorTargets(t, r)["app.Caller.diamond1"]; got != "" {
		t.Fatalf("legacy diamond bound %q; its spelling carried type arguments", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v6" {
		t.Fatalf("updated profile = %q", got)
	}
	assertJavaGenericTargets(t, r, "update")
	r.assertFreshParity(t, "java generic construction profile convergence")
}

// TestJavaGenericConstructionFollowsIncrementalChanges switches a call between
// diamond and explicit arguments and removes the matching constructor; every
// step must match a fresh index of the same tree.
func TestJavaGenericConstructionFollowsIncrementalChanges(t *testing.T) {
	r := newLifecycleRepo(t, javaGenericTree)
	caller := javaGenericTree["app/Caller.java"]
	r.write(t, "app/Caller.java", strings.Replace(caller, "new Box<>(1); }", "new Box<Integer>(1); }", 1))
	r.update(t, "app/Caller.java")
	r.assertFreshParity(t, "diamond to explicit")
	assertJavaGenericTargets(t, r, "diamond to explicit")
	r.write(t, "app/Box.java", `package app; public class Box<T> { public Box() {} public Box(T a, T b) {} }`)
	r.update(t, "app/Box.java")
	r.assertFreshParity(t, "constructor removed")
	if got := constructorTargets(t, r)["app.Caller.diamond1"]; got != "" {
		t.Fatalf("diamond1 bound %q after its constructor was removed", got)
	}
}
