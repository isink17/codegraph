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

func TestJavaPackageImportAndConstructorScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Util.java":   `package a; public class Util { public static void run() {} }`,
		"a/Foo.java":    `package a; public class Foo { public Foo() {} }`,
		"b/Caller.java": `package b; import static a.Util.run; class Caller { void x() { run(); new a.Foo(); } }`,
		"c/Foo.java":    `package c; public class Foo { public Foo() {} }`,
	})
	if got := r.edgeState(t, "b/Caller.java", "run"); !strings.Contains(got, "a/Util.java") || !strings.Contains(got, "java_static_import/high") {
		t.Fatalf("static import = %s", got)
	}
	found := false
	for _, line := range r.projection(t) {
		if strings.Contains(line, `b/Caller.java:`) && strings.Contains(line, `a.Foo.Foo`) && strings.Contains(line, "java_constructor/high") {
			found = true
		}
	}
	if !found {
		t.Fatalf("constructor not resolved: %v", r.projection(t))
	}
}

func TestJavaPackageIsolationRejectsUnimportedType(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Foo.java":    `package a; public class Foo { public static void run() {} }`,
		"b/Caller.java": `package b; class Caller { void x() { Foo.run(); } }`,
	})
	if got := r.edgeState(t, "b/Caller.java", "Foo.run"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("unimported type = %s", got)
	}
}

func TestJavaWildcardImportResolvesSingleCandidate(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Foo.java": `package a; public class Foo { public static void run() {} }`,
		"b/C.java":   "package b; import a.*; class C { void x() {\n Foo.run();\n } }",
	})
	if got := r.edgeState(t, "b/C.java", "Foo.run"); !strings.Contains(got, "a/Foo.java") {
		t.Fatalf("wildcard import = %s", got)
	}
}

func TestJavaTypeMemberRequiresUniqueStaticMethodAndOwnerProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, target, caller, want string
	}{
		{"ambiguous static overload", `package app; class Service { static void run(String x) {} static void run(byte[] x) {} }`, `package app; class Caller { void call() { Service.run(null); } }`, ":: [/]"},
		{"instance refusal", `package app; class Service { void run() {} }`, `package app; class Caller { void call() { Service.run(); } }`, ":: [/]"},
		{"same package static", `package app; class Service { static void run() {} }`, `package app; class Caller { void call() { Service.run(); } }`, "java_package_scope/high"},
		{"import provenance", `package lib; public class Service { public static void run() {} }`, `package app; import lib.Service; class Caller { void call() { Service.run(); } }`, "java_import_scope/high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{"Service.java": tc.target, "Caller.java": tc.caller})
			if got := r.edgeState(t, "Caller.java", "Service.run"); !strings.Contains(got, tc.want) {
				t.Fatalf("Service.run = %s, want %s", got, tc.want)
			}
		})
	}
}

// constructorTargets maps each caller method to the declaration its single
// `new` expression bound, by declared signature, or "" when it stayed
// unresolved. Overloaded constructors share one qualified name, so the
// signature is what tells them apart.
func constructorTargets(t *testing.T, r *lifecycleRepo) map[string]string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	signature := map[int64]string{}
	for _, sym := range symbols {
		signature[sym.ID] = sym.Signature
	}
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range edges {
		if e.Kind != "constructs" {
			continue
		}
		if e.DstSymbolID == nil {
			out[e.SrcQualifiedName] = ""
			continue
		}
		out[e.SrcQualifiedName] = signature[*e.DstSymbolID]
	}
	return out
}

