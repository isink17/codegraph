//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// Each annotated `private fun N() { ... }` below is followed by another
// declaration, which is what makes the grammar swallow it as an expression;
// the last one is followed by a detached-annotation split, recovered since Kotlin v7.
const swallowKotlin = "package lib\n" +
	"@Preview(showBackground = true)\n@Composable\nprivate fun hidden() {\n    target()\n}\n" +
	"fun visible(x: kotlin.Int) {}\n" +
	"@JvmName(\"execute\")\n@Composable\nprivate fun renamed() {}\n" +
	"@JvmSynthetic\n@Suppress(\"x\")\nprivate fun synth() {}\n" +
	"@JvmName(\"hexec\")\nfun hrun(x: kotlin.Int) {}\n" +
	"fun target() {}\n"

const swallowCaller = "package app;\nimport lib.ScreensKt;\nclass Caller {\n" +
	"    void visibleCall() { ScreensKt.visible(1); }\n" +
	"    void hiddenCall() { ScreensKt.hidden(); }\n" +
	"    void renamedCall() { ScreensKt.execute(); }\n" +
	"    void renamedSource() { ScreensKt.renamed(); }\n" +
	"    void synthCall() { ScreensKt.synth(); }\n" +
	"    void detachedCall() { ScreensKt.hexec(1); }\n}"

