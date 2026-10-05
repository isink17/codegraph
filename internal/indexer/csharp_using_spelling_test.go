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

// csharpUsingSpellingTree names namespaces that begin with "static" and
// spells `static` with a tab or a comment. Oracle: the C# specification
// (using_namespace_directive, using_static_directive, using_alias_directive,
// §14.5; simple-name lookup §12.8.4). No C# compiler (dotnet/csc/mcs) was
// available, so none of these targets were compiled.
//
//	Prefix.M      Util.Run()  staticns.Util.Run  `using staticns;` imports staticns only
//	GlobalName.M  Helper.Go() globalns.Helper.Go `using globalns;` imports globalns only
//	Plain.M       Util.Run()  ns.Util.Run        control: `using ns;`
//	Tab.M         Run()       ns.Util.Run        `using static<TAB>ns.Util;`
//	Comment.M     Run()       ns.Util.Run        `using /*c*/ static /*d*/ ns.Util;`
//	Alias.M       U.Run()     staticns.Util.Run  `using U = staticns.Util;`
//	Ambig.M       Util.Run()  none               two imported namespaces declare Util (CS0104)
//	Bad.M         Util.Run()  none               `using staticns.;` is a syntax error: imports nothing
//	app.Nested.N.M Util.Run() staticns.Util.Run  a using inside a namespace body is owned by it
//	app.Other.O.M  Util.Run() none               ...and does not reach a sibling namespace
//
// ns.Util and ns.Helper exist so that dropping "static"/"global" from a
// namespace name finds a real, wrong type.
var csharpUsingSpellingTree = map[string]string{
	"A.cs":          "namespace ns { public static class Util { public static void Run() {} } public static class Helper { public static void Go() {} } }\n",
	"B.cs":          "namespace staticns { public static class Util { public static void Run() {} } }\n",
	"C.cs":          "namespace globalns { public static class Helper { public static void Go() {} } }\n",
	"Prefix.cs":     "using staticns;\nnamespace app { public class Prefix { public void M() { Util.Run(); } } }\n",
	"GlobalName.cs": "using globalns;\nnamespace app { public class GlobalName { public void M() { Helper.Go(); } } }\n",
	"Plain.cs":      "using ns;\nnamespace app { public class Plain { public void M() { Util.Run(); } } }\n",
	"Tab.cs":        "using static\tns.Util;\nnamespace app { public class Tab { public void M() { Run(); } } }\n",
	"Comment.cs":    "using /*c*/ static /*d*/ ns.Util;\nnamespace app { public class Comment { public void M() { Run(); } } }\n",
	"Alias.cs":      "using U = staticns.Util;\nnamespace app { public class Alias { public void M() { U.Run(); } } }\n",
	"Ambig.cs":      "using ns;\nusing staticns;\nnamespace app { public class Ambig { public void M() { Util.Run(); } } }\n",
	"Bad.cs":        "using staticns.;\nnamespace app { public class Bad { public void M() { Util.Run(); } } }\n",
	"Nested.cs":     "namespace app.Nested { using staticns; public class N { public void M() { Util.Run(); } } }\nnamespace app.Other { public class O { public void M() { Util.Run(); } } }\n",
}

var csharpUsingSpellingWant = map[string]string{
	"app.Prefix.M|Util.Run":      "staticns.Util.Run",
	"app.GlobalName.M|Helper.Go": "globalns.Helper.Go",
	"app.Plain.M|Util.Run":       "ns.Util.Run",
	"app.Tab.M|Run":              "ns.Util.Run",
	"app.Comment.M|Run":          "ns.Util.Run",
	"app.Alias.M|U.Run":          "staticns.Util.Run",
	"app.Ambig.M|Util.Run":       "",
	"app.Bad.M|Util.Run":         "",
	"app.Nested.N.M|Util.Run":    "staticns.Util.Run",
	"app.Other.O.M|Util.Run":     "",
}

func csharpUsingTargets(t *testing.T, s *store.Store, root string) map[string]string {
	t.Helper()
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ExportEdgesPage(context.Background(), repo.ID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range edges {
		if e.Kind == "calls" {
			out[e.SrcQualifiedName+"|"+e.DstName] = e.DstQualifiedName
		}
	}
	return out
}

func assertCSharpUsingTargets(t *testing.T, s *store.Store, root, step string) {
	t.Helper()
	got := csharpUsingTargets(t, s, root)
	for key, want := range csharpUsingSpellingWant {
		if g, ok := got[key]; !ok || g != want {
			t.Errorf("%s: %s bound %q (present %v), want %q", step, key, g, ok, want)
		}
	}
}

func writeCSharpUsingTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range csharpUsingSpellingTree {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCSharpUsingSpellingBindsDeclaredNamespace(t *testing.T) {
	root := writeCSharpUsingTree(t)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewCSharp()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	assertCSharpUsingTargets(t, s.Store, root, "fresh")
	// Removing staticns.Util leaves the import naming nothing that declares
	// Util; ns.Util must not answer for it.
	if err := os.Remove(filepath.Join(root, "B.cs")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got := csharpUsingTargets(t, s.Store, root)["app.Prefix.M|Util.Run"]; got != "" {
		t.Fatalf("Prefix.M bound %q without staticns.Util, want unresolved", got)
	}
	assertCSharpFreshParity(t, s.Store, root)
}

// A graph written by the v4 C# parser recorded `using staticns;` as ns and
// missed `using static<TAB>`. Updating it with the current parser re-parses
// the C# files and must land exactly where a fresh index does, and a second
// update does no work.
func TestCSharpUsingSpellingProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := writeCSharpUsingTree(t)
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewCSharpV4()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	if got := fileParserProfile(t, s.raw(t), repo, "Prefix.cs"); got != "treesitter:csharp:v4" {
		t.Fatalf("legacy profile = %q", got)
	}
	if got := csharpUsingTargets(t, s.Store, root)["app.Prefix.M|Util.Run"]; got != "ns.Util.Run" {
		t.Fatalf("legacy Prefix.M bound %q, want the v4 wrong target ns.Util.Run", got)
	}
	idx := New(s.Store, parser.NewRegistry(tsparser.NewCSharp()), nil)
	summary, err := idx.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ParserProfileLanguages, ",") != "csharp" {
		t.Fatalf("update = %+v, want a csharp profile reparse", summary)
	}
	if got := fileParserProfile(t, s.raw(t), repo, "Prefix.cs"); got != "treesitter:csharp:v6" {
		t.Fatalf("updated profile = %q", got)
	}
	assertCSharpUsingTargets(t, s.Store, root, "update")
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
