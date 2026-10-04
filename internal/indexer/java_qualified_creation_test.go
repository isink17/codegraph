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

// javaQualifiedCreationTree has a top-level app.Inner and an inner class
// app.Outer.Inner, both with a no-argument constructor. Oracle (javac 17,
// `javap -c -p`):
//
//	Outer.self          this.new Inner()        invokespecial app/Outer$Inner."<init>":(Lapp/Outer;)V
//	Outer.selfQualified Outer.this.new Inner()  invokespecial app/Outer$Inner."<init>":(Lapp/Outer;)V
//	Outer.bare          new Inner()             invokespecial app/Outer$Inner."<init>":(Lapp/Outer;)V
//	Caller.viaVar       outer.new Inner()       invokespecial app/Outer$Inner."<init>":(Lapp/Outer;)V
//	Caller.plain        new Inner()             invokespecial app/Inner."<init>":()V
//
// A qualified creation constructs a member class of the qualifier's type,
// which a bare lookup of `Inner` does not find, so it must never bind the
// top-level namesake.
var javaQualifiedCreationTree = tree{
	"app/Inner.java": `package app; public class Inner { public Inner() {} }`,
	"app/Outer.java": `package app;
public class Outer {
    public class Inner { public Inner() {} }
    void self() { this.new Inner(); }
    void selfQualified() { Outer.this.new Inner(); }
    void bare() { new Inner(); }
}
`,
	"app/Caller.java": `package app;
public class Caller {
    void viaVar(Outer outer) { outer.new Inner(); }
    void plain() { new Inner(); }
}
`,
}

// javaConstructsTargets maps each caller to the qualified name its single
// `new` expression bound, or "" when it stayed unresolved.
func javaConstructsTargets(t *testing.T, r *lifecycleRepo) map[string]string {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range edges {
		if e.Kind == "constructs" {
			out[e.SrcQualifiedName] = e.DstQualifiedName
		}
	}
	return out
}

func assertJavaQualifiedCreationTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	got := javaConstructsTargets(t, r)
	for caller, want := range map[string]string{
		"app.Outer.self":          "",
		"app.Outer.selfQualified": "",
		// Outer declares Inner itself, which hides app.Inner (javac:
		// Outer$Inner).
		"app.Outer.bare":    "app.Outer.Inner.Inner",
		"app.Caller.viaVar": "",
		// Caller spells no supertype, so no inherited Inner can hide app.Inner.
		"app.Caller.plain": "app.Inner.Inner",
	} {
		if got[caller] != want {
			t.Errorf("%s: %s bound %q, want %q", step, caller, got[caller], want)
		}
	}
}

func TestJavaQualifiedCreationNeverBindsTopLevelNamesake(t *testing.T) {
	r := newLifecycleRepo(t, javaQualifiedCreationTree)
	assertJavaQualifiedCreationTargets(t, r, "fresh")
	caller := javaQualifiedCreationTree["app/Caller.java"]
	r.write(t, "app/Caller.java", strings.Replace(caller, "outer.new Inner();", "new Inner();", 1))
	r.update(t, "app/Caller.java")
	r.assertFreshParity(t, "qualified to plain")
	if got := javaConstructsTargets(t, r)["app.Caller.viaVar"]; got != "app.Inner.Inner" {
		t.Fatalf("plain creation bound %q, want app.Inner.Inner", got)
	}
	r.write(t, "app/Caller.java", caller)
	r.update(t, "app/Caller.java")
	r.assertFreshParity(t, "plain to qualified")
	assertJavaQualifiedCreationTargets(t, r, "plain to qualified")
}

