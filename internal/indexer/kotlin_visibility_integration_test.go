//go:build cgo

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

const visibilityKotlin = "package lib\n" +
	"@Suppress(names = [\"unused\"])\nprivate fun helper(x: kotlin.Int) {}\n" +
	"@Suppress(\"private\") fun visible(x: kotlin.Int) {}\n" +
	"@Preview(showBackground = true)\ninternal fun inner(x: kotlin.Int) {}\n" +
	"@JvmName(\"execute\")\n@Preview(showBackground = true)\nprivate fun run(x: kotlin.Int) {}\n" +
	"@JvmSynthetic\n@Preview(showBackground = true)\nprivate fun hidden(x: kotlin.Int) {}\n" +
	"fun last() {}\n"

// v7 cut a class's source at its first `{`, so primary-constructor modifiers
// read as the class visibility and hid a public class's companion bridges.
const visibilityService = "package lib\nclass Service private constructor() {\n    companion object {\n        @JvmStatic fun create() {}\n    }\n}\n"

const visibilityCaller = "package app;\nimport lib.VisKt;\nimport lib.Service;\nclass Caller {\n" +
	"    void privateCall() { VisKt.helper(1); }\n" +
	"    void publicCall() { VisKt.visible(1); }\n" +
	"    void internalCall() { VisKt.inner(1); }\n" +
	"    void renamedCall() { VisKt.execute(1); }\n" +
	"    void renamedSource() { VisKt.run(1); }\n" +
	"    void hiddenCall() { VisKt.hidden(1); }\n" +
	"    void factoryCall() { Service.create(); }\n}"

