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

// An unqualified or `this.`-qualified method call in a class body names a
// method of that class (JLS 15.12.1: the innermost class declaring a method
// of that name is searched; members of other classes are not in scope). The
// caller's class must be found by the same package-qualified identity its
// methods are declared under, so a packaged file and the default package
// answer alike, on every entrypoint. Inherited members are not modelled and
// stay unresolved.
func TestJavaSameClassCallsBindInPackagedAndDefaultFiles(t *testing.T) {
	type call struct{ dst, want string } // want "" means unresolved
	cases := []struct {
		name   string
		caller string
		src    string
		others tree
		calls  []call
	}{
		{
			name:   "packaged",
			caller: "app/Caller.java",
			src: `package app;

public class Caller {
    private void helper() {}
    private static int twice(int x, int y) { return x; }
    public void run() {
        helper();
        this.helper();
        twice(1, 2);
        Caller.twice(1, 2);
        inherited();
        other();
    }
}
`,
			others: tree{
				"app/Base.java":  "package app;\n\npublic class Base {\n    public void inherited() {}\n}\n",
				"app/Other.java": "package app;\n\npublic class Other {\n    public void other() {}\n    public void helper() {}\n    private static void hidden() {}\n}\n",
			},
			calls: []call{
				{"helper", "app/Caller.java:app.Caller.helper(function)"},
				{"this.helper", "app/Caller.java:app.Caller.helper(function)"},
				{"twice", "app/Caller.java:app.Caller.twice(function)"},
				{"Caller.twice", "app/Caller.java:app.Caller.twice(function)"},
				{"inherited", ""}, // not declared in Caller; inheritance is not modelled
				{"other", ""},     // a method of another class is not in scope
			},
		},
		{
			name:   "nested class in a packaged file",
			caller: "app/Outer.java",
			src: `package app;

public class Outer {
    public void helper() {}
    public static class Inner {
        void helper() {}
        void run() { helper(); }
    }
}
`,
			others: tree{"app/Other.java": "package app;\n\npublic class Other {\n    public void unrelated() {}\n}\n"},
			calls:  []call{{"helper", "app/Outer.java:app.Outer.Inner.helper(function)"}},
		},
		{
			// JLS 6.4.1: a method declared in the class shadows a
			// single-static-import of the same name.
			name:   "own method shadows a static import",
			caller: "app/Caller.java",
			src:    "package app;\n\nimport static app.Util.helper;\n\npublic class Caller {\n    void helper() {}\n    void run() { helper(); }\n}\n",
			others: tree{"app/Util.java": "package app;\n\npublic class Util {\n    public static void helper() {}\n}\n"},
			calls:  []call{{"helper", "app/Caller.java:app.Caller.helper(function)"}},
		},
		{
			name:   "private method of another class",
			caller: "app/Caller.java",
			src:    "package app;\n\npublic class Caller {\n    void run() { Other.hidden(); }\n}\n",
			others: tree{"app/Other.java": "package app;\n\npublic class Other {\n    private static void hidden() {}\n}\n"},
			calls:  []call{{"Other.hidden", ""}},
		},
		{
			// JLS 15.12.1: the anonymous, local or enum-constant class is the
			// innermost class with a member named helper, so the enclosing
			// class's helper is not the target. Those classes' members are not
			// modelled, so the calls stay unresolved; a lambda is not a class
			// and keeps binding the enclosing class's method.
			name:   "nested class bodies",
			caller: "app/Caller.java",
			src: `package app;

import static app.Util.util;

public class Caller {
    void helper() {}
    void lambdaUser() {}
    void run() {
        new Runnable() {
            public void run() { helper(); this.helper(); util(); }
            void helper() {}
            void util() {}
        };
        class Local {
            void helper() {}
            void go() { helper(); }
        }
        record Point(int x) {
            void helper() {}
            void go() { helper(); }
        }
        Runnable r = () -> lambdaUser();
    }
}
`,
			others: tree{"app/Util.java": "package app;\n\npublic class Util {\n    public static void util() {}\n}\n"},
			calls: []call{
				{"helper", ""},
				{"this.helper", ""},
				{"util", ""},
				{"lambdaUser", "app/Caller.java:app.Caller.lambdaUser(function)"},
			},
		},
		{
			// JLS 6.4.1: Outer.helper is in scope in Inner and shadows the
			// static import; whether it is callable from the static nested
			// class is not modelled, so the call stays unresolved.
			name:   "enclosing method shadows a static import",
			caller: "app/Outer.java",
			src:    "package app;\n\nimport static app.Util.helper;\n\npublic class Outer {\n    static void helper() {}\n    static class Inner {\n        void run() { helper(); }\n    }\n}\n",
			others: tree{"app/Util.java": "package app;\n\npublic class Util {\n    public static void helper() {}\n}\n"},
			calls:  []call{{"helper", ""}},
		},
		{
			// The same shadowing through a class named like its package,
			// whose members the adapter stores under the package name.
			name:   "package-named enclosing class shadows a static import",
			caller: "a/a.java",
			src:    "package a;\n\nimport static a.Util.g;\n\npublic class a {\n    static void g() {}\n    static class b {\n        void h() { g(); }\n    }\n}\n",
			others: tree{"a/Util.java": "package a;\n\npublic class Util {\n    public static void g() {}\n}\n"},
			calls:  []call{{"g", ""}},
		},
		{
			// Overloads in the calling class shadow the import even though
			// neither is chosen.
			name:   "own overloads shadow a static import",
			caller: "a/a.java",
			src:    "package a;\n\nimport static a.Util.g;\n\npublic class a {\n    void g(int x) {}\n    void g(String x) {}\n    void run() { g(1); }\n}\n",
			others: tree{"a/Util.java": "package a;\n\npublic class Util {\n    public static void g(int x) {}\n}\n"},
			calls:  []call{{"g", ""}},
		},
		{
			// The adapter collapses a container equal to the package name.
			name:   "class named like its package",
			caller: "app/app.java",
			src:    "package app;\n\npublic class app {\n    void helper() {}\n    void run() { helper(); }\n}\n",
			others: tree{"app/Other.java": "package app;\n\npublic class Other {\n    public void helper() {}\n}\n"},
			calls:  []call{{"helper", "app/app.java:app.helper(function)"}},
		},
		{
			name:   "default package anonymous class",
			caller: "Caller.java",
			src:    "public class Caller {\n    void helper() {}\n    void run() {\n        new Runnable() {\n            public void run() { helper(); }\n            void helper() {}\n        };\n    }\n}\n",
			others: tree{"Other.java": "public class Other {\n    public void unrelated() {}\n}\n"},
			calls:  []call{{"helper", ""}},
		},
		{
			name:   "default package",
			caller: "Caller.java",
			src:    "public class Caller {\n    private void helper() {}\n    void run() { helper(); this.helper(); }\n}\n",
			others: tree{"Other.java": "public class Other {\n    public void helper() {}\n}\n"},
			calls: []call{
				{"helper", "Caller.java:Caller.helper(function)"},
				{"this.helper", "Caller.java:Caller.helper(function)"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			all := tree{c.caller: c.src}
			var otherPaths []string
			for p, src := range c.others {
				all[p] = src
				otherPaths = append(otherPaths, p)
			}
			entrypoints := map[string]func() *lifecycleRepo{
				"fresh": func() *lifecycleRepo { return newLifecycleRepo(t, all) },
				"names": func() *lifecycleRepo {
					r := newLifecycleRepo(t, tree{c.caller: c.src})
					for p, src := range c.others {
						r.write(t, p, src)
					}
					r.update(t, otherPaths...)
					return r
				},
				"paths": func() *lifecycleRepo {
					r := newLifecycleRepo(t, all)
					r.write(t, c.caller, "// touched\n"+c.src)
					r.update(t, c.caller)
					return r
				},
				"combined": func() *lifecycleRepo {
					r := newLifecycleRepo(t, tree{c.caller: c.src})
					for p, src := range c.others {
						r.write(t, p, src)
					}
					r.write(t, c.caller, "// touched\n"+c.src)
					r.update(t, append([]string{c.caller}, otherPaths...)...)
					return r
				},
			}
			for _, entry := range []string{"fresh", "names", "paths", "combined"} {
				r := entrypoints[entry]()
				for _, call := range c.calls {
					got := r.edgeState(t, c.caller, call.dst)
					if call.want == "" {
						if !strings.Contains(got, ":: [/]") {
							t.Errorf("%s: %s = %s, want unresolved", entry, call.dst, got)
						}
					} else if !strings.Contains(got, call.want) {
						t.Errorf("%s: %s = %s, want %s", entry, call.dst, got, call.want)
					}
				}
				r.assertFreshParity(t, entry)
			}
		})
	}
}

// A private constructor is visible only inside its own class. A same-named
// class in another package is a different owner, while the class itself may
// construct itself through its private constructor.
func TestJavaPrivateConstructorOwnerIsTheQualifiedClass(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib/Caller.java": "package lib;\n\npublic class Caller {\n    private Caller() {}\n}\n",
		"app/Caller.java": "package app;\n\npublic class Caller {\n    private Caller(int x) {}\n    void foreign() { new lib.Caller(); }\n    static Caller make() { return new Caller(1); }\n}\n",
	})
	got := constructorTargets(t, r)
	if got["app.Caller.foreign"] != "" {
		t.Errorf("new lib.Caller() from app.Caller bound the private %q", got["app.Caller.foreign"])
	}
	if got["app.Caller.make"] != "private Caller(int x)" {
		t.Errorf("new Caller(1) from Caller bound %q, want its private constructor", got["app.Caller.make"])
	}
}

// A v4 graph carries no nested-class-scope marks, so calls inside anonymous
// classes can still be bound to the enclosing class there. Updating with the
// current parser re-parses Java files and must land where a fresh index does.
func TestJavaNestedClassScopeProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := tree{
		"app/Caller.java": "package app;\n\npublic class Caller {\n    void helper() {}\n    void run() {\n        new Runnable() {\n            public void run() { helper(); }\n            void helper() {}\n        };\n        helper();\n    }\n}\n",
	}
	for path, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewJavaV4()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v4" {
		t.Fatalf("legacy profile = %q", got)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "java" {
		t.Fatalf("update = %+v, want a java profile reparse", summary)
	}
	if got := fileParserProfile(t, s.raw(t), repo, "app/Caller.java"); got != "treesitter:java:v8" {
		t.Fatalf("updated profile = %q", got)
	}
	bound := 0
	for _, line := range r.projection(t) {
		if strings.Contains(line, `-calls-> "helper"`) && strings.Contains(line, "app.Caller.helper(function)") {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("helper bound %d times after update, want only the call outside the anonymous class:\n%s", bound, strings.Join(r.projection(t), "\n"))
	}
	r.assertFreshParity(t, "java nested class scope profile convergence")
}
