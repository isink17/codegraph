//go:build cgo

package indexer

import (
	"strings"
	"testing"
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
		// javac binds Outer$Inner, but the edge could sit in an anonymous or
		// local class body whose own type hides it, so it stays unresolved.
		"app.Outer.bare":    "",
		"app.Caller.viaVar": "",
		// javac binds app.Inner, but Caller's supertypes are not recorded and
		// a public member type Inner exists, so the name stays unresolved.
		"app.Caller.plain": "",
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
	if got := javaConstructsTargets(t, r)["app.Caller.viaVar"]; got != "" {
		t.Fatalf("plain creation bound %q", got)
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
		"app.Sub.raw":       "", // supertypes are not recorded
		"app.Outer.Mid.raw": "", // a nearer class could inherit an Inner
		"app.Outer.own":     "", // an anonymous or local body could hide it
		"app.Imp.raw":       "", // never the imported other.Box
		"app.Anon.raw":      "", // javac: app/Base$Box (anonymous subclass)
		"app.Anon.local":    "", // javac: app/Anon$1Box (local class)
		"app.Impl.raw":      "", // javac: other/I$Box (interface member)
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
// (javac 17): Impl.raw new Box() -> other/I$Box; Impl.st Box.m() -> other/I$Box.m.
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
// shares the name; another member type of that name, which the caller could
// inherit, still refuses. Oracle (javac 17): Use.raw new Box() -> other/T$Box.
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
	if got := javaConstructsTargets(t, r)["app.Use.raw"]; got != "" {
		t.Fatalf("Use.raw bound %q with another member type Box present, want unresolved", got)
	}
}
