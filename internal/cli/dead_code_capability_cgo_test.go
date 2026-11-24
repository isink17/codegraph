//go:build cgo

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// Dart, Lua and Scala record only calls the calling file proves, so with cgo
// they are call-capable yet a function of theirs called from another file has
// no caller. find_dead_code skips them, as it skips Svelte and Solidity.
func TestFindDeadCodeSkipsSameFileCallGraphs(t *testing.T) {
	for language, profile := range newDefaultRegistry().LanguageProfiles() {
		want := language == "dart" || language == "lua" || language == "scala"
		if got := parser.SameFileCallsOnly(profile.ID); got != want || !profile.EmitsCallEdges && got {
			t.Errorf("%s profile %s: SameFileCallsOnly = %v, want %v", language, profile.ID, got, want)
		}
	}

	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	for name, content := range map[string]string{
		"lib/lib.go":    "package lib\n\nfunc unusedGo() int { return 0 }\n",
		"dart/a.dart":   "int callee() => 1;\n",
		"dart/b.dart":   "import 'a.dart';\nint caller() => callee();\n",
		"lua/a.lua":     "local M = {}\nfunction M.callee() return 1 end\nreturn M\n",
		"lua/b.lua":     "local a = require(\"a\")\nfunction caller() return a.callee() end\n",
		"scala/A.scala": "package p\nobject A { def callee(): Int = 1 }\n",
		"scala/B.scala": "package p\nobject B { def caller(): Int = A.callee() }\n",
	} {
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
	answer, _ := serveDeadCode(t, root)
	if len(answer.Data.DeadCode) != 1 || answer.Data.DeadCode[0].Name != "unusedGo" {
		t.Fatalf("dead_code = %+v, want only unusedGo", answer.Data.DeadCode)
	}
	want := []store.DeadCodeSkippedLanguage{
		{Language: "dart", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
		{Language: "lua", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
		{Language: "scala", Capability: store.GraphCallCapable, Reason: store.DeadCodeSameFileCallsOnly},
	}
	if !reflect.DeepEqual(answer.Data.Skipped, want) {
		t.Fatalf("skipped_languages = %+v, want %+v", answer.Data.Skipped, want)
	}
}
