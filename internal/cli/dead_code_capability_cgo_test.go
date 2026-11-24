//go:build cgo

package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

func writeDeadCodeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, stderr, err := runCLI(t, "index", root, "--no-history"); err != nil {
		t.Fatalf("index: %v\n%s\n%s", err, out, stderr)
	}
	return root
}

// crossFileCalls holds, per production language that emits call edges, one
// call from one file to a function declared in another, in a form its
// resolver supports when it binds across files at all.
var crossFileCalls = map[string]map[string]string{
	"go":     {"go/a.go": "package p\n\nfunc XcalleeGo() int { return 1 }\n", "go/b.go": "package p\n\nfunc CallerGo() int { return XcalleeGo() }\n"},
	"python": {"py/__init__.py": "", "py/pkg/__init__.py": "", "py/pkg/a.py": "def xcallee_py():\n    return 1\n", "py/pkg/b.py": "from py.pkg.a import xcallee_py\n\ndef caller_py():\n    return xcallee_py()\n"},
	"java":   {"java/A.java": "package p;\npublic class A { public static int xcalleeJava() { return 1; } }\n", "java/B.java": "package p;\npublic class B { public int callerJava() { return A.xcalleeJava(); } }\n"},
	"kotlin": {"kt/A.kt": "package p\nfun xcalleeKt(): Int = 1\n", "kt/B.kt": "package p\nfun callerKt(): Int = xcalleeKt()\n"},
	"csharp": {"cs/A.cs": "namespace P { public static class A { public static int XcalleeCs() { return 1; } } }\n", "cs/B.cs": "namespace P { public class B { public int CallerCs() { return A.XcalleeCs(); } } }\n"},
	"typescript": {"ts/a.ts": "export function xcalleeTs(): number { return 1; }\n",
		"ts/b.ts": "import { xcalleeTs } from './a';\nexport function callerTs(): number { return xcalleeTs(); }\n"},
	"rust": {"rs/lib.rs": "mod a;\npub fn caller_rs() -> i32 { a::xcallee_rs() }\n", "rs/a.rs": "pub fn xcallee_rs() -> i32 { 1 }\n"},
	"php":  {"php/a.php": "<?php\nfunction xcallee_php() { return 1; }\n", "php/b.php": "<?php\nrequire 'a.php';\nfunction caller_php() { return xcallee_php(); }\n"},
	"cpp":  {"cpp/a.h": "inline int xcallee_cpp() { return 1; }\n", "cpp/b.cpp": "#include \"a.h\"\nint caller_cpp() { return xcallee_cpp(); }\n"},
	"ruby": {"rb/service.rb": "module App\n  class Service\n    def self.xcallee_rb; end\n  end\nend\n",
		"rb/caller.rb": "module App\n  class Caller\n    class << self\n      def f\n        Service.xcallee_rb()\n      end\n    end\n  end\nend\n"},
	"dart":  {"dart/a.dart": "int xcalleeDart() => 1;\n", "dart/b.dart": "import 'a.dart';\nint callerDart() => xcalleeDart();\n"},
	"lua":   {"lua/a.lua": "local M = {}\nfunction M.xcallee_lua() return 1 end\nreturn M\n", "lua/b.lua": "local a = require(\"a\")\nfunction caller_lua() return a.xcallee_lua() end\n"},
	"scala": {"scala/A.scala": "package p\nobject A { def xcalleeScala(): Int = 1 }\n", "scala/B.scala": "package p\nobject B { def callerScala(): Int = A.xcalleeScala() }\n"},
	"swift": {"swift/A.swift": "struct SU {\n    static func xcalleeSwift() -> Int { return 1 }\n}\nfunc xcalleeSwiftFree() -> Int { return 1 }\n",
		"swift/B.swift": "func callerSwift() -> Int { return SU.xcalleeSwift() + xcalleeSwiftFree() }\n"},
}

var crossFileCallees = map[string][]string{
	"go": {"XcalleeGo"}, "python": {"xcallee_py"}, "java": {"xcalleeJava"}, "kotlin": {"xcalleeKt"}, "csharp": {"XcalleeCs"},
	"typescript": {"xcalleeTs"}, "rust": {"xcallee_rs"}, "php": {"xcallee_php"}, "cpp": {"xcallee_cpp"}, "ruby": {"xcallee_rb"},
	"dart": {"xcalleeDart"}, "lua": {"xcallee_lua"}, "scala": {"xcalleeScala"}, "swift": {"xcalleeSwift", "xcalleeSwiftFree"},
}

