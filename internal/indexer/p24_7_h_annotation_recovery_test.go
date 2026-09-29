//go:build cgo

package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// Every Kotlin file below ends in a declaration after the annotated one: that
// later declaration is what makes the grammar detach an own-line `@Ann(arg)`
// from its `fun`, the split v7 recovers.
const recoveryCaller = `package app;
import lib.SplitKt;
import lib.CommonKt;
import lib.SynthKt;
import lib.OverKt;
import lib.GuardKt;
import lib.CollideKt;
import lib.QualifiedKt;
import lib.AliasedKt;
import lib.HidesKt;
import lib.ForeignKt;
import lib.ForeignAliasKt;
class Caller {
    void renamed() { SplitKt.sexec(1); }
    void renamedSource() { SplitKt.srun(1); }
    void sibling() { SplitKt.other(); }
    void deprecated() { CommonKt.drun(1); }
    void throwsCall() { CommonKt.trun(1); }
    void suppressed() { CommonKt.urun(1); }
    void optIn() { CommonKt.orun(1); }
    void hidden() { SynthKt.hrun(); }
    void overZero() { OverKt.oexec(); }
    void overOne() { OverKt.oexec(1); }
    void overSource() { OverKt.vrun(1); }
    void swallowed() { GuardKt.execute(); }
    void collision() { CollideKt.execute(); }
    void qualified() { QualifiedKt.qexec(1); }
    void aliased() { AliasedKt.aexec(1); }
    void hides() { HidesKt.hexec(1); }
    void hidesSource() { HidesKt.hrun2(1); }
    void foreign() { ForeignKt.fexec(1); }
    void foreignSource() { ForeignKt.frun(1); }
    void foreignAlias() { ForeignAliasKt.zexec(1); }
    void foreignAliasSource() { ForeignAliasKt.zrun(1); }
}`

func recoveryTree() tree {
	return tree{
		"Caller.java":         recoveryCaller,
		"lib/Split.kt":        "package lib\n@JvmName(\"sexec\")\nfun srun(x: kotlin.Int) {}\nfun other() {}\n",
		"lib/Common.kt":       "package lib\n@Deprecated(\"x\")\nfun drun(x: kotlin.Int) {}\n@Throws(Exception::class)\nfun trun(x: kotlin.Int) {}\n@Suppress(\"UNUSED\")\nfun urun(x: kotlin.Int) {}\n@OptIn(ExperimentalStdlibApi::class)\nfun orun(x: kotlin.Int) {}\nfun last() {}\n",
		"lib/Synth.kt":        "package lib\n@JvmSynthetic\n@Suppress(\"x\")\nfun hrun() {}\nfun other2() {}\n",
		"lib/Over.kt":         "package lib\n@JvmOverloads\n@JvmName(\"oexec\")\nfun vrun(x: kotlin.Int = 0) {}\nfun other3() {}\n",
		"lib/Guard.kt":        "package lib\n@JvmName(\"execute\") fun a() { println() }\nfun execute(): kotlin.Int { return 1 }\n",
		"lib/Collide.kt":      "package lib\n@JvmName(\"execute\")\nfun ca() {}\n\nfun execute(): kotlin.Int = 1\n",
		"lib/Qualified.kt":    "package lib\n@kotlin.jvm.JvmName(\"qexec\")\nfun qrun(x: kotlin.Int) {}\nfun other4() {}\n",
		"lib/Aliased.kt":      "package lib\nimport kotlin.jvm.JvmName as JN\n@JN(\"aexec\")\nfun arun(x: kotlin.Int) {}\nfun other5() {}\n",
		"lib/Hides.kt":        "package lib\nimport kotlin.jvm.JvmName as JN\n@JvmName(\"hexec\")\nfun hrun2(x: kotlin.Int) {}\nfun other6() {}\n",
		"lib/Foreign.kt":      "package lib\nimport other.JvmName\n@JvmName(\"fexec\")\nfun frun(x: kotlin.Int) {}\nfun other7() {}\n",
		"lib/ForeignAlias.kt": "package lib\nimport other.Named as JN\n@JN(\"zexec\")\nfun zrun(x: kotlin.Int) {}\nfun other8() {}\n",
	}
}

