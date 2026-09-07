//go:build cgo

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	ts "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

func TestCSharpScopeResolutionUsesNamespaceAndThisEvidence(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"Core.cs": `namespace App.Core;
public class Service { public static void Run() {} public void InstanceRun() {} private static void Secret() {} }`,
		"Other.cs": `namespace App.Other;
public class Service { public static void Run() {} }`,
		"Caller.cs": `using App.Core;
namespace App.Caller;
public class Caller {
 public void InstanceRun() {}
 public void F(Service service) {
  Service.Run();
  this.InstanceRun();
  service.InstanceRun();
 }
}`,
		"Alias.cs": `using A = App.Core.Service;
namespace App.Caller;
class AliasCaller { void F() { A.Run(); } }`,
		"Static.cs": `using static App.Core.Service;
namespace App.Caller;
class StaticCaller { void F() { Run(); } }`,
		"Ambiguous.cs": `using App.Core;
using App.Other;
namespace App.Caller;
class Ambiguous { void F() { Service.Run(); } }`,
		"Shadow.cs": `using App.Core;
namespace App.Caller;
class Shadow { void F(Service Service) { Service.Run(); } }`,
		"Unknown.cs": `namespace App.Caller;
class Unknown { void F() { Service.Run(); } }`,
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range files {
		p, err := ts.NewCSharp().Parse(context.Background(), path, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("parse %s symbols=%d edges=%v", path, len(p.Symbols), p.Edges)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	summary, err := New(s, parser.NewRegistry(ts.NewCSharp()), nil).Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary=%+v", summary)
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ExportEdgesPage(context.Background(), repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.ExportEdge{}
	for _, e := range edges {
		t.Logf("edge=%q resolved=%v strategy=%q", e.DstName, e.DstSymbolID != nil, e.ResolutionStrategy)
		got[e.FilePath+":"+e.DstName] = e
	}
	assert := func(file, name, strategy string, resolved bool) {
		e := got[file+":"+name]
		if (e.DstSymbolID != nil) != resolved || resolved && e.ResolutionStrategy != strategy {
			t.Fatalf("%s:%s = %#v", file, name, e)
		}
	}
	assert("Caller.cs", "Service.Run", "csharp_type_scope", true)
	assert("Caller.cs", "this.InstanceRun", "csharp_this_scope", true)
	assert("Caller.cs", "service.InstanceRun", "", false)
	assert("Alias.cs", "A.Run", "csharp_alias_scope", true)
	assert("Static.cs", "Run", "csharp_static_using", true)
	assert("Ambiguous.cs", "Service.Run", "", false)
	assert("Shadow.cs", "Service.Run", "", false)
	assert("Unknown.cs", "Service.Run", "", false)
	if got["Caller.cs:Service.Run"].DstQualifiedName != "App.Core.Service.Run" {
		t.Fatalf("Service.Run target = %#v", got["Caller.cs:Service.Run"])
	}
}

func TestCSharpScopeFreshIncrementalParity(t *testing.T) {
	initial := map[string]string{
		"Core.cs":   `namespace App.Core; public class Service { public static void Run() {} }`,
		"Caller.cs": `using App.Core; namespace App.Caller; class Caller { void F() { Service.Run(); } }`,
	}
	final := map[string]string{
		"Core.cs":   `namespace App.Renamed; public class Service { public static void Run() {} }`,
		"Caller.cs": `using App.Renamed; namespace App.Caller; class Caller { void F() { Service.Run(); } }`,
	}
	build := func(t *testing.T, root string, files map[string]string) *store.Store {
		for path, content := range files {
			if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(s, parser.NewRegistry(ts.NewCSharp()), nil).Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	incrementalRoot := t.TempDir()
	s := build(t, incrementalRoot, initial)
	idx := New(s, parser.NewRegistry(ts.NewCSharp()), nil)
	if err := os.WriteFile(filepath.Join(incrementalRoot, "Core.cs"), []byte(final["Core.cs"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incrementalRoot, "Caller.cs"), []byte(final["Caller.cs"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background(), Options{RepoRoot: incrementalRoot, ScanKind: "update", Paths: []string{"Core.cs", "Caller.cs"}}); err != nil {
		t.Fatal(err)
	}
	incremental := csharpProjection(t, s, incrementalRoot)
	_ = s.Close()
	freshRoot := t.TempDir()
	fresh := build(t, freshRoot, final)
	defer fresh.Close()
	freshProjection := csharpProjection(t, fresh, freshRoot)
	if strings.Join(incremental, "\n") != strings.Join(freshProjection, "\n") {
		t.Fatalf("fresh/incremental mismatch\nfresh=%v\nincremental=%v", freshProjection, incremental)
	}
}

func TestCSharpScopeF1IncrementalTransitions(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("A.cs", `namespace A; public static class Helpers { public static void Run() {} }`)
	write("B.cs", `namespace B; public static class Helpers { public static void Run() {} }`)
	write("Caller.cs", `using static A.Helpers; namespace X; class Caller { void F() { Run(); } }`)
	write("Local.cs", `namespace X;
class Local {
 void Run() {}
 void F() { Run(); }
}`)
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(ts.NewCSharp()), nil)
	if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	write("Caller.cs", `using static A.Helpers; using static B.Helpers; namespace X; class Caller { void F() { Run(); } }`)
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"Caller.cs"}}); err != nil {
		t.Fatal(err)
	}
	if csharpResolved(t, s, root, "Caller.cs", "Run") {
		t.Fatal("ambiguous static using remained resolved")
	}
	write("Caller.cs", `using static B.Helpers; namespace X; class Caller { void F() { Run(); } }`)
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"Caller.cs"}}); err != nil {
		t.Fatal(err)
	}
	if !csharpResolved(t, s, root, "Caller.cs", "Run") {
		t.Fatal("unique static using did not rebind")
	}

	write("Local.cs", `namespace X;
class Local { void Run() {} void F() {
 void Run() {}
 Run();
} }`)
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"Local.cs"}}); err != nil {
		t.Fatal(err)
	}
	if csharpResolved(t, s, root, "Local.cs", "Run") {
		t.Fatal("local-function shadow remained resolved")
	}
	write("Local.cs", `namespace X;
class Local {
 void Run() {}
 void F() { Run(); }
 void G() {}
}`)
	if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"Local.cs"}}); err != nil {
		t.Fatal(err)
	}
	if !csharpResolved(t, s, root, "Local.cs", "Run") {
		t.Fatal("local-function removal did not rebind")
	}
}