// javaArityTree declares constructors whose overloads differ only in
// parameter count, and calls each with an argument list whose source text
// holds extra commas or parentheses: nested calls, generic types, lambdas,
// string and char literals, annotations and varargs. Java selects a
// constructor by its argument expressions, never by the punctuation inside
// them.
var javaArityTree = tree{
	"app/Nest.java":        `package app; public class Nest { public Nest(int a) {} public Nest(int a, int b) {} }`,
	"app/Gen.java":         `package app; import java.util.Map; public class Gen { public Gen(Map<String, Integer> m) {} public Gen(int a, int b) {} }`,
	"app/Lam.java":         `package app; import java.util.function.IntUnaryOperator; public class Lam { public Lam(IntUnaryOperator f) {} public Lam(int a, int b) {} }`,
	"app/Str.java":         `package app; public class Str { public Str(String s) {} public Str(int a, int b) {} }`,
	"app/Chr.java":         `package app; public class Chr { public Chr(char c) {} public Chr(int a, int b) {} }`,
	"app/Var.java":         `package app; public class Var { public Var(int... xs) {} }`,
	"app/VarFix.java":      `package app; public class VarFix { public VarFix(int a) {} public VarFix(int... xs) {} }`,
	"app/Zero.java":        `package app; public class Zero { public Zero() {} public Zero(int a) {} }`,
	"app/DeclGen.java":     `package app; import java.util.Map; public class DeclGen { public DeclGen(Map<String, Integer> m) {} public DeclGen(int a) {} }`,
	"app/DeclGenOnly.java": `package app; import java.util.Map; public class DeclGenOnly { public DeclGenOnly(Map<String, Integer> m) {} }`,
	"app/Two.java":         `package app; public class Two { public Two(int a) {} public Two(int a, int b) {} }`,
	"app/Ann.java":         `package app; public class Ann { public Ann(@SuppressWarnings({"a", "b"}) int a) {} public Ann(int a, int b) {} }`,
	"app/Bad.java":         `package app; public class Bad { public Bad(int... a, int b) {} }`,
	"app/Caller.java": `package app;

import java.util.Map;

public class Caller {
    static int bar(int x, int y) { return x; }
    void nest() { new Nest(bar(1, 2)); }
    void gen(int a, int b) { new Gen(Map.of("a", a, "b", b)); }
    void lam() { new Lam(x -> bar(x, x)); }
    void str() { new Str("a, b)"); }
    void chr() { new Chr(','); }
    void var3() { new Var(1, 2, 3); }
    void var0() { new Var(); }
    void varFix() { new VarFix(1); }
    void zero() { new Zero(); }
    void declGen(Map<String, Integer> m) { new DeclGen(m); }
    void declGenOnly(Map<String, Integer> m) { new DeclGenOnly(m); }
    void two() { new Two(1, 2); }
    void one() { new Two(1); }
    void ann() { new Ann(1); }
    void broken() { new Two(1,); }
    void bad() { new Bad(1, 2); }
    void anon() { new Two(1) { }; }
}
`,
}

var javaArityWant = map[string]string{
	"app.Caller.nest":        "public Nest(int a)",
	"app.Caller.gen":         "public Gen(Map<String, Integer> m)",
	"app.Caller.lam":         "public Lam(IntUnaryOperator f)",
	"app.Caller.str":         "public Str(String s)",
	"app.Caller.chr":         "public Chr(char c)",
	"app.Caller.var3":        "public Var(int... xs)",
	"app.Caller.var0":        "public Var(int... xs)",
	"app.Caller.varFix":      "", // a fixed and a varargs constructor both accept one argument; types are not modelled
	"app.Caller.zero":        "public Zero()",
	"app.Caller.declGen":     "", // two one-parameter constructors; types are not modelled
	"app.Caller.declGenOnly": "public DeclGenOnly(Map<String, Integer> m)",
	"app.Caller.two":         "public Two(int a, int b)",
	"app.Caller.one":         "public Two(int a)",
	"app.Caller.ann":         `public Ann(@SuppressWarnings({"a", "b"}) int a)`,
	"app.Caller.broken":      "", // the argument list does not parse, so its count is unknown
	"app.Caller.bad":         "", // varargs before another parameter: the declared count is unknown
	"app.Caller.anon":        "public Two(int a)",
}

func TestJavaConstructorArityIsCountedFromSyntax(t *testing.T) {
	r := newLifecycleRepo(t, javaArityTree)
	got := constructorTargets(t, r)
	for caller, want := range javaArityWant {
		if got[caller] != want {
			t.Errorf("%s: new bound %q, want %q", caller, got[caller], want)
		}
	}
	if len(got) != len(javaArityWant) {
		t.Errorf("constructs edges = %v, want one per caller in %v", got, javaArityWant)
	}
}

// A graph written by the previous Java parser carries no constructor arity
// facts. Updating it with the current parser re-parses the Java files and must
// land exactly where a fresh index lands.
func TestJavaConstructorArityProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaArityTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV3()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v3" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	// Without arity facts no constructor call is decided.
	legacyTargets := constructorTargets(t, r)
	if len(legacyTargets) != len(javaArityWant) {
		t.Fatalf("legacy constructs edges = %v, want one per caller in %v", legacyTargets, javaArityWant)
	}
	for caller, target := range legacyTargets {
		if target != "" {
			t.Fatalf("legacy %s bound %q with no arity evidence", caller, target)
		}
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v11" {
		t.Fatalf("updated profile = %q", got)
	}
	got := constructorTargets(t, r)
	for caller, want := range javaArityWant {
		if got[caller] != want {
			t.Errorf("%s: new bound %q after update, want %q", caller, got[caller], want)
		}
	}
	r.assertFreshParity(t, "java constructor arity profile convergence")
}
