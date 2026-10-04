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

// javaNoInheritedTypeTree constructs the simple name Bag, which a top-level
// app.Bag and two member types (app.Base.Bag, other.Holder.Bag) share. A class
// that, with every class enclosing it, spells no supertype inherits only
// java.lang.Object, Enum or Record, none of which declares a member type, so
// the package or import type answers. Oracle (javac 17, `javap -c -p`):
//
//	Plain.make       new Bag()                 app/Bag
//	Plain.lambda     () -> new Bag()           app/Bag
//	Api.make         new Bag()                 app/Bag
//	Nest$In.make     new Bag()                 app/Bag
//	Imp.make         new Bag()                 other/Holder$Bag (single-type import)
//	SImp.make        new Bag()                 other/Holder$Bag (single-static-import)
//	SSpace.make      new Bag()                 other/Holder$Bag (`import static other.Holder. Bag;`)
//	TSpace.make      new Bag()                 other/Holder$Bag (`import other.Holder /*c*/ .Bag;`)
//	SWild.make       new Bag()                 app/Bag (static on-demand import never shadows)
//	Sub.make         new Bag()                 app/Base$Bag (inherited member)
//	Impl$In.make     new Bag()                 app/Bag (Impl implements Runnable)
//	Encl$In.make     new Bag()                 app/Encl$Bag (enclosing member)
//	Anon.make        new Bag() in `new Base(){}` app/Base$Bag (anonymous subclass)
//	Local.make       new Bag() after local class Bag  app/Local$1Bag
//	Field.bag        field initializer new Bag()      app/Bag
//
// SImp, SSpace, Impl$In and Field stay unresolved: a static import is not
// resolved to a type, an implements clause is not followed, and a field
// initializer is not marked. TSpace's comment is not part of the imported
// name, so it binds like Imp.
var javaNoInheritedTypeTree = tree{
	"app/Bag.java":      `package app; public class Bag { public Bag() {} }`,
	"app/Base.java":     `package app; public class Base { public static class Bag { public Bag() {} } }`,
	"other/Holder.java": `package other; public class Holder { public static class Bag { public Bag() {} } }`,
	"app/Plain.java":    "package app;\npublic class Plain {\n    void make() {\n        new Bag();\n    }\n    void lambda() {\n        Runnable r = () -> new Bag();\n    }\n}\n",
	"app/Api.java":      "package app;\npublic interface Api {\n    default void make() {\n        new Bag();\n    }\n}\n",
	"app/Nest.java":     "package app;\npublic class Nest {\n    static class In {\n        void make() {\n            new Bag();\n        }\n    }\n}\n",
	"app/Imp.java":      "package app;\nimport other.Holder.Bag;\npublic class Imp {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/SImp.java":     "package app;\nimport static other.Holder.Bag;\npublic class SImp {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/SSpace.java":   "package app;\nimport static other.Holder. Bag;\npublic class SSpace {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/TSpace.java":   "package app;\nimport other.Holder /*c*/ .Bag;\npublic class TSpace {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/SWild.java":    "package app;\nimport static other.Holder.*;\npublic class SWild {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/Sub.java":      "package app;\npublic class Sub extends Base {\n    void make() {\n        new Bag();\n    }\n}\n",
	"app/Impl.java":     "package app;\npublic class Impl implements Runnable {\n    public void run() {}\n    static class In {\n        void make() {\n            new Bag();\n        }\n    }\n}\n",
	"app/Encl.java":     "package app;\npublic class Encl {\n    static class Bag { Bag() {} }\n    static class In {\n        void make() {\n            new Bag();\n        }\n    }\n}\n",
	"app/Anon.java":     "package app;\npublic class Anon {\n    void make() {\n        new Base() {\n            void h() {\n                new Bag();\n            }\n        };\n    }\n}\n",
	"app/Local.java":    "package app;\npublic class Local {\n    void make() {\n        class Bag { Bag() {} }\n        new Bag();\n    }\n}\n",
	"app/Field.java":    "package app;\npublic class Field {\n    Bag bag = new Bag();\n}\n",
}

func assertJavaNoInheritedTypeTargets(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	got := javaConstructsByCallee(t, r)
	for key, want := range map[string]string{
		"app.Plain.make|Bag":   "app.Bag.Bag",
		"app.Plain.lambda|Bag": "app.Bag.Bag",
		"app.Api.make|Bag":     "app.Bag.Bag",
		"app.Nest.In.make|Bag": "app.Bag.Bag",
		"app.Imp.make|Bag":     "other.Holder.Bag.Bag",
		"app.SImp.make|Bag":    "",
		"app.SSpace.make|Bag":  "",
		"app.TSpace.make|Bag":  "other.Holder.Bag.Bag",
		"app.SWild.make|Bag":   "app.Bag.Bag",
		"app.Sub.make|Bag":     "",
		"app.Impl.In.make|Bag": "",
		"app.Encl.In.make|Bag": "",
		"app.Anon.make|Bag":    "",
		"app.Local.make|Bag":   "",
	} {
		if g := got[key]; len(g) != 1 || g[0] != want {
			t.Errorf("%s: %s bound %q, want [%q]", step, key, g, want)
		}
	}
}

func TestJavaNoInheritedTypeCreationBindsPackageType(t *testing.T) {
	r := newLifecycleRepo(t, javaNoInheritedTypeTree)
	assertJavaNoInheritedTypeTargets(t, r, "fresh")
	// A supertype clause on Plain makes Base.Bag inheritable (javac:
	// app/Base$Bag), so make stays unresolved; removing it binds app.Bag
	// again. Each update must match a fresh index.
	plain := javaNoInheritedTypeTree["app/Plain.java"]
	r.write(t, "app/Plain.java", strings.Replace(plain, "public class Plain {", "public class Plain extends Base {", 1))
	r.update(t, "app/Plain.java")
	r.assertFreshParity(t, "supertype added")
	if got := javaConstructsByCallee(t, r)["app.Plain.make|Bag"]; len(got) != 1 || got[0] != "" {
		t.Fatalf("Plain.make bound %q with a supertype, want unresolved", got)
	}
	r.write(t, "app/Plain.java", plain)
	r.update(t, "app/Plain.java")
	r.assertFreshParity(t, "supertype removed")
	assertJavaNoInheritedTypeTargets(t, r, "supertype removed")
	// A second top-level Bag in the package makes the name ambiguous to the
	// graph, so nothing binds it.
	r.write(t, "app2/Bag.java", `package app; public class Bag { public Bag() {} }`)
	r.update(t, "app2/Bag.java")
	r.assertFreshParity(t, "second package type")
	if got := javaConstructsByCallee(t, r)["app.Plain.make|Bag"]; len(got) != 1 || got[0] != "" {
		t.Fatalf("Plain.make bound %q with two app.Bag, want unresolved", got)
	}
}

// A graph written by the v8 Java parser carries no no-supertype marker, so
// those constructions stay unresolved. Updating it with the current parser
// re-parses the Java files and must land exactly where a fresh index does.
func TestJavaNoInheritedTypeProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range javaNoInheritedTypeTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV8()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Plain.java"); got != "treesitter:java:v8" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := javaConstructsByCallee(t, r)["app.Plain.make|Bag"]; len(got) != 1 || got[0] != "" {
		t.Fatalf("legacy Plain.make bound %q, want unresolved", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	assertJavaNoInheritedTypeTargets(t, r, "update")
	r.assertFreshParity(t, "java no-inherited profile convergence")
	summary, err = r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesIndexed != 0 || len(summary.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no work", summary)
	}
	r.assertFreshParity(t, "java no-inherited second update")
}