// javaMemberShadowTree spells raw simple-name constructions that a member type
// can shadow. Oracle (javac 17, `javap -c -p`):
//
//	Sub.raw        new Box()    app/Base$Box   (inherited member type)
//	Outer$Mid.raw  new Inner()  app/Outer$Inner (member of an outer class)
//	Outer.own      new Inner()  app/Outer$Inner (member of the own class)
//	Imp.raw        new Box()    app/Imp$Box    (member beats `import other.Box`)
//	Free.raw       new Free()   app/Free
//
// Each caller method sits on its own line: methods sharing a line lose their
// constructs edges, which is a separate defect.
var javaMemberShadowTree = tree{
	"app/Box.java":   `package app; public class Box { public Box() {} }`,
	"app/Inner.java": `package app; public class Inner { public Inner() {} }`,
	"other/Box.java": `package other; public class Box { public Box() {} }`,
	"app/Base.java":  `package app; public class Base { public static class Box { public Box() {} } }`,
	"app/Sub.java":   `package app; public class Sub extends Base { void raw() { new Box(); } }`,
	"app/Outer.java": "package app; public class Outer { public static class Inner { public Inner() {} }\n public static class Mid { void raw() { new Inner(); } }\n void own() { new Inner(); } }",
	"app/Imp.java":   "package app; import other.Box; public class Imp { static class Box { Box() {} }\n void raw() { new Box(); } }",
	"app/Free.java":  "package app; public class Free { public Free() {}\n void raw() { new Free(); } }",
	// Anon.raw builds Base$Box inside an anonymous Base subclass and
	// Anon.local a local Box, though Anon declares its own member Box
	// (javac: app/Base$Box, app/Anon$1Box). Impl inherits Box from an
	// interface in another package (javac: other/I$Box).
	"app/Anon.java": "package app; public class Anon { static class Box { Box() {} }\n void raw() { new Base() { void h() { new Box(); } }; }\n void local() { class Box { Box() {} } new Box(); } }",
	"other/I.java":  "package other; public interface I { class Box { public Box() {} } }",
	"app/Impl.java": "package app; public class Impl implements other.I {\n void raw() { new Box(); } }",
}

func TestJavaSimpleNameConstructionRespectsMemberTypes(t *testing.T) {
	r := newLifecycleRepo(t, javaMemberShadowTree)
	got := javaConstructsTargets(t, r)
	for caller, want := range map[string]string{
		"app.Sub.raw":       "",                      // supertypes are not recorded
		"app.Outer.Mid.raw": "",                      // a nearer class could inherit an Inner
		"app.Outer.own":     "app.Outer.Inner.Inner", // own member type
		"app.Imp.raw":       "app.Imp.Box.Box",       // own member beats the import
		"app.Anon.raw":      "",                      // javac: app/Base$Box (anonymous subclass)
		"app.Anon.local":    "",                      // javac: app/Anon$1Box (local class)
		"app.Impl.raw":      "",                      // javac: other/I$Box (interface member)
		"app.Free.raw":      "app.Free.Free",
	} {
		if got[caller] != want {
			t.Errorf("%s bound %q, want %q", caller, got[caller], want)
		}
	}
	// Removing the member types that could shadow Box lets Sub bind the
	// package's Box again; the update must match a fresh index.
	r.write(t, "app/Base.java", `package app; public class Base {}`)
	r.write(t, "app/Imp.java", `package app; public class Imp {}`)
	r.write(t, "app/Anon.java", `package app; public class Anon {}`)
	r.write(t, "other/I.java", `package other; public interface I {}`)
	r.update(t, "app/Base.java", "app/Imp.java", "app/Anon.java", "other/I.java")
	r.assertFreshParity(t, "member types removed")
	if got := javaConstructsTargets(t, r)["app.Sub.raw"]; got != "app.Box.Box" {
		t.Fatalf("Sub.raw bound %q after the member types were removed", got)
	}
}

// An interface's member types are implicitly public and inherited by its
// implementers in any package, though recorded as package-private. Oracle
// (javac 17): Impl.raw new Box() -> other/I$Box.
func TestJavaInterfaceMemberTypeFromAnotherPackageShadows(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"app/Box.java":  `package app; public class Box { public Box() {} public static void m() {} }`,
		"other/I.java":  `package other; public interface I { class Box { public Box() {} public static void m() {} } }`,
		"app/Impl.java": "package app; public class Impl implements other.I {\n void raw() { new Box(); }\n}",
	})
	if got := javaConstructsTargets(t, r)["app.Impl.raw"]; got != "" {
		t.Fatalf("Impl.raw bound %q, want unresolved (javac: other/I$Box)", got)
	}
}

