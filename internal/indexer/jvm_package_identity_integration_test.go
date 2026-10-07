//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// Each caller names package docs only in a comment and really lives in
// com.real, which holds no Helper. The docs package does, so reading the
// comment as the package fabricates same-package bindings both ways: from the
// caller to Helper, and from the docs users to the caller.
var commentPackageTree = tree{
	"com/real/Caller.java": "/* Moved from package docs; */\npackage com.real;\nclass Caller {\n    static void run() { Helper.go(); }\n}\n",
	"docs/Helper.java":     "package docs;\npublic class Helper {\n    public static void go() {}\n}\n",
	"com/real/Caller.kt":   "/*\npackage docs\n*/\npackage com.real\nfun caller() { helper() }\n",
	"docs/Helper.kt":       "package docs\nfun helper() {}\n",
	"com/real/Other.kt":    "package com.real\nfun other() {}\n",
	"docs/User.java":       "package docs;\nclass User {\n    void use() { Caller.run(); }\n}\n",
	"docs/User.kt":         "package docs\nfun user() { caller() }\n",
	"main.go":              "package main\nfunc main() {}\n",
}

func commentPackageSymbols(t *testing.T, r *lifecycleRepo) string {
	t.Helper()
	return swallowRows(t, r, `SELECT s.qualified_name||'|'||s.stable_key FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path LIKE 'com/real/Caller.%' ORDER BY f.path, s.start_line`)
}

func assertCommentPackageUnbound(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	if got := commentPackageSymbols(t, r); got != "com.real.Caller|type:java:com.real:Caller,com.real.Caller.run|func:java:com.real:Caller:run,com.real.caller|func:kotlin:com.real.caller" {
		t.Fatalf("caller symbols = %q", got)
	}
	for caller, target := range map[string]string{"com.real.Caller.run": "docs.Helper.go", "com.real.caller": "docs.helper", "docs.User.use": "com.real.Caller.run", "docs.user": "com.real.caller"} {
		if got, _ := jvmNameCall(t, r, caller); got != "" {
			t.Fatalf("%s -> %q, want unresolved", caller, got)
		}
		assertJVMNoQueryRelation(t, r, caller, target)
	}
}

// A package named only in a comment binds nothing: the caller keeps its real
// package, and the same-name declaration in the comment's package stays
// unreached by the edge, its reference, FindCallers and FindCallees.
func TestJVMCommentPackageDoesNotBindSamePackage(t *testing.T) {
	r := newLifecycleRepo(t, commentPackageTree)
	assertCommentPackageUnbound(t, r)
	// The real package still binds once it declares the name.
	r.write(t, "com/real/Helper.java", "package com.real;\npublic class Helper {\n    public static void go() {}\n}\n")
	r.write(t, "com/real/Helper.kt", "package com.real\nfun helper() {}\n")
	r.update(t)
	for caller, target := range map[string]string{"com.real.Caller.run": "com.real.Helper.go", "com.real.caller": "com.real.helper"} {
		if got, _ := jvmNameCall(t, r, caller); !strings.HasPrefix(got, target+"|") {
			t.Fatalf("%s -> %q, want %s", caller, got, target)
		}
		assertJVMQueryRelation(t, r, caller, target)
	}
	r.assertFreshParity(t, "real package declares the name")
	r.remove(t, "com/real/Helper.java")
	r.remove(t, "com/real/Helper.kt")
	r.update(t)
	assertCommentPackageUnbound(t, r)
	r.assertFreshParity(t, "real package name removed")
}

// A database written by the raw-text package parsers (Java v2, Kotlin v9)
// holds the comment's package and the fabricated edges. Reconverging both
// languages reparses unchanged files, renames the symbols and drops the edges,
// agrees with a fresh index, and then settles.
func TestJVMRawTextPackageProfileConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for path, content := range commentPackageTree {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProfileFile(t, abs, content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJavaV2(), tsparser.NewKotlinV9(), goparser.New()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	for path, want := range map[string]string{"com/real/Caller.java": "treesitter:java:v2", "com/real/Caller.kt": "treesitter:kotlin:v9"} {
		if got := fileParserProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("legacy %s profile = %q, want %q", path, got, want)
		}
	}
	// The reproduction: the comment is the package and the edges bind.
	if got := commentPackageSymbols(t, r); got != "docs.Caller|type:java:docs:Caller,docs.Caller.run|func:java:docs:Caller:run,docs.caller|func:kotlin:docs.caller" {
		t.Fatalf("legacy caller symbols = %q", got)
	}
	for caller, target := range map[string]string{"docs.Caller.run": "docs.Helper.go", "docs.caller": "docs.helper", "docs.User.use": "docs.Caller.run", "docs.user": "docs.caller"} {
		if got, _ := jvmNameCall(t, r, caller); !strings.HasPrefix(got, target+"|") {
			t.Fatalf("legacy %s -> %q, want %s", caller, got, target)
		}
	}

	// A path-scoped update reconverges only its paths' language, Kotlin; a
	// full update then reconverges Java.
	for _, step := range []struct {
		paths    []string
		files    int
		language string
	}{{[]string{"com/real/Other.kt"}, 4, "kotlin"}, {nil, 3, "java"}} {
		summary, err := r.idx.Update(ctx, Options{RepoRoot: root, Paths: step.paths})
		if err != nil {
			t.Fatal(err)
		}
		if summary.FilesChanged != step.files || summary.FilesIndexed != step.files || strings.Join(summary.ParserProfileLanguages, ",") != step.language {
			t.Fatalf("%s reconvergence update = %+v", step.language, summary)
		}
	}
	for path, want := range map[string]string{"com/real/Caller.java": "treesitter:java:v12", "docs/Helper.java": "treesitter:java:v12", "com/real/Caller.kt": "treesitter:kotlin:v12", "docs/Helper.kt": "treesitter:kotlin:v12", "com/real/Other.kt": "treesitter:kotlin:v12", "docs/User.java": "treesitter:java:v12", "docs/User.kt": "treesitter:kotlin:v12", "main.go": "go-ast:go:v1"} {
		if got := fileParserProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("%s profile = %q, want %q", path, got, want)
		}
	}
	assertCommentPackageUnbound(t, r)
	r.assertFreshParity(t, "raw-text package profile convergence")
	again, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil || again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, %v; want no-op", again, err)
	}
}

func TestKotlinDuplicateHeaderProfileConvergence(t *testing.T) {
	r := newLifecycleRepo(t, tree{"A.kt": "package real\npackage fake\nfun run() {}\n"})
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		"UPDATE files SET parser_profile='treesitter:kotlin:v10'",
		"UPDATE file_scope_evidence SET package_name='real'",
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	summary := r.update(t)
	if summary.FilesIndexed != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("upgrade = %+v", summary)
	}
	var pkg string
	if err := db.QueryRow("SELECT package_name FROM file_scope_evidence").Scan(&pkg); err != nil || pkg != "" {
		t.Fatalf("package = %q, %v", pkg, err)
	}
	r.assertFreshParity(t, "duplicate Kotlin header profile transition")
	if again := r.update(t); again.FilesIndexed != 0 {
		t.Fatalf("second update = %+v", again)
	}
}
