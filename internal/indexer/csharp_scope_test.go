//go:build cgo

package indexer

import (
	"context"
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