func swallowRows(t *testing.T, r *lifecycleRepo, query string) string {
	t.Helper()
	rows, err := r.raw(t).QueryContext(r.ctx, query, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

// swallowFacts is the Screens.kt state a swallowed layout and its clean parse
// must agree on: every symbol with its visibility, range and signature, every
// call edge with its source, and every call reference.
func swallowFacts(t *testing.T, r *lifecycleRepo) string {
	t.Helper()
	return swallowRows(t, r, `SELECT s.qualified_name||'|'||s.visibility||'|'||s.start_line||'-'||s.end_line||'|'||s.signature FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path='lib/Screens.kt' ORDER BY s.start_line`) +
		" edges " + swallowRows(t, r, `SELECT src.qualified_name||'->'||e.dst_name||'@'||e.line FROM edges e JOIN symbols src ON src.id=e.src_symbol_id JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path='lib/Screens.kt' ORDER BY e.line, e.dst_name`) +
		" refs " + swallowRows(t, r, `SELECT rt.qualified_name||'@'||rt.start_line FROM references_tbl rt JOIN files f ON f.id=rt.file_id WHERE rt.repo_id=? AND f.path='lib/Screens.kt' AND rt.ref_kind='call' ORDER BY rt.start_line, rt.qualified_name`)
}

// A recovered private function restores its file's facade, so the public
// sibling's Java caller binds, while the function itself stays private to
// Java. Its body call belongs to it, and its head is no call.
func TestKotlinSwallowedPrivateFunctionJavaInterop(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Caller.java": swallowCaller, "lib/Screens.kt": swallowKotlin})
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Screens.kt", "lib.ScreensKt|explicit=0|multifile=0")
	want := "lib.hidden|private|2-6|@Preview(showBackground = true)\n@Composable\nprivate fun hidden() {\n    target()\n},lib.visible|public|7-7|fun visible(x: kotlin.Int) {}," +
		"lib.renamed|private|8-10|@JvmName(\"execute\")\n@Composable\nprivate fun renamed() {},lib.synth|private|11-13|@JvmSynthetic\n@Suppress(\"x\")\nprivate fun synth() {}," +
		"lib.hrun|public|15-15|@JvmName(\"hexec\")\nfun hrun(x: kotlin.Int) {},lib.target|public|16-16|fun target() {}" +
		" edges lib.hidden->target@5 refs target@5"
	if got := swallowFacts(t, r); got != want {
		t.Fatalf("facts =\n%s\nwant\n%s", got, want)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.visibleCall"); got != "lib.visible|fun visible(x: kotlin.Int) {}" {
		t.Fatalf("visibleCall -> %q", got)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.detachedCall"); got != "lib.hrun|@JvmName(\"hexec\")\nfun hrun(x: kotlin.Int) {}" {
		t.Fatalf("detachedCall -> %q", got)
	}
	assertJVMQueryRelation(t, r, "app.Caller.visibleCall", "lib.visible")
	for _, caller := range []string{"hiddenCall", "renamedCall", "renamedSource", "synthCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Errorf("%s -> %q, want unresolved", caller, got)
		}
	}
	assertJVMNoQueryRelation(t, r, "app.Caller.hiddenCall", "lib.hidden")
	assertJVMQueryRelation(t, r, "lib.hidden", "lib.target")
	if got := jvmNameFacts(t, r); got != "lib.hrun=hexec,lib.renamed=execute" {
		t.Fatalf("name facts = %q", got)
	}
	r.assertFreshParity(t, "swallowed private function recovery")
}

// Laying the same declaration out so the grammar parses it cleanly -- a `;`
// after its body -- changes no fact, in either direction. A @JvmName added to
// and removed from the swallowed layout moves only its name evidence, and the
// private function stays hidden from Java throughout.
func TestKotlinSwallowedPrivateFunctionLifecycle(t *testing.T) {
	// first is the annotation before @Composable: a neutral one or a @JvmName.
	swallowed := func(first string) string {
		return "package lib\n" + first + "\n@Composable\nprivate fun run() {\n    target()\n}\nfun target() {}\n"
	}
	// The v8 parser proves each swallowed layout really is swallowed.
	for _, annotation := range []string{"@Preview", "@JvmName(\"execute\")"} {
		pf, err := tsparser.NewKotlinV8().Parse(context.Background(), "lib/Screens.kt", []byte(swallowed(annotation)))
		if err != nil || len(pf.Symbols) != 1 || pf.Symbols[0].Name != "target" {
			t.Fatalf("v8 parse of %q = %+v, %v; want only target", annotation, pf.Symbols, err)
		}
	}
	clean := strings.Replace(swallowed("@Preview"), "\n}\nfun target", "\n};\nfun target", 1)
	caller := "package app;\nimport lib.ScreensKt;\nclass Caller {\n    void runCall() { ScreensKt.run(); }\n    void executeCall() { ScreensKt.execute(); }\n    void targetCall() { ScreensKt.target(); }\n}"
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "lib/Screens.kt": clean})
	base := swallowFacts(t, r)
	if base != "lib.run|private|2-6|@Preview\n@Composable\nprivate fun run() {\n    target()\n},lib.target|public|7-7|fun target() {} edges lib.run->target@5 refs target@5" {
		t.Fatalf("clean facts = %q", base)
	}
	for i, step := range []struct{ content, facts, names string }{
		{swallowed("@Preview"), base, ""},
		{clean, base, ""},
		{swallowed("@JvmName(\"execute\")"), "", "lib.run=execute"},
		{swallowed("@JvmName(\"perform\")"), "", "lib.run=perform"},
		{swallowed("@Preview"), base, ""},
	} {
		r.write(t, "lib/Screens.kt", step.content)
		r.update(t, "lib/Screens.kt")
		if got := swallowFacts(t, r); step.facts != "" && got != step.facts {
			t.Fatalf("step %d facts = %q, want %q", i, got, step.facts)
		}
		if got := jvmNameFacts(t, r); got != step.names {
			t.Fatalf("step %d name facts = %q, want %q", i, got, step.names)
		}
		assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Screens.kt", "lib.ScreensKt|explicit=0|multifile=0")
		for _, c := range []string{"runCall", "executeCall"} {
			if got, _ := jvmNameCall(t, r, "app.Caller."+c); got != "" {
				t.Fatalf("step %d %s -> %q, want unresolved", i, c, got)
			}
		}
		if got, _ := jvmNameCall(t, r, "app.Caller.targetCall"); !strings.HasPrefix(got, "lib.target|") {
			t.Fatalf("step %d targetCall -> %q", i, got)
		}
		r.assertFreshParity(t, "swallowed layout lifecycle")
	}

	// Deleting the file removes the recovered function, its body edge, its
	// evidence and the facade; restoring it brings all of them back.
	r.write(t, "lib/Screens.kt", swallowed("@JvmName(\"execute\")"))
	r.update(t, "lib/Screens.kt")
	r.remove(t, "lib/Screens.kt")
	r.update(t, "lib/Screens.kt")
	if got := swallowFacts(t, r); got != " edges  refs " {
		t.Fatalf("deleted facts = %q", got)
	}
	if got := jvmNameFacts(t, r); got != "" {
		t.Fatalf("deleted name facts = %q", got)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.targetCall"); got != "" {
		t.Fatalf("deleted targetCall -> %q", got)
	}
	r.assertFreshParity(t, "swallowed file deleted")
	r.write(t, "lib/Screens.kt", swallowed("@JvmName(\"execute\")"))
	r.update(t, "lib/Screens.kt")
	if got := jvmNameFacts(t, r); got != "lib.run=execute" {
		t.Fatalf("restored name facts = %q", got)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.targetCall"); !strings.HasPrefix(got, "lib.target|") {
		t.Fatalf("restored targetCall -> %q", got)
	}
	assertJVMQueryRelation(t, r, "lib.run", "lib.target")
	r.assertFreshParity(t, "swallowed file restored")
}

// A v8 database, written by the v8 parser that left the swallowed function
// unrecovered, converges on a path-scoped update: stale Kotlin reparses to the
// current profile, the private function, its facade and its body edge appear,
// its head stops being a call, and the unchanged Java v2 caller of the public
// sibling binds without being reparsed.
func TestKotlinV8ToV9SwallowRecoveryConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string]string{
		"Caller.java":    swallowCaller,
		"lib/Screens.kt": swallowKotlin,
		"Other.kt":       "package lib\nfun other() {}\n",
		"main.go":        "package main\nfunc main() {}\n",
		"caller.ts":      "export function caller(): void {}\n",
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		writeProfileFile(t, filepath.Join(root, path), content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJava(), tsparser.NewKotlinV8(), goparser.New(), tsparser.NewTypeScript()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := fileParserProfile(t, s.raw(t), repo, "lib/Screens.kt"); got != "treesitter:kotlin:v8" {
		t.Fatalf("legacy profile = %q", got)
	}
	assertKotlinFacade(t, s.path, repo, "lib/Screens.kt", "")
	// The unclean v8 root also kept the detached @JvmName from hrun.
	v8 := "lib.visible|public|7-7|fun visible(x: kotlin.Int) {},lib.hrun|public|15-15|fun hrun(x: kotlin.Int) {},lib.target|public|16-16|fun target() {}" +
		" edges  refs hidden@4,hidden()@4,target@5,renamed@10,renamed()@10,synth@13,synth()@13"
	if got := swallowFacts(t, r); got != v8 {
		t.Fatalf("v8 facts =\n%s\nwant\n%s", got, v8)
	}
	for _, caller := range []string{"visibleCall", "detachedCall", "hiddenCall", "renamedCall", "renamedSource", "synthCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v8 %s -> %q, want unresolved", caller, got)
		}
	}

	summary, err := r.idx.Update(ctx, Options{RepoRoot: root, Paths: []string{"Other.kt"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 2 || summary.FilesIndexed != 2 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("v8-to-v9 update=%+v", summary)
	}
	for path, want := range map[string]string{"lib/Screens.kt": "treesitter:kotlin:v11", "Other.kt": "treesitter:kotlin:v11", "Caller.java": "treesitter:java:v8", "main.go": "go-ast:go:v1", "caller.ts": "treesitter:typescript:v1"} {
		if got := fileParserProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("%s profile=%q, want %q", path, got, want)
		}
	}
	assertKotlinFacade(t, s.path, repo, "lib/Screens.kt", "lib.ScreensKt|explicit=0|multifile=0")
	if got := swallowFacts(t, r); !strings.HasSuffix(got, " edges lib.hidden->target@5 refs target@5") || !strings.HasPrefix(got, "lib.hidden|private|2-6|") {
		t.Fatalf("v9 facts = %q", got)
	}
	for caller, want := range map[string]string{"visibleCall": "lib.visible|", "detachedCall": "lib.hrun|"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); !strings.HasPrefix(got, want) {
			t.Fatalf("v9 %s -> %q, want %s", caller, got, want)
		}
	}
	assertJVMQueryRelation(t, r, "app.Caller.visibleCall", "lib.visible")
	assertJVMQueryRelation(t, r, "lib.hidden", "lib.target")
	for _, caller := range []string{"hiddenCall", "renamedCall", "renamedSource", "synthCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v9 %s -> %q, want unresolved", caller, got)
		}
	}
	if got := jvmNameFacts(t, r); got != "lib.hrun=hexec,lib.renamed=execute" {
		t.Fatalf("v9 name facts = %q", got)
	}
	r.assertFreshParity(t, "Kotlin v8-to-v9 swallowed function convergence")
	again, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil || again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v, %v; want no-op", again, err)
	}
}