func csharpResolved(t *testing.T, s *store.Store, root, file, name string) bool {
	t.Helper()
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ExportEdgesPage(context.Background(), repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(edges))
	for _, edge := range edges {
		keys = append(keys, edge.FilePath+":"+edge.DstName)
		if edge.FilePath == file && edge.DstName == name {
			return edge.DstSymbolID != nil
		}
	}
	t.Fatalf("missing %s:%s edge; got %v", file, name, keys)
	return false
}

func TestCSharpScopeF1FailsClosed(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"A.cs":         `namespace A; public static class Helpers { public static void Run() {} }`,
		"B.cs":         `namespace B; public static class Helpers { public static void Run() {} } public static class Service { public static void Run() {} }`,
		"Unrelated.cs": `namespace Totally.Unrelated; public class Core { public class Service { public static void Run() {} } }`,
		"Hidden.cs":    `namespace App; class Outer { private class Hidden { public static void Run() {} } }`,
		"Caller.cs": `using static A.Helpers;
using static B.Helpers;
namespace X;
class Caller { void F() { Run(); A.Helpers.Run(); Core.Service.Run(); } }`,
		"CallerReversed.cs": `using static B.Helpers;
using static A.Helpers;
namespace X;
class CallerReversed { void F() { Run(); } }`,
		"Overload.cs": `namespace App;
class Overload {
 void Run(int x) {}
 void Run(string x) {}
 void F() { this.Run(1); }
}`,
		"Local.cs": `namespace App;
class Local {
 void Run() {}
 void F() {
  void Run() {}
  Run();
 }
}`,
		"HiddenCaller.cs": `namespace App; class HiddenCaller { void F() { Hidden.Run(); } }`,
		"Scopes.cs": `namespace A { using B; class CallerA { void F() { Service.Run(); } } }
namespace C { class CallerC { void F() { Service.Run(); } } }`,
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := New(s, parser.NewRegistry(ts.NewCSharp()), nil).Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ExportEdgesPage(context.Background(), repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range edges {
		seen[e.FilePath+":"+e.DstName] = true
		switch e.FilePath + ":" + e.DstName {
		case "Caller.cs:Run", "Caller.cs:Core.Service.Run", "CallerReversed.cs:Run", "Overload.cs:this.Run", "Local.cs:Run", "HiddenCaller.cs:Hidden.Run":
			if e.DstSymbolID != nil {
				t.Errorf("%s resolved to %q", e.FilePath+":"+e.DstName, e.DstQualifiedName)
			}
		}
		if e.FilePath == "Scopes.cs" && e.DstName == "Service.Run" {
			want := strings.HasPrefix(e.SrcQualifiedName, "A.")
			if (e.DstSymbolID != nil) != want {
				t.Errorf("namespace-scoped using source=%q resolved=%v", e.SrcQualifiedName, e.DstSymbolID != nil)
			}
		}
	}
	for _, key := range []string{"Caller.cs:Run", "Caller.cs:Core.Service.Run", "CallerReversed.cs:Run", "Overload.cs:this.Run", "Local.cs:Run", "HiddenCaller.cs:Hidden.Run"} {
		if !seen[key] {
			t.Errorf("missing expected call edge %s", key)
		}
	}
}

func BenchmarkScopeCSharpScale(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 500; i++ {
		content := fmt.Sprintf(`namespace N%d;
public static class Service { public static void Run() {} public static void Create() {} }
public class Caller { public void F() { Service.Run(); Service.Create(); } }`, i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("File%03d.cs", i)), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("fresh", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s, err := store.Open(filepath.Join(b.TempDir(), "graph.db"))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := New(s, parser.NewRegistry(ts.NewCSharp()), nil).Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
				b.Fatal(err)
			}
			s.Close()
		}
	})

	b.Run("incremental_one_file", func(b *testing.B) {
		s, err := store.Open(filepath.Join(b.TempDir(), "graph.db"))
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		idx := New(s, parser.NewRegistry(ts.NewCSharp()), nil)
		if _, err := idx.Index(context.Background(), Options{RepoRoot: root, ScanKind: "index"}); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			content := fmt.Sprintf("namespace N%d; public static class Service { public static void Run() {} public static void Create() {} } public class Caller { public void F() { Service.Run(); Service.Create(); } public void G() {} }", i)
			if err := os.WriteFile(filepath.Join(root, "File000.cs"), []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
			if _, err := idx.Update(context.Background(), Options{RepoRoot: root, ScanKind: "update", Paths: []string{"File000.cs"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func csharpProjection(t *testing.T, s *store.Store, root string) []string {
	t.Helper()
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ExportEdgesPage(context.Background(), repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, len(edges))
	for _, e := range edges {
		rows = append(rows, e.FilePath+"|"+e.DstName+"|"+e.DstQualifiedName+"|"+e.ResolutionStrategy+"|"+e.ResolutionConfidence)
	}
	sort.Strings(rows)
	return rows
}