// SameFileCallsOnly is a claim about each language's resolver. This ties it
// to what the production resolver does: a language binds a cross-file call
// exactly when its profile is not same-file-only. A new call-capable language
// fails here until it is classified.
func TestSameFileCallsOnlyMatchesProductionResolvers(t *testing.T) {
	files := map[string]string{}
	for _, f := range crossFileCalls {
		for name, content := range f {
			files[name] = content
		}
	}
	root := writeDeadCodeFixture(t, files)
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, _, repoID, err := openExistingApp(ctx, cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	for language, profile := range newDefaultRegistry().LanguageProfiles() {
		if !profile.EmitsCallEdges {
			continue
		}
		if language == "hcl" {
			// Terraform references, no calls; its block and attribute kinds are
			// never dead-code candidates.
			continue
		}
		callees, ok := crossFileCallees[language]
		if !ok {
			t.Errorf("%s emits call edges but has no cross-file fixture here", language)
			continue
		}
		wantBound := !parser.SameFileCallsOnly(profile.ID)
		for _, callee := range callees {
			result, err := app.Query.FindCallersResult(ctx, repoID, callee, 0, 10, 0)
			if err != nil {
				t.Fatalf("%s %s: %v", language, callee, err)
			}
			if !result.TargetFound {
				t.Fatalf("%s %s: target not indexed", language, callee)
			}
			if bound := len(result.Callers) > 0; bound != wantBound {
				t.Errorf("%s (%s) %s: cross-file caller bound = %v, SameFileCallsOnly = %v", language, profile.ID, callee, bound, !wantBound)
			}
		}
	}
}

// Dart, Lua, Scala and Swift record only calls the calling file proves, so with
// cgo they are call-capable yet a function of theirs called from another file
// has no caller. find_dead_code skips them, as it skips Svelte and Solidity.
func TestFindDeadCodeSkipsSameFileCallGraphs(t *testing.T) {
	files := map[string]string{"lib/lib.go": "package lib\n\nfunc unusedGo() int { return 0 }\n"}
	for _, language := range []string{"dart", "lua", "scala", "swift"} {
		for name, content := range crossFileCalls[language] {
			files[name] = content
		}
	}
	answer, _ := serveDeadCode(t, writeDeadCodeFixture(t, files))
	if len(answer.Data.DeadCode) != 1 || answer.Data.DeadCode[0].Name != "unusedGo" {
		t.Fatalf("dead_code = %+v, want only unusedGo", answer.Data.DeadCode)
	}
	want := []store.DeadCodeSkippedLanguage{
		{Language: "dart", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
		{Language: "lua", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
		{Language: "scala", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
		{Language: "swift", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
	}
	if !reflect.DeepEqual(answer.Data.Skipped, want) {
		t.Fatalf("skipped_languages = %+v, want %+v", answer.Data.Skipped, want)
	}
}

// SvelteKit components import library code through the `$lib` alias, with or
// without the `.js` the ESM convention adds (Svelte 5 rune modules are
// `.svelte.js`), and through barrel files that re-export it, nested or not. Neither leaves a reference, so the
// import is the only trace of the use, and it must withhold the TS symbols.
func TestFindDeadCodeWithholdsSvelteKitAliasAndBarrelImports(t *testing.T) {
	answer, _ := serveDeadCode(t, writeDeadCodeFixture(t, map[string]string{
		"tsconfig.json":             `{"compilerOptions":{"paths":{"$lib/*":["./src/lib/*"]}}}`,
		"src/lib/fmt.ts":            "export function fmtDate(d: Date): string { return d.toISOString(); }\n",
		"src/lib/Chart.ts":          "export default class Chart {}\n",
		"src/lib/barrel/index.ts":   "export { fmtBarrel } from './fmt';\nexport * from './more';\n",
		"src/lib/barrel/fmt.ts":     "export function fmtBarrel(): string { return ''; }\n",
		"src/lib/barrel/more.ts":    "export function moreStar(): number { return 1; }\n",
		"src/lib/unused.ts":         "export function unusedTs(): number { return 1; }\n",
		"src/lib/jsext.ts":          "export function aJsExt(): number { return 1; }\n",
		"src/lib/counter.svelte.ts": "export function counterRune(): number { return 1; }\n",
		"src/lib/nest/index.ts":     "export * from './sub';\n",
		"src/lib/nest/sub/index.ts": "export { nestedLeaf } from './leaf';\n",
		"src/lib/nest/sub/leaf.ts":  "export function nestedLeaf(): number { return 1; }\n",
		"src/routes/Ext.svelte":     "<script lang=\"ts\">\n  import { aJsExt } from \"$lib/jsext.js\";\n  import { counterRune } from \"$lib/counter.svelte.js\";\n  import { nestedLeaf } from \"$lib/nest\";\n</script>\n\n<p>{aJsExt()}{counterRune()}{nestedLeaf()}</p>\n",
		"src/routes/+page.svelte":   "<script lang=\"ts\">\n  import { fmtDate } from \"$lib/fmt\";\n  import Chart from \"$lib/Chart\";\n  const d = new Date();\n</script>\n\n<p>{fmtDate(d)}</p>\n<Chart/>\n",
		"src/routes/Barrel.svelte":  "<script lang=\"ts\">\n  import { fmtBarrel, moreStar } from \"../lib/barrel\";\n</script>\n\n<p>{fmtBarrel()}{moreStar()}</p>\n",
	}))
	var names []string
	for _, item := range answer.Data.DeadCode {
		names = append(names, item.Name)
	}
	if !reflect.DeepEqual(names, []string{"unusedTs"}) {
		t.Fatalf("dead_code = %v, want [unusedTs]", names)
	}
}
