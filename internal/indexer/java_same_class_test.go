//go:build cgo

package indexer

import (
	"strings"
	"testing"
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