// Recovered own-line annotations bind exactly as the attached ones do: a
// proven rename moves the Java spelling, an ABI-neutral annotation keeps the
// source name, JvmSynthetic hides, JvmOverloads widens arity, and an unknown
// or colliding spelling stays unresolved. The swallowed declaration stays
// fail closed.
func TestP247HRecoveredTopLevelAnnotations(t *testing.T) {
	r := newLifecycleRepo(t, recoveryTree())
	for path, facade := range map[string]string{
		"lib/Split.kt": "lib.SplitKt", "lib/Common.kt": "lib.CommonKt", "lib/Synth.kt": "lib.SynthKt", "lib/Over.kt": "lib.OverKt",
		"lib/Collide.kt": "lib.CollideKt", "lib/Qualified.kt": "lib.QualifiedKt", "lib/Aliased.kt": "lib.AliasedKt",
		"lib/Hides.kt": "lib.HidesKt", "lib/Foreign.kt": "lib.ForeignKt", "lib/ForeignAlias.kt": "lib.ForeignAliasKt", "lib/Guard.kt": "",
	} {
		if facade != "" {
			facade += "|explicit=0|multifile=0"
		}
		assertKotlinFacade(t, r.dbPath, r.repoID, path, facade)
	}
	bound := map[string]string{
		"renamed":            "lib.srun|@JvmName(\"sexec\")\nfun srun(x: kotlin.Int) {}",
		"sibling":            "lib.other|fun other() {}",
		"deprecated":         "lib.drun|@Deprecated(\"x\")\nfun drun(x: kotlin.Int) {}",
		"throwsCall":         "lib.trun|@Throws(Exception::class)\nfun trun(x: kotlin.Int) {}",
		"suppressed":         "lib.urun|@Suppress(\"UNUSED\")\nfun urun(x: kotlin.Int) {}",
		"optIn":              "lib.orun|@OptIn(ExperimentalStdlibApi::class)\nfun orun(x: kotlin.Int) {}",
		"overZero":           "lib.vrun|@JvmOverloads\n@JvmName(\"oexec\")\nfun vrun(x: kotlin.Int = 0) {}",
		"overOne":            "lib.vrun|@JvmOverloads\n@JvmName(\"oexec\")\nfun vrun(x: kotlin.Int = 0) {}",
		"qualified":          "lib.qrun|@kotlin.jvm.JvmName(\"qexec\")\nfun qrun(x: kotlin.Int) {}",
		"aliased":            "lib.arun|@JN(\"aexec\")\nfun arun(x: kotlin.Int) {}",
		"foreignAliasSource": "lib.zrun|@JN(\"zexec\")\nfun zrun(x: kotlin.Int) {}",
	}
	for _, caller := range []string{"renamed", "renamedSource", "sibling", "deprecated", "throwsCall", "suppressed", "optIn", "hidden", "overZero", "overOne", "overSource", "swallowed", "collision", "qualified", "aliased", "hides", "hidesSource", "foreign", "foreignSource", "foreignAlias", "foreignAliasSource"} {
		got, _ := jvmNameCall(t, r, "app.Caller."+caller)
		if got != bound[caller] {
			t.Errorf("%s -> %q, want %q", caller, got, bound[caller])
			continue
		}
		if got != "" {
			assertJVMQueryRelation(t, r, "app.Caller."+caller, got[:strings.IndexByte(got, '|')])
		}
	}
	// The hidden function keeps JvmSynthetic in the persisted signature, the
	// only place the Store reads it from.
	var sig string
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT signature FROM symbols WHERE repo_id=? AND qualified_name='lib.hrun'`, r.repoID).Scan(&sig); err != nil || sig != "@JvmSynthetic\n@Suppress(\"x\")\nfun hrun() {}" {
		t.Fatalf("hidden signature = %q, %v", sig, err)
	}
	if got := jvmNameFacts(t, r); got != "lib.arun=aexec,lib.ca=execute,lib.frun=?,lib.hrun2=?,lib.qrun=qexec,lib.srun=sexec,lib.vrun=oexec" {
		t.Fatalf("name facts = %q", got)
	}
	var known, lo, hi int
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT e.is_known,e.jvm_arity_min,e.jvm_arity_max FROM kotlin_jvm_callable_evidence e JOIN symbols s ON s.id=e.symbol_id WHERE e.repo_id=? AND s.qualified_name='lib.vrun'`, r.repoID).Scan(&known, &lo, &hi); err != nil || known != 1 || lo != 0 || hi != 1 {
		t.Fatalf("JvmOverloads evidence = (%d,%d,%d), %v", known, lo, hi, err)
	}
	r.assertFreshParity(t, "recovered top-level annotations")
}