// A single-type import of a member type binds it when no other member type
// shares the name, or when the caller spells no supertype; another member
// type of that name, which a caller with a supertype could inherit, still
// refuses. Oracle (javac 17): Use.raw new Box() -> other/T$Box, and
// app/Base$Box once Use extends Base.
func TestJavaImportedMemberTypeBindsUnlessAnotherMemberShares(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"other/T.java": `package other; public class T { public static class Box { public Box() {} } }`,
		"app/Use.java": "package app; import other.T.Box; public class Use {\n void raw() { new Box(); }\n}",
	})
	if got := javaConstructsTargets(t, r)["app.Use.raw"]; got != "other.T.Box.Box" {
		t.Fatalf("Use.raw bound %q, want other.T.Box.Box", got)
	}
	r.write(t, "app/Base.java", `package app; public class Base { public static class Box { public Box() {} } }`)
	r.update(t, "app/Base.java")
	r.assertFreshParity(t, "second member type Box added")
	if got := javaConstructsTargets(t, r)["app.Use.raw"]; got != "other.T.Box.Box" {
		t.Fatalf("Use.raw bound %q with no supertype, want other.T.Box.Box", got)
	}
	r.write(t, "app/Use.java", "package app; import other.T.Box; public class Use extends Base {\n void raw() { new Box(); }\n}")
	r.update(t, "app/Use.java")
	r.assertFreshParity(t, "Use extends Base")
	if got := javaConstructsTargets(t, r)["app.Use.raw"]; got != "" {
		t.Fatalf("Use.raw bound %q with an inheritable member type Box, want unresolved", got)
	}
}

// javaOwnMemberTree constructs member types the calling class declares
// itself. A declared member type hides every inherited, enclosing, imported
// and package type of that name (JLS 6.4.1, 8.5), so it binds whatever the
// class extends. Oracle (javac 17, `javap -c -p`):
//
//	Own.make         new Box()            app/Own$Box
//	Own.makePrivate  new Hidden()         app/Own$Hidden
//	Own.makeGeneric  new G<String>()      app/Own$G
//	Own.lambda       () -> new Box()      app/Own$Box
//	Own.anon         new Box() in anon    app/Own$Box (Own$1.h)
//	Own.local        new Box() after local class Box  app/Own$1Box
//	Sub.make         new Box()            app/Sub$Box (Base.Box is hidden)
//	Imp.make         new Box()            app/Imp$Box (`import other.Box` is shadowed)
//	Outer$Mid.make   new Inner()          app/Outer$Inner
//	Outer$Near.make  new Inner()          app/Outer$Near$Inner
//	Two.make         new Box()            app/Two$Box
//	Iface.make       new Box()            app/Iface$Box
var javaOwnMemberTree = tree{
	"app/Box.java":   `package app; public class Box { public Box() {} }`,
	"other/Box.java": `package other; public class Box { public Box() {} }`,
	"app/Base.java":  `package app; public class Base { public static class Box { public Box() {} } }`,
	"app/Own.java": `package app;
public class Own {
    static class Box { Box() {} }
    private static class Hidden { Hidden() {} }
    static class G<T> { G() {} }
    void make() {
        new Box();
    }
    void makePrivate() {
        new Hidden();
    }
    void makeGeneric() {
        new G<String>();
    }
    void lambda() {
        Runnable r = () -> new Box();
    }
    void anon() {
        new Object() { void h() { new Box(); } };
    }
    void local() {
        class Box { Box() {} }
        new Box();
    }
}
`,
	"app/Sub.java": `package app;
public class Sub extends Base {
    static class Box { Box() {} }
    void make() {
        new Box();
    }
}
`,
	"app/Imp.java": `package app;
import other.Box;
public class Imp {
    static class Box { Box() {} }
    void make() {
        new Box();
    }
}
`,
	"app/Outer.java": `package app;
public class Outer {
    static class Inner { Inner() {} }
    static class Mid {
        void make() {
            new Inner();
        }
    }
    static class Near {
        static class Inner { Inner() {} }
        void make() {
            new Inner();
        }
    }
}
`,
	// Two.make ends on the line where C.other starts, so the edge is credited
	// to C.other, whose class declares another Box.
	"app/Two.java": `package app;
public class Two {
    static class Box { Box() {} }
    void make() {
        new Box(); } static class C extends Base { void other() {}
        static class Box { Box() {} } }
}
`,
	"app/Iface.java": `package app;
public interface Iface {
    class Box { public Box() {} }
    default void make() {
        new Box();
    }
}
`,
}