func kotlinVisibilities(t *testing.T, r *lifecycleRepo) string {
	t.Helper()
	rows, err := r.raw(t).QueryContext(r.ctx, `SELECT s.qualified_name||'='||s.visibility FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path='lib/Vis.kt' ORDER BY s.qualified_name`, r.repoID)
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

// A private function whose annotation argument holds `=` stays private, so the
// Java call javac rejects stays unresolved; annotation text spelling a
// visibility never hides a public function. The internal, renamed and
// JvmSynthetic functions keep their own refusals.
func TestKotlinStructuralVisibilityJavaAccess(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Caller.java": visibilityCaller, "lib/Vis.kt": visibilityKotlin, "lib/Service.kt": visibilityService})
	if got := kotlinVisibilities(t, r); got != "lib.helper=private,lib.hidden=private,lib.inner=internal,lib.last=public,lib.run=private,lib.visible=public" {
		t.Fatalf("visibility = %q", got)
	}
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Vis.kt", "lib.VisKt|explicit=0|multifile=0")
	for _, caller := range []string{"privateCall", "internalCall", "renamedCall", "renamedSource", "hiddenCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Errorf("%s -> %q, want unresolved", caller, got)
		}
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.publicCall"); got != "lib.visible|@Suppress(\"private\") fun visible(x: kotlin.Int) {}" {
		t.Fatalf("publicCall -> %q", got)
	}
	assertJVMQueryRelation(t, r, "app.Caller.publicCall", "lib.visible")
	assertJVMNoQueryRelation(t, r, "app.Caller.privateCall", "lib.helper")
	if got, _ := jvmNameCall(t, r, "app.Caller.factoryCall"); !strings.HasPrefix(got, "lib.Service.Companion.create|") {
		t.Fatalf("factoryCall -> %q", got)
	}
	if got := jvmNameFacts(t, r); got != "lib.run=execute" {
		t.Fatalf("name facts = %q", got)
	}
	var sig string
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT signature FROM symbols WHERE repo_id=? AND qualified_name='lib.hidden'`, r.repoID).Scan(&sig); err != nil || !strings.HasPrefix(sig, "@JvmSynthetic\n") {
		t.Fatalf("hidden signature = %q, %v", sig, err)
	}
	r.assertFreshParity(t, "structural Kotlin visibility")
}

// kotlinV7Visibility reproduces the treesitter:kotlin:v7 parser for fixtures
// whose declarations all carry their annotations attached: v7 and v8 emit
// identical facts there except Visibility, which v7 read from the declaration
// source cut at its first `{` or `=` and matched against visibility words.
// That source is exactly the bytes of the symbol Range, so the wrapper
// re-applies the historical algorithm to them and keeps everything else. A
// declaration recovered from detached annotations starts before its Range in
// v7, so the wrapper refuses one rather than fabricate a wrong v7 database.
type kotlinV7Visibility struct{ *tsparser.KotlinAdapter }

func (a kotlinV7Visibility) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:kotlin:v7", EmitsCallEdges: true}
}

func (a kotlinV7Visibility) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.KotlinAdapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	lines := []int{0}
	for i, c := range content {
		if c == '\n' {
			lines = append(lines, i+1)
		}
	}
	for i, sym := range pf.Symbols {
		start := lines[sym.Range.StartLine-1] + sym.Range.StartCol - 1
		end := lines[sym.Range.EndLine-1] + sym.Range.EndCol - 1
		text := string(content[start:end])
		if sym.Kind == "function" && strings.TrimSpace(text) != sym.Signature {
			return pf, fmt.Errorf("%s: v7 visibility of a recovered declaration is not reproducible", sym.QualifiedName)
		}
		if brace := strings.IndexByte(text, '{'); brace >= 0 {
			text = text[:brace]
		}
		if eq := strings.IndexByte(text, '='); eq >= 0 {
			text = text[:eq]
		}
		pf.Symbols[i].Visibility = "public"
		for _, v := range []string{"private", "protected", "internal"} {
			if regexp.MustCompile(`\b` + v + `\b`).MatchString(text) {
				pf.Symbols[i].Visibility = v
				break
			}
		}
	}
	return pf, nil
}

// A v7 database persisted false-public private functions (their Java callers
// bound) and a false-private public function and class (their Java callers
// unresolved). A
// path-scoped update reparses only the stale Kotlin to the current profile; the unchanged Java
// v2 caller re-decides both edges without being reparsed.
func TestKotlinV7ToV8VisibilityConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string]string{
		"Caller.java":    visibilityCaller,
		"lib/Vis.kt":     visibilityKotlin,
		"lib/Service.kt": visibilityService,
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
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJava(), kotlinV7Visibility{tsparser.NewKotlin()}, goparser.New(), tsparser.NewTypeScript()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	if got := fileParserProfile(t, s.raw(t), repo, "lib/Vis.kt"); got != "treesitter:kotlin:v7" {
		t.Fatalf("legacy profile = %q", got)
	}
	if got := kotlinVisibilities(t, r); got != "lib.helper=public,lib.hidden=public,lib.inner=public,lib.last=public,lib.run=public,lib.visible=private" {
		t.Fatalf("v7 visibility = %q", got)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.privateCall"); !strings.HasPrefix(got, "lib.helper|") {
		t.Fatalf("v7 privateCall -> %q, want the historical false positive", got)
	}
	if got, _ := jvmNameCall(t, r, "app.Caller.renamedCall"); !strings.HasPrefix(got, "lib.run|") {
		t.Fatalf("v7 renamedCall -> %q, want the historical false positive", got)
	}
	// The false-public internal function stayed unbound even in v7: its
	// internal modifier already vetoes the Java ABI independently.
	for _, caller := range []string{"publicCall", "factoryCall", "internalCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v7 %s -> %q, want unresolved", caller, got)
		}
	}

	summary, err := r.idx.Update(ctx, Options{RepoRoot: root, Paths: []string{"Other.kt"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 3 || summary.FilesIndexed != 3 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("v7-to-v8 update=%+v", summary)
	}
	for path, want := range map[string]string{"lib/Vis.kt": "treesitter:kotlin:v12", "lib/Service.kt": "treesitter:kotlin:v12", "Other.kt": "treesitter:kotlin:v12", "Caller.java": "treesitter:java:v12", "main.go": "go-ast:go:v1", "caller.ts": "treesitter:typescript:v2"} {
		if got := fileParserProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("%s profile=%q, want %q", path, got, want)
		}
	}
	if got := kotlinVisibilities(t, r); got != "lib.helper=private,lib.hidden=private,lib.inner=internal,lib.last=public,lib.run=private,lib.visible=public" {
		t.Fatalf("v8 visibility = %q", got)
	}
	for _, caller := range []string{"privateCall", "internalCall", "renamedCall", "renamedSource", "hiddenCall"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v8 %s -> %q, want unresolved", caller, got)
		}
	}
	for caller, want := range map[string]string{"publicCall": "lib.visible|", "factoryCall": "lib.Service.Companion.create|"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); !strings.HasPrefix(got, want) {
			t.Fatalf("v8 %s -> %q, want %s", caller, got, want)
		}
	}
	assertJVMQueryRelation(t, r, "app.Caller.publicCall", "lib.visible")
	assertJVMNoQueryRelation(t, r, "app.Caller.privateCall", "lib.helper")
	r.assertFreshParity(t, "Kotlin v7-to-v8 visibility convergence")
	again, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil || again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v, %v; want no-op", again, err)
	}
}