// Moving an annotation between its own line and the declaration's line only
// changes the grammar's tree, never the compiler's meaning, so bindings and
// evidence stay the same across the layout lifecycle.
func TestP247HAnnotationLayoutLifecycle(t *testing.T) {
	caller := "package app;\nimport lib.ActionsKt;\nclass Caller {\n    void viaRun() { ActionsKt.run(1); }\n    void viaExecute() { ActionsKt.execute(1); }\n}"
	sameLine := "package lib\n@JvmName(\"execute\") fun run(x: kotlin.Int) {}\nfun other() {}\n"
	ownLine := "package lib\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}\n"
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "lib/Actions.kt": sameLine})
	for i, content := range []string{sameLine, ownLine, sameLine} {
		if i > 0 {
			r.write(t, "lib/Actions.kt", content)
			r.update(t, "lib/Actions.kt")
		}
		step := []string{"same-line", "own-line", "same-line again"}[i]
		assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "lib.ActionsKt|explicit=0|multifile=0")
		if got, _ := jvmNameCall(t, r, "app.Caller.viaRun"); got != "" {
			t.Errorf("%s: viaRun -> %q, want unresolved", step, got)
		}
		got, _ := jvmNameCall(t, r, "app.Caller.viaExecute")
		if !strings.HasPrefix(got, "lib.run|@JvmName(\"execute\")") {
			t.Errorf("%s: viaExecute -> %q", step, got)
		}
		if facts := jvmNameFacts(t, r); facts != "lib.run=execute" {
			t.Errorf("%s: name facts %q", step, facts)
		}
		assertJVMQueryRelation(t, r, "app.Caller.viaExecute", "lib.run")
		r.assertFreshParity(t, step)
	}
}

// The P24.7-F JvmName lifecycle on the own-line layout: rename, collision and
// delete/restore re-decide through the recovered evidence and the existing
// owner-name invalidation.
func TestP247HOwnLineJvmNameLifecycle(t *testing.T) {
	caller := "package app;\nimport lib.ActionsKt;\nclass Caller {\n    void viaRun() { ActionsKt.run(1); }\n    void viaExecute() { ActionsKt.execute(1); }\n    void viaPerform() { ActionsKt.perform(1); }\n}"
	file := func(annotation string, extra ...string) string {
		src := "package lib\n"
		if annotation != "" {
			src += annotation + "\n"
		}
		return src + "fun run(x: kotlin.Int) {}\n" + strings.Join(append(extra, "fun other() {}"), "\n") + "\n"
	}
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "lib/Actions.kt": file("")})
	check := func(step, bound, facts string) {
		t.Helper()
		for _, m := range []string{"viaRun", "viaExecute", "viaPerform"} {
			got, _ := jvmNameCall(t, r, "app.Caller."+m)
			if want := m == bound; want != strings.HasPrefix(got, "lib.run|") || !want && got != "" {
				t.Errorf("%s: %s -> %q, bound=%q", step, m, got, bound)
			}
		}
		if got := jvmNameFacts(t, r); got != facts {
			t.Errorf("%s: name facts %q, want %q", step, got, facts)
		}
		r.assertFreshParity(t, step)
	}
	edit := func(content string) {
		t.Helper()
		r.write(t, "lib/Actions.kt", content)
		if summary := r.update(t, "lib/Actions.kt"); summary.ResolveMode != "paths+names" {
			t.Fatalf("edit summary mode=%q", summary.ResolveMode)
		}
	}
	execute, perform := `@JvmName("execute")`, `@JvmName("perform")`
	check("plain", "viaRun", "")
	edit(file(execute))
	check("execute", "viaExecute", "lib.run=execute")
	edit(file(perform))
	check("perform", "viaPerform", "lib.run=perform")
	edit(file(""))
	check("plain again", "viaRun", "")

	edit(file(execute))
	check("collision base", "viaExecute", "lib.run=execute")
	edit(file(execute, "fun execute(x: kotlin.Long) {}"))
	check("competitor added", "", "lib.run=execute")
	edit(file(execute))
	check("competitor removed", "viaExecute", "lib.run=execute")

	r.remove(t, "lib/Actions.kt")
	r.update(t, "lib/Actions.kt")
	check("deleted", "", "")
	r.write(t, "lib/Actions.kt", file(execute))
	r.update(t, "lib/Actions.kt")
	check("restored", "viaExecute", "lib.run=execute")
}

