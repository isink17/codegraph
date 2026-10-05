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

// csharpStaticUsingMembersTree pits `using static App.Util;` against members
// that simple-name lookup finds first. Oracle: the C# specification, simple
// names §12.8.4 and member lookup §12.5 (spec-backed, not compiler-backed: no
// C# compiler was available). Lookup visits the immediately enclosing type and
// its base classes, then each enclosing type and its bases, and only then the
// using directives of the enclosing namespace declarations. The resolver does
// not bind inherited or enclosing members, so where the spec picks one the
// graph must leave the call unresolved rather than name App.Util.
//
//	caller            call   spec target            graph
//	Derived.Run       Foo()  App.Base.Foo           unresolved
//	DerivedS.Run      Sfoo() App.BaseS.Sfoo         unresolved (static base member)
//	Outer.Inner.Go    Bar()  App.Outer.Bar          unresolved
//	Outer2.Inner.Go   Baz()  App.Base2.Baz          unresolved (enclosing type's base)
//	Impl.Run          Foo()  IThing.Foo (DIM)       unresolved (interface base)
//	Arity.Run         Foo()  CS1501 on Base1.Foo    unresolved (lookup stops at Base1)
//	StaticCtx.Run     Foo()  CS0120 on Base.Foo     unresolved (static caller)
//	Priv.Run          Foo()  App.Util.Foo           unresolved (private base member is excluded; refused anyway)
//	Self.Run          Foo()  App.Self.Foo           App.Self.Foo (same-type control)
//	Local.Run         Foo()  local function Foo     unresolved
//	Plain.Run         Foo()  App.Util.Foo           App.Util.Foo (no base, not nested)
//	Plain.Two         Qux()  CS0121 ambiguous       unresolved (two using static)
//	Plain.One         Only() App.Util2.Only         App.Util2.Only
//	Reopened.Run      Foo()  App.Util.Foo           App.Util.Foo (namespace reopened)
//	Typed.Run         h.Hi() App.Helper.Hi          App.Helper.Hi (typed receiver)
var csharpStaticUsingMembersTree = map[string]string{
	"Util.cs": `namespace App {
  public static class Util { public static void Foo() {} public static void Bar() {} public static void Baz() {} public static void Sfoo() {} public static void Qux() {} }
  public static class Util2 { public static void Qux() {} public static void Only() {} }
  public class Helper { public void Hi() {} }
}
`,
	"Bases.cs": `namespace App {
  public class Base { public void Foo() {} }
  public class BaseS { public static void Sfoo() {} }
  public class Base1 { public void Foo(int x) {} }
  public class Base2 { public static void Baz() {} }
  public class PBase { private void Foo() {} }
  public interface IThing { void Foo() {} }
}
`,
	"A.cs": `using static App.Util;
using static App.Util2;
namespace App {
  public class Derived : Base { public void Run() { Foo(); } }
  public class DerivedS : BaseS { public void Run() { Sfoo(); } }
  public class Outer {
    public static void Bar() {}
    public class Inner { public void Go() { Bar(); } }
  }
  public class Outer2 : Base2 { public class Inner { public void Go() { Baz(); } } }
  public class Impl : IThing { public void Run() { Foo(); } }
  public class Arity : Base1 { public void Run() { Foo(); } }
  public class StaticCtx : Base { public static void Run() { Foo(); } }
  public class Priv : PBase { public void Run() { Foo(); } }
  public class Self {
    public void Foo() {}
    public void Run() { Foo(); }
  }
  public class Local { public void Run() { void Foo() {} Foo(); } }
  public class Plain {
    public void Run() { Foo(); }
    public void Two() { Qux(); }
    public void One() { Only(); }
  }
  public class Typed : Base { public void Run() { Helper h = new Helper(); h.Hi(); } }
}
`,
	"B.cs": `using static App.Util;
namespace App { public class Reopened { public void Run() { Foo(); } } }
`,
}

var csharpStaticUsingMembersWant = map[string]string{
	"App.Derived.Run|Foo":     "",
	"App.DerivedS.Run|Sfoo":   "",
	"App.Outer.Inner.Go|Bar":  "",
	"App.Outer2.Inner.Go|Baz": "",
	"App.Impl.Run|Foo":        "",
	"App.Arity.Run|Foo":       "",
	"App.StaticCtx.Run|Foo":   "",
	"App.Priv.Run|Foo":        "",
	"App.Self.Run|Foo":        "App.Self.Foo",
	"App.Local.Run|Foo":       "",
	"App.Plain.Run|Foo":       "App.Util.Foo",
	"App.Plain.Two|Qux":       "",
	"App.Plain.One|Only":      "App.Util2.Only",
	"App.Reopened.Run|Foo":    "App.Util.Foo",
	"App.Typed.Run|h.Hi":      "App.Helper.Hi",
}

