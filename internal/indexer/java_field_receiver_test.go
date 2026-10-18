//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// javaFieldReceiverTree calls methods on fields. It compiles with
// `javac --release 17 --enable-preview`; the expected targets are javac's
// compile-time declarations, or "" where the resolver must refuse.
var javaFieldReceiverTree = map[string]string{
	"app/Box.java": `package app;
public class Box<T extends Repo> {
  T item;
  void f() { item.save(); }
}
`,
	"app/Child.java": `package app;
public class Child extends Svc {
  Child() { super(null); }
  void inherited() { repo.save(); }
}
`,
	"app/Ext.java": `package app;
public class Ext extends java.util.ArrayList<String> { public void grow() {} }
`,
	"app/Kind.java": `package app;
public enum Kind {
  A;
  Repo r;
  void f() { r.save(); }
}
`,
	"app/Other.java": `package app;
public class Other { public void save() {} }
`,
	"app/Repo.java": `package app;
public class Repo {
  public void save() {}
  public void save(int n) {}
  public void dup(String s) {}
  public void dup(Integer i) {}
}
`,
	"app/Store.java": `package app;
public interface Store { void put(); }
`,
	"app/Svc.java": `package app;
import java.util.function.Consumer;
public class Svc {
  protected final Repo repo = new Repo();
  static Repo shared;
  Store store;
  Ext ext;
  java.util.List<Repo> list;
  Svc(Repo repo) {
    this.repo.save(1);
    repo.save();
  }
  void run() {
    repo.save();
    this.repo.save();
    shared.save();
    Runnable r = () -> repo.save();
    repo.dup("x");
    repo.toString();
    store.put();
    ext.grow();
    list.size();
  }
  void local() {
    Other repo = new Other();
    repo.save();
    this.repo.save();
  }
  void lambda() {
    Consumer<Other> c = repo -> repo.save();
  }
  void pattern(Object o) {
    switch (o) {
      case Other repo -> repo.save();
      default -> {}
    }
  }
  {
    Object o = new Other();
    if (o instanceof Other repo) { repo.save(); }
  }
  void localType() {
    class Repo { void save() {} }
    repo.save();
  }
  void anon() {
    new Runnable() {
      Other own;
      public void run() { repo.save(); own.save(); }
    };
  }
  class Inner {
    Repo mine;
    void go() { mine.save(); repo.save(); }
  }
}
`,
}

// Every Java call edge, keyed "caller|callee", with its sorted targets.
func javaFieldReceiverWant(saveTarget string) map[string][]string {
	return map[string][]string{
		"app.Svc.Svc|this.repo.save":    {"app.Repo.save"},
		"app.Svc.Svc|repo.save":         {"app.Repo.save"},
		"app.Svc.run|repo.save":         {saveTarget, saveTarget},
		"app.Svc.run|this.repo.save":    {saveTarget},
		"app.Svc.run|shared.save":       {saveTarget},
		"app.Svc.run|repo.dup":          {""},
		"app.Svc.run|repo.toString":     {""},
		"app.Svc.run|store.put":         {""},
		"app.Svc.run|ext.grow":          {""},
		"app.Svc.run|list.size":         {""},
		"app.Svc.local|repo.save":       {"app.Other.save"},
		"app.Svc.local|this.repo.save":  {saveTarget},
		"app.Svc.lambda|repo.save":      {""},
		"app.Svc.pattern|repo.save":     {""},
		"app.Svc.localType|repo.save":   {""},
		"app.Svc.anon|repo.save":        {""},
		"app.Svc.anon|own.save":         {""},
		"app.Svc.Inner.go|mine.save":    {saveTarget},
		"app.Svc.Inner.go|repo.save":    {""},
		"app.Child.inherited|repo.save": {""},
		"app.Box.f|item.save":           {""},
	}
}

func assertJavaFieldReceiverTargets(t *testing.T, r *lifecycleRepo, step string, want map[string][]string) {
	t.Helper()
	got := javaEdgesByCallee(t, r, "calls")
	for key, targets := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s: unexpected call edge %s -> %q", step, key, targets)
		}
	}
	for key, w := range want {
		g := append([]string(nil), got[key]...)
		sort.Strings(g)
		w = append([]string(nil), w...)
		sort.Strings(w)
		if len(g) != len(w) || strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("%s: %s bound %q, want %q", step, key, g, w)
		}
	}
}

func TestJavaFieldReceivers(t *testing.T) {
	r := newLifecycleRepo(t, javaFieldReceiverTree)
	assertJavaFieldReceiverTargets(t, r, "fresh", javaFieldReceiverWant("app.Repo.save"))
	if s := r.update(t); s.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", s.FilesIndexed)
	}
	r.assertFreshParity(t, "no-op update")

	// Without a zero-argument save the field calls that relied on it unbind.
	r.write(t, "app/Repo.java", strings.Replace(javaFieldReceiverTree["app/Repo.java"], "  public void save() {}\n", "", 1))
	r.update(t, "app/Repo.java")
	want := javaFieldReceiverWant("")
	want["app.Svc.Svc|repo.save"] = []string{""}
	assertJavaFieldReceiverTargets(t, r, "save removed", want)
	r.assertFreshParity(t, "save removed")

	r.write(t, "app/Repo.java", javaFieldReceiverTree["app/Repo.java"])
	r.update(t, "app/Repo.java")
	assertJavaFieldReceiverTargets(t, r, "save restored", javaFieldReceiverWant("app.Repo.save"))
	r.assertFreshParity(t, "save restored")

	// Retyping the field rebinds its calls to the new declared type.
	r.write(t, "app/Svc.java", strings.Replace(javaFieldReceiverTree["app/Svc.java"], "protected final Repo repo = new Repo();", "protected final Other repo = new Other();", 1))
	r.update(t, "app/Svc.java")
	got := javaEdgesByCallee(t, r, "calls")
	if g := strings.Join(got["app.Svc.local|this.repo.save"], ","); g != "app.Other.save" {
		t.Fatalf("retyped this.repo.save bound %q, want app.Other.save", g)
	}
	r.assertFreshParity(t, "field retyped")
}

// A graph written by the v13 Java parser marks no field receivers; an
// ordinary update re-parses the Java files and lands where a fresh index does.
func TestJavaFieldReceiverProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaFieldReceiverTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV13(), tsparser.NewKotlin()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Svc.java"); got != "treesitter:java:v13" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if g := javaEdgesByCallee(t, r, "calls")["app.Svc.run|this.repo.save"]; len(g) != 1 || g[0] != "" {
		t.Fatalf("legacy this.repo.save bound %q, want unresolved", g)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaFieldReceiverTargets(t, r, "update", javaFieldReceiverWant("app.Repo.save"))
	r.assertFreshParity(t, "field receiver profile convergence")
}

// A field of a parameterized project class type binds by the raw class name;
// a field typed by a bare type variable does not.
func TestJavaParameterizedFieldReceiver(t *testing.T) {
	r := newLifecycleRepo(t, map[string]string{
		"app/Node.java": `package app;
public class Node<T> {
  Node<T> next;
  T value;
  public void g() {}
  void walk() { next.g(); this.next.g(); }
}
`,
		"app/List.java": `package app;
public class List {
  Node<String> head;
  void f() { head.g(); }
}
`,
	})
	assertJavaFieldReceiverTargets(t, r, "fresh", map[string][]string{
		"app.Node.walk|next.g":      {"app.Node.g"},
		"app.Node.walk|this.next.g": {"app.Node.g"},
		"app.List.f|head.g":         {"app.Node.g"},
	})
	r.assertFreshParity(t, "fresh")
}