// An existing v6 database, written by the genuine v6 parser that refused the
// facade on every detached annotation, converges on a path-scoped update:
// stale Kotlin reparses to the current profile, recovered facades and evidence appear, and the
// unchanged Java v2 callers re-decide without reparsing -- the hidden function
// staying hidden once its facade exists.
func TestP247HKotlinV6ToV7Convergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string]string{
		"Caller.java": "package app;\nimport lib.SplitKt;\nimport lib.SynthKt;\nclass Caller {\n    void renamed() { SplitKt.sexec(1); }\n    void sibling() { SplitKt.other(); }\n    void hidden() { SynthKt.hrun(); }\n}",
		"Split.kt":    "package lib\n@JvmName(\"sexec\")\nfun srun(x: kotlin.Int) {}\nfun other() {}\n",
		"Synth.kt":    "package lib\n@JvmSynthetic\n@Suppress(\"x\")\nfun hrun() {}\nfun other2() {}\n",
		"Other.kt":    "package lib\nfun helper() {}",
		"main.go":     "package main\nfunc main() {}\n",
		"caller.ts":   "export function caller(): void {}\n",
	}
	for path, content := range files {
		writeProfileFile(t, filepath.Join(root, path), content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJava(), tsparser.NewKotlinV6(), goparser.New(), tsparser.NewTypeScript()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	for _, path := range []string{"Split.kt", "Synth.kt"} {
		assertKotlinFacade(t, s.path, repo, path, "")
	}
	for _, caller := range []string{"renamed", "sibling", "hidden"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v6 %s -> %q, want unresolved", caller, got)
		}
	}
	if got := jvmNameFacts(t, r); got != "" {
		t.Fatalf("v6 name facts = %q", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root, Paths: []string{"Other.kt"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 3 || summary.FilesIndexed != 3 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("v6-to-v7 update=%+v", summary)
	}
	for path, want := range map[string]string{"Split.kt": "treesitter:kotlin:v8", "Synth.kt": "treesitter:kotlin:v8", "Other.kt": "treesitter:kotlin:v8", "Caller.java": "treesitter:java:v2", "main.go": "go-ast:go:v1", "caller.ts": "treesitter:typescript:v1"} {
		if got := p246FileProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("%s profile=%q, want %q", path, got, want)
		}
	}
	assertKotlinFacade(t, s.path, repo, "Split.kt", "lib.SplitKt|explicit=0|multifile=0")
	assertKotlinFacade(t, s.path, repo, "Synth.kt", "lib.SynthKt|explicit=0|multifile=0")
	if got := jvmNameFacts(t, r); got != "lib.srun=sexec" {
		t.Fatalf("v7 name facts = %q", got)
	}
	for caller, want := range map[string]string{"renamed": "lib.srun", "sibling": "lib.other", "hidden": ""} {
		got, _ := jvmNameCall(t, r, "app.Caller."+caller)
		if want == "" {
			if got != "" {
				t.Fatalf("v7 %s -> %q, want unresolved", caller, got)
			}
			continue
		}
		if !strings.HasPrefix(got, want+"|") {
			t.Fatalf("v7 %s -> %q, want %s", caller, got, want)
		}
		assertJVMQueryRelation(t, r, "app.Caller."+caller, want)
	}
	r.assertFreshParity(t, "Kotlin v6-to-v7 annotation recovery convergence")
	again, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil || again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v, %v; want no-op", again, err)
	}
}
