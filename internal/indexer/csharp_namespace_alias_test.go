//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const csharpAliasPrefixDefs = `namespace a.N { public static class Util { public static void Run() {} } }
namespace other { public static class Util { public static void Run() {} } }
namespace N { public static class Util { public static void Run() {} } }`

// A dotted qualifier whose first segment is an alias in scope is a namespace
// alias prefix (C# 7.7.1): it resolves through the alias, never through the
// declared namespaces, so a declared namespace of the same name must not bind.
func TestCSharpNamespaceAliasPrefixIsRefused(t *testing.T) {
	call := `public class Caller { public void M() { N.Util.Run(); } }`
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"adjacent namespace alias prefix", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using N = other; ` + call + ` }`}, ""},
		{"alias to an unindexed namespace", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using N = ext.Lib; ` + call + ` }`}, ""},
		{"ancestor namespace alias prefix", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using N = other; namespace c { ` + call + ` } }`}, ""},
		{"root alias prefix", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `using N = other; namespace a.b { ` + call + ` }`}, ""},
		{"file-scoped root alias prefix", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": "using N = other;\nnamespace a.b;\n" + call}, ""},
		{"no alias still binds through the namespace", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { ` + call + ` }`}, "a.N.Util.Run"},
		{"alias of another name does not refuse", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using M = other; ` + call + ` }`}, "a.N.Util.Run"},
		{"alias in another namespace's declaration is out of scope", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace x { using N = other; } namespace a.b { ` + call + ` }`}, "a.N.Util.Run"},
		{"alias in another file is out of scope", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "Other.cs": `namespace a.b { using N = other; }`, "App.cs": `namespace a.b { ` + call + ` }`}, "a.N.Util.Run"},
		{"direct type alias still binds", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using U = other.Util; public class Caller { public void M() { U.Run(); } } }`}, "other.Util.Run"},
		{"global qualification ignores the alias", map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": `namespace a.b { using N = other; public class Caller { public void M() { global::N.Util.Run(); } } }`}, "N.Util.Run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, root := indexCSharpFiles(t, tc.files)
			name := "N.Util.Run"
			switch tc.name {
			case "direct type alias still binds":
				name = "U.Run"
			case "global qualification ignores the alias":
				name = "global::N.Util.Run"
			}
			if got := csharpTarget(t, s, root, "App.cs", name); got != tc.want {
				t.Fatalf("target=%q, want %q", got, tc.want)
			}
			assertCSharpFreshParity(t, s, root)
		})
	}
}

// Adding or removing the alias retargets the edge through both update shapes,
// leaves no stale target or caller edge, and a repeated update changes nothing.
func TestCSharpNamespaceAliasPrefixFollowsIncrementalChanges(t *testing.T) {
	body := `public class Caller { public void M() { N.Util.Run(); } }`
	steps := []struct{ name, app, want string }{
		{"no alias", `namespace a.b { ` + body + ` }`, "a.N.Util.Run"},
		{"alias added", `namespace a.b { using N = other; ` + body + ` }`, ""},
		{"alias removed", `namespace a.b { ` + body + ` }`, "a.N.Util.Run"},
		{"ancestor alias added", `namespace a.b { using N = other; namespace c { ` + body + ` } }`, ""},
	}
	for _, scoped := range []bool{true, false} {
		s, idx, root := indexCSharpFiles(t, map[string]string{"Defs.cs": csharpAliasPrefixDefs, "App.cs": steps[0].app})
		repo, err := s.UpsertRepo(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			if err := os.WriteFile(filepath.Join(root, "App.cs"), []byte(step.app), 0o644); err != nil {
				t.Fatal(err)
			}
			opts := Options{RepoRoot: root, ScanKind: "update"}
			if scoped {
				opts.Paths = []string{"App.cs"}
			}
			if _, err := idx.Update(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			if got := csharpTarget(t, s, root, "App.cs", "N.Util.Run"); got != step.want {
				t.Fatalf("scoped=%v %s: target=%q, want %q", scoped, step.name, got, step.want)
			}
			if step.want == "" {
				assertCallers(t, s, repo.ID, "a.N.Util.Run")
				assertCallers(t, s, repo.ID, "other.Util.Run")
			} else {
				assertCallers(t, s, repo.ID, "a.N.Util.Run", "a.b.Caller.M")
			}
			assertCSharpFreshParity(t, s, root)
			before := csharpProjection(t, s, root)
			if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update"}); err != nil {
				t.Fatal(err)
			}
			after := csharpProjection(t, s, root)
			if len(before) != len(after) {
				t.Fatalf("%s: repeated update changed edges: %v -> %v", step.name, before, after)
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("%s: repeated update changed edges: %v -> %v", step.name, before, after)
				}
			}
		}
	}
}