// javaConstructsByCallee maps "caller|class" to the qualified names every
// such construction bound ("" when unresolved), in source order.
func javaConstructsByCallee(t *testing.T, r *lifecycleRepo) map[string][]string {
	t.Helper()
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range edges {
		if e.Kind == "constructs" {
			k := e.SrcQualifiedName + "|" + e.DstName
			out[k] = append(out[k], e.DstQualifiedName)
		}
	}
	return out
}

func assertJavaOwnMemberTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	got := javaConstructsByCallee(t, r)
	for key, want := range map[string]string{
		"app.Own.make|Box":           "app.Own.Box.Box",
		"app.Own.makePrivate|Hidden": "app.Own.Hidden.Hidden",
		"app.Own.makeGeneric|G":      "app.Own.G.G",
		"app.Own.lambda|Box":         "app.Own.Box.Box",
		// An anonymous body may inherit a Box from its supertype.
		"app.Own.anon|Box": "",
		// The local class Box is not recorded.
		"app.Own.local|Box":         "",
		"app.Sub.make|Box":          "app.Sub.Box.Box",
		"app.Imp.make|Box":          "app.Imp.Box.Box",
		"app.Outer.Near.make|Inner": "app.Outer.Near.Inner.Inner",
		// Mid could inherit an Inner from a supertype that is not recorded.
		"app.Outer.Mid.make|Inner": "",
		// Two.make shares its last line with C.other. The column credits the
		// construction to make (javac: app/Two$Box), but the same-line
		// resolver guard still leaves its target unresolved.
		"app.Two.make|Box":   "",
		"app.Iface.make|Box": "app.Iface.Box.Box",
	} {
		if g := got[key]; len(g) != 1 || g[0] != want {
			t.Errorf("%s: %s bound %q, want [%q]", step, key, g, want)
		}
	}
}

func TestJavaOwnMemberTypeConstructionBinds(t *testing.T) {
	r := newLifecycleRepo(t, javaOwnMemberTree)
	assertJavaOwnMemberTargets(t, r, "fresh")
	// Without Own's member Box, make names the package type app.Box: Own
	// spells no supertype, so Base.Box cannot be inherited (javac: app/Box).
	// Restoring the member binds it again. Each update must match a fresh
	// index.
	own := javaOwnMemberTree["app/Own.java"]
	r.write(t, "app/Own.java", strings.Replace(own, "    static class Box { Box() {} }\n", "", 1))
	r.update(t, "app/Own.java")
	r.assertFreshParity(t, "own member removed")
	if got := javaConstructsByCallee(t, r)["app.Own.make|Box"]; len(got) != 1 || got[0] != "app.Box.Box" {
		t.Fatalf("Own.make bound %q without the member type, want app.Box.Box", got)
	}
	r.write(t, "app/Own.java", own)
	r.update(t, "app/Own.java")
	r.assertFreshParity(t, "own member restored")
	assertJavaOwnMemberTargets(t, r, "own member restored")
}

// A graph written by the v6 Java parser carries no own-member marker, so its
// own-member constructions stay unresolved. Updating it with the current
// parser re-parses the Java files and must land exactly where a fresh index
// lands; a second update changes nothing.
func TestJavaOwnMemberTypeProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaOwnMemberTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV6()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Own.java"); got != "treesitter:java:v6" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := javaConstructsByCallee(t, r)["app.Own.make|Box"]; len(got) != 1 || got[0] != "" {
		t.Fatalf("legacy Own.make bound %q, want unresolved", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaOwnMemberTargets(t, r, "update")
	r.assertFreshParity(t, "java own member profile convergence")
	summary, err = r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	r.assertFreshParity(t, "java own member second update")
}
