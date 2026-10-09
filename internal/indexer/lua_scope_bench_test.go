//go:build cgo

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// luaBenchFile is a deterministic Lua file of funcs local functions: each
// calls the next three times on one line, calls a shadowed redeclaration and
// a reassigned local, so it mixes proven, shadowed and refused calls.
func luaBenchFile(funcs int, salt string) string {
	var b strings.Builder
	b.WriteString("local function r() end\nr = nil\n")
	for k := range funcs {
		fmt.Fprintf(&b, "local function f%d() end\n", k)
	}
	for k := range funcs {
		n := (k + 1) % funcs
		fmt.Fprintf(&b, "local function g%d() f%d() f%d() f%d() r() end\n", k, n, n, n)
	}
	b.WriteString("local function f0() end\nlocal function h() f0() end\n")
	fmt.Fprintf(&b, "return {h = h, salt = %q}\n", salt)
	return b.String()
}

func writeLuaBenchRepo(b *testing.B, root string, files, funcs int) {
	b.Helper()
	for i := range files {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("m%03d.lua", i)), []byte(luaBenchFile(funcs, "")), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

func openLuaBenchIndexer(b *testing.B, dbPath string) (*store.Store, *Indexer) {
	b.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	return s, New(s, parser.NewRegistry(tsparser.NewLua()), nil)
}

func BenchmarkLuaScopeIndexing(b *testing.B) {
	const files, funcs = 120, 30
	ctx := context.Background()
	root := b.TempDir()
	writeLuaBenchRepo(b, root, files, funcs)

	b.Run("full", func(b *testing.B) {
		dir := b.TempDir()
		for b.Loop() {
			b.StopTimer()
			dbPath := filepath.Join(dir, "g.sqlite")
			_ = os.Remove(dbPath)
			s, idx := openLuaBenchIndexer(b, dbPath)
			b.StartTimer()
			if _, err := idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = s.Close()
			b.StartTimer()
		}
	})

	s, idx := openLuaBenchIndexer(b, filepath.Join(b.TempDir(), "g.sqlite"))
	defer s.Close()
	if _, err := idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		b.Fatal(err)
	}
	b.Run("one-file", func(b *testing.B) {
		i := 0
		for b.Loop() {
			b.StopTimer()
			i++
			if err := os.WriteFile(filepath.Join(root, "m000.lua"), []byte(luaBenchFile(funcs, fmt.Sprint(i))), 0o644); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update", Paths: []string{"m000.lua"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
	// The debug toggle: a file that reaches the debug library withdraws every
	// binding, and removing it restores them.
	b.Run("debug-toggle", func(b *testing.B) {
		hook := filepath.Join(root, "hook.lua")
		on := false
		for b.Loop() {
			b.StopTimer()
			on = !on
			if on {
				if err := os.WriteFile(hook, []byte("debug.sethook(function() end, \"c\")\n"), 0o644); err != nil {
					b.Fatal(err)
				}
			} else if err := os.Remove(hook); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update", Paths: []string{"hook.lua"}}); err != nil {
				b.Fatal(err)
			}
		}
		if on {
			_ = os.Remove(hook)
			if _, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update", Paths: []string{"hook.lua"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("noop", func(b *testing.B) {
		for b.Loop() {
			if _, err := idx.Update(ctx, Options{RepoRoot: root, ScanKind: "update"}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