func writeCSharpStaticUsingMembersTree(t *testing.T, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, tree := range []map[string]string{csharpStaticUsingMembersTree, extra} {
		for path, content := range tree {
			if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func assertCSharpStaticUsingMembers(t *testing.T, s *store.Store, root, step string, want map[string]string) {
	t.Helper()
	got := csharpUsingTargets(t, s, root)
	for key, w := range want {
		if g, ok := got[key]; !ok || g != w {
			t.Errorf("%s: %s bound %q (present %v), want %q", step, key, g, ok, w)
		}
	}
}

// csharpStaticUsingReferenceTargets maps each call reference to the symbol it
// names, "" when it names none.
func csharpStaticUsingReferenceTargets(t *testing.T, s *profileStore, root string) map[string]string {
	t.Helper()
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.raw(t).Query(`SELECT c.qualified_name, r.name, COALESCE(d.qualified_name,'')
FROM references_tbl r JOIN symbols c ON c.id=r.context_symbol_id LEFT JOIN symbols d ON d.id=r.symbol_id
WHERE r.repo_id=?`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ctxName, name, dst string
		if err := rows.Scan(&ctxName, &name, &dst); err != nil {
			t.Fatal(err)
		}
		out[ctxName+"|"+name] = dst
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCSharpStaticUsingYieldsToInheritedAndEnclosingMembers(t *testing.T) {
	root := writeCSharpStaticUsingMembersTree(t, nil)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewCSharp()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	assertCSharpStaticUsingMembers(t, s.Store, root, "fresh", csharpStaticUsingMembersWant)
	refs := csharpStaticUsingReferenceTargets(t, s, root)
	for _, key := range []string{"App.Derived.Run|Foo", "App.Outer.Inner.Go|Bar"} {
		if refs[key] != "" {
			t.Errorf("reference %s names %q, want none", key, refs[key])
		}
	}
	if refs["App.Plain.Run|Foo"] != "App.Util.Foo" {
		t.Errorf("reference App.Plain.Run|Foo names %q, want App.Util.Foo", refs["App.Plain.Run|Foo"])
	}
}

// A global using static elsewhere is ambiguity evidence: even the no-base
// control refuses a name it could import.
func TestCSharpStaticUsingMembersGlobalUsingRefuses(t *testing.T) {
	root := writeCSharpStaticUsingMembersTree(t, map[string]string{"G.cs": "global using static App.Util;\n"})
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewCSharp()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	assertCSharpStaticUsingMembers(t, s.Store, root, "global", map[string]string{
		"App.Plain.Run|Foo":      "",
		"App.Derived.Run|Foo":    "",
		"App.Outer.Inner.Go|Bar": "",
		"App.Self.Run|Foo":       "App.Self.Foo",
	})
}

// A graph written before base lists were recorded bound App.Util for the
// inherited and enclosing cases. An ordinary update with no source change
// must clear those edges and their references, land where a fresh index
// does, and a second update does no work.
func TestCSharpStaticUsingMembersUpgradeClearsWrongTarget(t *testing.T) {
	ctx := context.Background()
	root := writeCSharpStaticUsingMembersTree(t, nil)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewCSharpV5()), nil).Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	// Reproduce the graph decided under resolver epoch 3.
	if _, err := s.raw(t).Exec(`UPDATE settings SET value='3' WHERE key LIKE 'resolver.policy.%.csharp'`); err != nil {
		t.Fatal(err)
	}
	// Without a recorded base list nothing preempts the using static target.
	legacy := csharpUsingTargets(t, s.Store, root)
	if legacy["App.Derived.Run|Foo"] != "App.Util.Foo" {
		t.Fatalf("legacy Derived.Run bound %q, want the wrong App.Util.Foo", legacy["App.Derived.Run|Foo"])
	}
	idx := New(s.Store, parser.NewRegistry(tsparser.NewCSharp()), nil)
	summary, err := idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "csharp" {
		t.Fatalf("update = %+v, want a csharp profile reparse", summary)
	}
	assertCSharpStaticUsingMembers(t, s.Store, root, "update", csharpStaticUsingMembersWant)
	refs := csharpStaticUsingReferenceTargets(t, s, root)
	for _, key := range []string{"App.Derived.Run|Foo", "App.Outer.Inner.Go|Bar"} {
		if refs[key] != "" {
			t.Errorf("update: reference %s names %q, want none", key, refs[key])
		}
	}
	var epoch string
	if err := s.raw(t).QueryRow(`SELECT value FROM settings WHERE key LIKE 'resolver.policy.%.csharp'`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != "4" {
		t.Fatalf("csharp resolver policy = %s, want 4", epoch)
	}
	assertCSharpFreshParity(t, s.Store, root)
	summary, err = idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	assertCSharpFreshParity(t, s.Store, root)
}
