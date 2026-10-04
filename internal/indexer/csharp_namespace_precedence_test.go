//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	ts "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

const csharpPrecedenceDefs = `namespace a { public static class Util { public static void Run() {} } }
namespace staticns { public static class Util { public static void Run() {} } }
namespace other { public static class Util { public static void Run() {} } }`

func indexCSharpFiles(t *testing.T, files map[string]string) (*store.Store, *Indexer, string) {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	idx := New(s, parser.NewRegistry(ts.NewCSharp()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	return s, idx, root
}

// An import owned by a namespace is considered before the enclosing namespace
// (C# 7.7.1), but evidence holds only the owner's canonical name, not the
// declaration enclosing the call, and a file-scoped directive has an empty
// owner. Where the answer depends on that unproven scope the edge stays
// unresolved; where it does not, the spec order decides.
func TestCSharpNamespaceLevelLookupOrder(t *testing.T) {
	defs := csharpPrecedenceDefs
	call := `class Caller { void M() { Util.Run(); } }`
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"inner using vs outer type is unproven scope", map[string]string{"Defs.cs": defs, "App.cs": `namespace a.b { using staticns; public class Caller { public void M() { Util.Run(); } } }`}, ""},
		{"inner using alone binds", map[string]string{"Defs.cs": `namespace staticns { public static class Util { public static void Run() {} } }`, "App.cs": `namespace a.b { using staticns; ` + call + ` }`}, "staticns.Util.Run"},
		{"inner alias alone binds", map[string]string{"Defs.cs": defs, "App.cs": `namespace x { using Util = staticns.Util; ` + call + ` }`}, "staticns.Util.Run"},
		{"inner alias vs outer type is unproven scope", map[string]string{"Defs.cs": defs, "App.cs": `namespace a.b { using Util = staticns.Util; ` + call + ` }`}, ""},
		{"own-level type beats inner using", map[string]string{"Defs.cs": defs + `namespace a.b { public static class Util { public static void Run() {} } }`, "App.cs": `namespace a.b { using staticns; ` + call + ` }`}, "a.b.Util.Run"},
		{"own-level type and alias are ambiguous", map[string]string{"Defs.cs": defs + `namespace a.b { public static class Util { public static void Run() {} } }`, "App.cs": `namespace a.b { using Util = staticns.Util; ` + call + ` }`}, ""},
		{"two inner imports compete", map[string]string{"Defs.cs": defs, "App.cs": `namespace q { using staticns; using other; ` + call + ` }`}, ""},
		{"root alias loses to inner own-level type", map[string]string{"Defs.cs": defs + `namespace a.b { public static class Util { public static void Run() {} } }`, "App.cs": `using Util = staticns.Util; namespace a.b { ` + call + ` }`}, "a.b.Util.Run"},
		{"root using vs outer type is unproven scope", map[string]string{"Defs.cs": defs, "App.cs": `using staticns; namespace a.b { ` + call + ` }`}, ""},
		{"root using alone binds", map[string]string{"Defs.cs": defs, "App.cs": `using staticns; namespace q { ` + call + ` }`}, "staticns.Util.Run"},
		{"file-scoped using vs outer type is unproven scope", map[string]string{"Defs.cs": defs, "App.cs": "namespace a.b;\nusing staticns;\n" + call}, ""},
		{"outer type without import still binds", map[string]string{"Defs.cs": defs, "App.cs": `namespace a.b { ` + call + ` }`}, "a.Util.Run"},
		{"enclosing declaration using is not applied but own type wins", map[string]string{"Defs.cs": defs, "App.cs": `namespace a { using staticns; namespace b { ` + call + ` } }`}, "a.Util.Run"},
		{"explicit global qualification", map[string]string{"Defs.cs": defs, "App.cs": `namespace a.b { using staticns; class Caller { void M() { global::a.Util.Run(); } } }`}, "a.Util.Run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, root := indexCSharpFiles(t, tc.files)
			name := "Util.Run"
			if tc.name == "explicit global qualification" {
				name = "global::a.Util.Run"
			}
			if got := csharpTarget(t, s, root, "App.cs", name); got != tc.want {
				t.Fatalf("target=%q, want %q", got, tc.want)
			}
			assertCSharpFreshParity(t, s, root)
		})
	}
}

func TestCSharpNamespaceLevelLookupIncrementalPolicy(t *testing.T) {
	s, idx, root := indexCSharpFiles(t, map[string]string{
		"Defs.cs": csharpPrecedenceDefs,
		"App.cs":  `namespace a.b { using staticns; public class Caller { public void M() { Util.Run(); } } }`,
	})
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	update := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "App.cs"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"App.cs"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := csharpTarget(t, s, root, "App.cs", "Util.Run"); got != "" {
		t.Fatalf("unproven scope bound %q", got)
	}
	assertCallers(t, s, repo.ID, "a.Util.Run")
	assertCallers(t, s, repo.ID, "staticns.Util.Run")

	update(`namespace a.b { public class Caller { public void M() { Util.Run(); } } }`)
	if got := csharpTarget(t, s, root, "App.cs", "Util.Run"); got != "a.Util.Run" {
		t.Fatalf("after dropping the using target=%q, want a.Util.Run", got)
	}
	assertCallers(t, s, repo.ID, "a.Util.Run", "a.b.Caller.M")
	assertCSharpFreshParity(t, s, root)

	update(`namespace a.b { using staticns; public class Caller { public void M() { Util.Run(); } } }`)
	if got := csharpTarget(t, s, root, "App.cs", "Util.Run"); got != "" {
		t.Fatalf("after adding the using, stale target %q remains", got)
	}
	assertCallers(t, s, repo.ID, "a.Util.Run")
	assertCSharpFreshParity(t, s, root)

	before := csharpProjection(t, s, root)
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	}
	after := csharpProjection(t, s, root)
	if len(before) != len(after) {
		t.Fatalf("repeated update changed edges: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("repeated update changed edges: %v -> %v", before, after)
		}
	}
}
