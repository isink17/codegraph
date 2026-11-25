//go:build cgo

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// rustBenchModule is module k of an n-module flat crate: calls fully
// qualified crate::mJ::f() spellings and, through an explicit `use` of the
// first segment, m(k+1)::f() and other::g().
func rustBenchModule(k, n, calls int, salt string) string {
	var b strings.Builder
	a, r := (k+1)%n, (k+2)%n
	fmt.Fprintf(&b, "use crate::m%d;\nuse crate::m%d as other;\n\npub fn f() {}\npub fn g() {}\n\npub struct S%d;\n\nimpl S%d {\n    pub fn new() -> Self { S%d }\n}\n\npub fn calls() {\n", a, r, k, k, k)
	for j := range calls {
		fmt.Fprintf(&b, "    crate::m%d::f();\n", (k*7+j*13+1)%n)
	}
	fmt.Fprintf(&b, "}\n\npub fn via_import() {\n    m%d::f();\n    other::g();\n    f();\n}\n%s", a, salt)
	return b.String()
}

func writeRustBenchCrate(b *testing.B, root string, modules, calls int) {
	b.Helper()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		b.Fatal(err)
	}
	var lib strings.Builder
	for k := range modules {
		fmt.Fprintf(&lib, "pub mod m%d;\n", k)
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("m%d.rs", k)), []byte(rustBenchModule(k, modules, calls, "")), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "lib.rs"), []byte(lib.String()), 0o644); err != nil {
		b.Fatal(err)
	}
}

func rustBenchSize(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 2 {
		return n
	}
	return def
}

// BenchmarkRustCrateUpdate measures update cost in one large Rust crate.
// CODEGRAPH_BENCH_RUST_MODULES (default 300) and CODEGRAPH_BENCH_RUST_CALLS
// (default 10 crate:: calls per module) size the crate.
func BenchmarkRustCrateUpdate(b *testing.B) {
	modules := rustBenchSize("CODEGRAPH_BENCH_RUST_MODULES", 300)
	calls := rustBenchSize("CODEGRAPH_BENCH_RUST_CALLS", 10)
	ctx := context.Background()
	root := b.TempDir()
	writeRustBenchCrate(b, root, modules, calls)
	open := func(b *testing.B, dbPath string) (*store.Store, *Indexer) {
		b.Helper()
		s, err := store.Open(dbPath)
		if err != nil {
			b.Fatal(err)
		}
		return s, New(s, parser.NewRegistry(tsparser.NewRust()), nil)
	}
	b.Logf("modules=%d crate_calls_per_module=%d", modules, calls)

	b.Run("full", func(b *testing.B) {
		dir := b.TempDir()
		for b.Loop() {
			b.StopTimer()
			dbPath := filepath.Join(dir, "g.sqlite")
			_ = os.Remove(dbPath)
			s, idx := open(b, dbPath)
			b.StartTimer()
			if _, err := idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = s.Close()
			b.StartTimer()
		}
	})

	s, idx := open(b, filepath.Join(b.TempDir(), "g.sqlite"))
	defer s.Close()
	// Only Rust carries a policy marker, so policy-upgrade can bump it alone.
	version := 1000
	s.SetResolverPolicies(map[string]int{"rust": version})
	if _, err := idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		b.Fatal(err)
	}
	const target = "src/m1.rs"
	original := rustBenchModule(1, modules, calls, "")
	// The target's content and the edit counters outlive each sub-benchmark
	// run, so with -count > 1 every edit still differs from what is on disk.
	current, touches, added := original, 0, 0
	edit := func(b *testing.B, content string) {
		b.Helper()
		b.StopTimer()
		current = content
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(target)), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		summary, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update", Paths: []string{target}})
		if err != nil {
			b.Fatal(err)
		}
		if summary.FilesChanged != 1 {
			b.Fatalf("files changed = %d, want 1", summary.FilesChanged)
		}
	}

	b.Run("noop", func(b *testing.B) {
		for b.Loop() {
			if _, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update"}); err != nil {
				b.Fatal(err)
			}
		}
	})
	// A comment-only edit: the file is reparsed, nothing it declares or
	// calls changes.
	b.Run("one-file-touch", func(b *testing.B) {
		for b.Loop() {
			touches++
			edit(b, original+fmt.Sprintf("// %d\n", touches))
		}
	})
	b.Run("one-file-decl-added", func(b *testing.B) {
		for b.Loop() {
			added++
			edit(b, original+fmt.Sprintf("pub fn added%d() {}\n", added))
		}
	})
	// Alternates the import that binds m2::f() between crate::m2 and an
	// alias of crate::m7, so the file's qualified call changes target.
	b.Run("one-file-import-rebind", func(b *testing.B) {
		rebound := strings.Replace(original, "use crate::m2;", "use crate::m7 as m2;", 1)
		for b.Loop() {
			if current == rebound {
				edit(b, original)
			} else {
				edit(b, rebound)
			}
		}
	})
	if current != original {
		edit(b, original)
	}
	// A resolver policy bump re-decides every Rust edge of the unchanged crate.
	b.Run("policy-upgrade", func(b *testing.B) {
		for b.Loop() {
			version++
			s.SetResolverPolicies(map[string]int{"rust": version})
			summary, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update"})
			if err != nil {
				b.Fatal(err)
			}
			if !slices.Equal(summary.ResolverPolicyLanguages, []string{"rust"}) {
				b.Fatalf("re-decided %v, want [rust]", summary.ResolverPolicyLanguages)
			}
		}
	})
}
