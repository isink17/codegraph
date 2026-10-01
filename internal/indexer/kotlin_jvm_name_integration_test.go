//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// jvmNameCall returns the destination of the single call edge in caller as
// "qualified_name|signature" ("" when unresolved) and its symbol id, and
// asserts that the call reference is bound to exactly the same symbol.
func jvmNameCall(t *testing.T, r *lifecycleRepo, caller string) (string, int64) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dst, ref sql.NullInt64
	var target string
	err = db.QueryRowContext(r.ctx, `SELECT e.dst_symbol_id,COALESCE(d.qualified_name||'|'||d.signature,''),
		(SELECT rt.symbol_id FROM references_tbl rt WHERE rt.repo_id=e.repo_id AND rt.file_id=e.file_id AND rt.start_line=e.line AND rt.ref_kind='call' AND rt.qualified_name=e.dst_name)
		FROM edges e JOIN symbols src ON src.id=e.src_symbol_id LEFT JOIN symbols d ON d.id=e.dst_symbol_id
		WHERE e.repo_id=? AND src.qualified_name=? AND e.edge_kind='calls'`, r.repoID, caller).Scan(&dst, &target, &ref)
	if err != nil {
		t.Fatalf("call edge of %s: %v", caller, err)
	}
	if dst != ref {
		t.Fatalf("%s edge target %v but reference target %v", caller, dst, ref)
	}
	return target, dst.Int64
}

func jvmNameSymbolID(t *testing.T, r *lifecycleRepo, qname, signature string) int64 {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	if err := db.QueryRowContext(r.ctx, `SELECT id FROM symbols WHERE repo_id=? AND qualified_name=? AND signature=?`, r.repoID, qname, signature).Scan(&id); err != nil {
		t.Fatalf("symbol %s %q: %v", qname, signature, err)
	}
	return id
}

// jvmNameFacts renders the persisted 044 rows as "qname=name" ("qname=?" for
// unknown), sorted.
func jvmNameFacts(t *testing.T, r *lifecycleRepo) string {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(r.ctx, `SELECT s.qualified_name||'='||COALESCE(n.jvm_name,'?') FROM kotlin_jvm_name_evidence n JOIN symbols s ON s.id=n.symbol_id AND s.file_id=n.file_id WHERE n.repo_id=? ORDER BY 1`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fact string
		if err := rows.Scan(&fact); err != nil {
			t.Fatal(err)
		}
		out = append(out, fact)
	}
	return strings.Join(out, ",")
}

func jvmNameRowCount(t *testing.T, r *lifecycleRepo) int {
	t.Helper()
	var n int
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT COUNT(*) FROM kotlin_jvm_name_evidence WHERE repo_id=?`, r.repoID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const jvmNameCaller = `package app;
import lib.ActionsKt;
import lib.AliasedKt;
import lib.API;
import lib.ApiKt;
import lib.Multi;
import lib.Service;
import lib.Holder;
import lib.Named;
class Caller {
    void topRenamed() { ActionsKt.execute(1); }
    void topSource() { ActionsKt.run(1); }
    void topWrongArity() { ActionsKt.execute(1, 2); }
    void defaultShort() { ActionsKt.dexec(1); }
    void defaultFull() { ActionsKt.dexec(1, ""); }
    void overloadShort() { ActionsKt.oexec(1); }
    void overloadFull() { ActionsKt.oexec(1, ""); }
    void overloadOrderShort() { ActionsKt.oexec2(1); }
    void overloadOrderFull() { ActionsKt.oexec2(1, ""); }
    void zeroRenamed() { ActionsKt.zexec(); }
    void zeroSource() { ActionsKt.zrun(); }
    void zeroOverloadNone() { ActionsKt.zoexec(); }
    void zeroOverloadOne() { ActionsKt.zoexec(1); }
    void synthetic() { ActionsKt.synth(1); }
    void internalRenamed() { ActionsKt.inexec(1); }
    void extensionRenamed() { ActionsKt.extexec("", 1); }
    void suspendRenamed() { ActionsKt.susexec(1); }
    void valueRenamed() { ActionsKt.valexec(1); }
    void sameArity() { ActionsKt.clash(1); }
    void pairRenamed() { ActionsKt.pair(1); }
    void pairSource() { ActionsKt.pair(1, 2); }
    void multiOne() { ActionsKt.multi(1); }
    void multiTwo() { ActionsKt.multi(1, 2); }
    void unknownSibling() { ActionsKt.veto(1); }
    void reservedSource() { ActionsKt.kw(1); }
    void aliasedRenamed() { AliasedKt.aexec(1); }
    void aliasedSource() { AliasedKt.arun(1); }
    void aliasedSynthetic() { AliasedKt.synth2(1); }
    void apiRenamed() { API.execute(1); }
    void apiSource() { API.apiRun(1); }
    void apiDefaultFacade() { ApiKt.execute(1); }
    void multiExecute() { Multi.execute(1); }
    void multiPerform() { Multi.perform(1); }
    void multiSource() { Multi.m1(1); }
    void objectRenamed() { Service.INSTANCE.execute(1); }
    void objectSource() { Service.INSTANCE.run(1); }
    void objectStaticSpelling() { Service.execute(1); }
    void objectJvmStatic() { Service.sexec(1); }
    void objectJvmStaticInstance() { Service.INSTANCE.sexec(1); }
    void objectZero() { Service.INSTANCE.zexec(); }
    void objectZeroSource() { Service.INSTANCE.zrun(); }
    void companionRenamed() { Holder.Companion.execute(1); }
    void companionSource() { Holder.Companion.run(1); }
    void companionOuter() { Holder.execute(1); }
    void companionStaticOuter() { Holder.sexec(1); }
    void companionStaticField() { Holder.Companion.sexec(1); }
    void namedRenamed() { Named.Factory.execute(1); }
    void namedGuessed() { Named.Companion.execute(1); }
    void namedStaticOuter() { Named.sexec(1); }
    void namedStaticField() { Named.Factory.sexec(1); }
}`

// Top-level declarations keep the annotation on the declaration's own line or
// before another modifier: the grammar splits `@Ann(args)` + newline + `fun`
// at the root, which the facade guard then refuses (see the misparse test).
const jvmNameActions = `package lib
@JvmName("execute") fun run(x: kotlin.Int) {}
@JvmName("dexec") fun drun(a: kotlin.Int, b: kotlin.String = "") {}
@JvmName("oexec") @JvmOverloads fun orun(a: kotlin.Int, b: kotlin.String = "") {}
@JvmOverloads @JvmName("oexec2") fun orun2(a: kotlin.Int, b: kotlin.String = "") {}
@JvmName("zexec") public fun zrun() {}
@JvmName("zoexec") @JvmOverloads fun zorun(x: kotlin.Int = 0) {}
@JvmSynthetic @JvmName("synth") fun hidden(x: kotlin.Int) {}
fun synth(x: kotlin.Int) {}
@JvmName("inexec") internal fun irun(x: kotlin.Int) {}
@JvmName("extexec") fun kotlin.String.erun(x: kotlin.Int) {}
@JvmName("susexec") suspend fun srun(x: kotlin.Int) {}
@JvmName("valexec") fun vrun(x: Token) {}
@JvmInline value class Token(val v: kotlin.Int)
@JvmName("clash") fun c1(x: kotlin.Int) {}
fun clash(x: kotlin.Long) {}
@JvmName("pair") fun p1(x: kotlin.Int) {}
fun pair(x: kotlin.Int, y: kotlin.Int) {}
@JvmName("multi") fun ma(x: kotlin.Int) {}
@JvmName("multi") fun mb(x: kotlin.Int, y: kotlin.Int) {}
@JvmName("multi") fun mc(x: kotlin.String) {}
@JvmName("veto") fun vt(x: kotlin.Int) {}
fun veto(x: Custom) {}
@JvmName("class") fun kw(x: kotlin.Int) {}
`

const jvmNameService = `package lib
object Service {
    @JvmName("execute")
    fun run(x: kotlin.Int) {}
    @JvmStatic
    @JvmName("sexec")
    fun srun(x: kotlin.Int) {}
    @JvmName("zexec")
    fun zrun() {}
}
class Holder {
    companion object {
        @JvmName("execute")
        fun run(x: kotlin.Int) {}
        @JvmStatic @JvmName("sexec")
        fun srun(x: kotlin.Int) {}
    }
}
class Named {
    companion object Factory {
        @JvmName("execute")
        fun run(x: kotlin.Int) {}
        @JvmName("sexec")
        @JvmStatic
        fun srun(x: kotlin.Int) {}
    }
}
`

func jvmNameTree() tree {
	return tree{
		"Caller.java":    jvmNameCaller,
		"lib/Actions.kt": jvmNameActions,
		"lib/Aliased.kt": "package lib\nimport kotlin.jvm.JvmName as JN\nimport kotlin.jvm.JvmSynthetic as JS\n@JN(\"aexec\") fun arun(x: kotlin.Int) {}\n@JS @JN(\"synth2\") fun hidden2(x: kotlin.Int) {}\nfun synth2(x: kotlin.Int) {}\n",
		"lib/Api.kt":     "@file:JvmName(\"API\")\npackage lib\n@JvmName(\"execute\") fun apiRun(x: kotlin.Int) {}\n",
		"lib/M1.kt":      "@file:JvmName(\"Multi\")\n@file:JvmMultifileClass\npackage lib\n@JvmName(\"execute\") fun m1(x: kotlin.Int) {}\n",
		"lib/M2.kt":      "@file:JvmName(\"Multi\")\n@file:JvmMultifileClass\npackage lib\n@JvmName(\"perform\") fun m2(x: kotlin.Int) {}\n",
		"lib/Service.kt": jvmNameService,
	}
}

// Every supported owner family resolves the compiler-proven Java spelling to
// the canonical Kotlin source symbol; the source spelling retires, and
// ambiguity or ABI uncertainty stays unresolved.
func TestKotlinJvmNameOwnerFamilies(t *testing.T) {
	r := newLifecycleRepo(t, jvmNameTree())
	type target struct{ qname, sig string }
	actions := func(sig string) target {
		name := sig[strings.Index(sig, "fun ")+4:]
		name = name[:strings.IndexByte(name, '(')]
		return target{"lib." + name, sig}
	}
	want := map[string]target{
		"topRenamed":           actions(`@JvmName("execute") fun run(x: kotlin.Int) {}`),
		"defaultFull":          actions(`@JvmName("dexec") fun drun(a: kotlin.Int, b: kotlin.String = "") {}`),
		"overloadShort":        actions(`@JvmName("oexec") @JvmOverloads fun orun(a: kotlin.Int, b: kotlin.String = "") {}`),
		"overloadFull":         actions(`@JvmName("oexec") @JvmOverloads fun orun(a: kotlin.Int, b: kotlin.String = "") {}`),
		"overloadOrderShort":   actions(`@JvmOverloads @JvmName("oexec2") fun orun2(a: kotlin.Int, b: kotlin.String = "") {}`),
		"overloadOrderFull":    actions(`@JvmOverloads @JvmName("oexec2") fun orun2(a: kotlin.Int, b: kotlin.String = "") {}`),
		"zeroRenamed":          actions(`@JvmName("zexec") public fun zrun() {}`),
		"zeroOverloadNone":     actions(`@JvmName("zoexec") @JvmOverloads fun zorun(x: kotlin.Int = 0) {}`),
		"zeroOverloadOne":      actions(`@JvmName("zoexec") @JvmOverloads fun zorun(x: kotlin.Int = 0) {}`),
		"synthetic":            actions(`fun synth(x: kotlin.Int) {}`),
		"pairRenamed":          actions(`@JvmName("pair") fun p1(x: kotlin.Int) {}`),
		"pairSource":           actions(`fun pair(x: kotlin.Int, y: kotlin.Int) {}`),
		"multiTwo":             actions(`@JvmName("multi") fun mb(x: kotlin.Int, y: kotlin.Int) {}`),
		"apiRenamed":           {"lib.apiRun", `@JvmName("execute") fun apiRun(x: kotlin.Int) {}`},
		"multiExecute":         {"lib.m1", `@JvmName("execute") fun m1(x: kotlin.Int) {}`},
		"multiPerform":         {"lib.m2", `@JvmName("perform") fun m2(x: kotlin.Int) {}`},
		"objectRenamed":        {"lib.Service.run", "@JvmName(\"execute\")\n    fun run(x: kotlin.Int) {}"},
		"objectJvmStatic":      {"lib.Service.srun", "@JvmStatic\n    @JvmName(\"sexec\")\n    fun srun(x: kotlin.Int) {}"},
		"objectZero":           {"lib.Service.zrun", "@JvmName(\"zexec\")\n    fun zrun() {}"},
		"companionRenamed":     {"lib.Holder.Companion.run", "@JvmName(\"execute\")\n        fun run(x: kotlin.Int) {}"},
		"companionStaticOuter": {"lib.Holder.Companion.srun", "@JvmStatic @JvmName(\"sexec\")\n        fun srun(x: kotlin.Int) {}"},
		"companionStaticField": {"lib.Holder.Companion.srun", "@JvmStatic @JvmName(\"sexec\")\n        fun srun(x: kotlin.Int) {}"},
		"namedRenamed":         {"lib.Named.Factory.run", "@JvmName(\"execute\")\n        fun run(x: kotlin.Int) {}"},
		"namedStaticOuter":     {"lib.Named.Factory.srun", "@JvmName(\"sexec\")\n        @JvmStatic\n        fun srun(x: kotlin.Int) {}"},
		"namedStaticField":     {"lib.Named.Factory.srun", "@JvmName(\"sexec\")\n        @JvmStatic\n        fun srun(x: kotlin.Int) {}"},
	}
	unresolved := []string{
		"topSource", "topWrongArity", "defaultShort", "zeroSource", "internalRenamed", "extensionRenamed",
		"suspendRenamed", "valueRenamed", "sameArity", "multiOne", "unknownSibling", "reservedSource",
		"aliasedSource", "apiSource", "apiDefaultFacade", "multiSource", "objectSource", "objectStaticSpelling",
		"objectJvmStaticInstance", "objectZeroSource", "companionSource", "companionOuter", "namedGuessed",
	}
	for caller, w := range want {
		id := jvmNameSymbolID(t, r, w.qname, w.sig)
		got, gotID := jvmNameCall(t, r, "app.Caller."+caller)
		if gotID != id {
			t.Errorf("%s -> %q (id %d), want %s id %d", caller, got, gotID, w.qname, id)
			continue
		}
		assertJVMQueryRelation(t, r, "app.Caller."+caller, w.qname)
	}
	for _, caller := range unresolved {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Errorf("%s -> %q, want unresolved", caller, got)
		}
	}
	// Canonical identity: no symbol carries a JVM spelling.
	db := r.raw(t)
	var aliases int
	if err := db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM symbols WHERE repo_id=? AND language='kotlin' AND name IN ('execute','dexec','oexec','zexec','sexec','perform')`, r.repoID).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("JVM spelling symbols = %d, %v", aliases, err)
	}
	r.assertFreshParity(t, "JvmName owner families")
}

// Top-level misparse false positive: the grammar drops an annotated top-level
// function without an ERROR node. The facade is refused, so a surviving sibling
// cannot bind a call javac rejects or finds ambiguous. The own-line split that
// only detaches the annotation is recovered by v7 detached-annotation recovery
// and binds as javac does.
func TestKotlinJvmNameTopLevelMisparseNoFalsePositive(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.java": `package app;
import lib.GuardKt;
import lib.SplitKt;
class Caller {
    void ambiguous() { GuardKt.execute(); }
    void renamedSource() { SplitKt.split(1); }
    void renamed() { SplitKt.renamed(1); }
    void sibling() { SplitKt.other(); }
}`,
		"lib/Guard.kt": "package lib\n@JvmName(\"execute\") fun a() { println() }\nfun execute(): kotlin.Int { return 1 }\n",
		"lib/Split.kt": "package lib\n@JvmName(\"renamed\")\nfun split(x: kotlin.Int) {}\nfun other() {}\n",
	})
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Guard.kt", "")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Split.kt", "lib.SplitKt|explicit=0|multifile=0")
	for caller, want := range map[string]string{"ambiguous": "", "renamedSource": "", "renamed": "lib.split|", "sibling": "lib.other|"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); want == "" && got != "" || !strings.HasPrefix(got, want) {
			t.Errorf("%s -> %q, want %q", caller, got, want)
		}
	}
	r.assertFreshParity(t, "top-level misparse")
}

// The Kotlin source name never changes; only the persisted JVM name does.
// Owner-name invalidation must retire the old spelling and activate the new
// one, through known, unknown and absent evidence, deletion and collisions.
func TestKotlinJvmNameLifecycle(t *testing.T) {
	caller := `package app;
import lib.ActionsKt;
class Caller {
    void viaRun() { ActionsKt.run(1); }
    void viaExecute() { ActionsKt.execute(1); }
    void viaPerform() { ActionsKt.perform(1); }
    void viaExecute2() { ActionsKt.execute(1, 2); }
}`
	unrelated := "package app;\nimport lib.Missing;\nclass Unrelated { void call() { Missing.stop(); } }"
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Unrelated.java": unrelated, "lib/Actions.kt": "package lib\nfun run(x: kotlin.Int) {}\n", "lib/Other.kt": "package lib\nfun helper() {}\n"})
	type state struct{ run, execute, perform, execute2, facts string }
	check := func(step string, w state) {
		t.Helper()
		for caller, want := range map[string]string{"viaRun": w.run, "viaExecute": w.execute, "viaPerform": w.perform, "viaExecute2": w.execute2} {
			got, _ := jvmNameCall(t, r, "app.Caller."+caller)
			if got != want {
				t.Errorf("%s: %s -> %q, want %q", step, caller, got, want)
			}
		}
		if got := jvmNameFacts(t, r); got != w.facts {
			t.Errorf("%s: name facts %q, want %q", step, got, w.facts)
		}
		// Raw rows, not joined to symbols: a purge miss would leave orphans.
		if got, want := jvmNameRowCount(t, r), len(strings.FieldsFunc(w.facts, func(c rune) bool { return c == ',' })); got != want {
			t.Errorf("%s: %d 044 rows, want %d", step, got, want)
		}
		assertJVMUnresolved(t, r, "Unrelated.java", "Missing.stop")
		r.assertFreshParity(t, step)
	}
	edit := func(content string) {
		t.Helper()
		r.write(t, "lib/Actions.kt", content)
		summary := r.update(t, "lib/Actions.kt")
		// Owner-scoped: the four ActionsKt spellings plus the file's own names,
		// never the repository.
		if summary.ResolveMode != "paths+names" || summary.ResolveCrossFileTargets > 8 {
			t.Fatalf("edit summary mode=%q targets=%d", summary.ResolveMode, summary.ResolveCrossFileTargets)
		}
	}
	plain := "fun run(x: kotlin.Int) {}"
	execute := `@JvmName("execute") fun run(x: kotlin.Int) {}`
	perform := `@JvmName("perform") fun run(x: kotlin.Int) {}`
	file := func(decl string, extra ...string) string {
		return "package lib\n" + decl + "\n" + strings.Join(extra, "\n")
	}
	check("A plain", state{run: "lib.run|" + plain})
	edit(file(execute))
	check("B execute", state{execute: "lib.run|" + execute, facts: "lib.run=execute"})
	edit(file(perform))
	check("C perform", state{perform: "lib.run|" + perform, facts: "lib.run=perform"})
	edit(file(plain))
	check("D plain", state{run: "lib.run|" + plain})

	// known -> unknown -> known with another name -> none
	edit(file(execute))
	check("known", state{execute: "lib.run|" + execute, facts: "lib.run=execute"})
	unknown := "const val NAME = \"execute\"\n@JvmName(NAME) fun run(x: kotlin.Int) {}"
	edit(file(unknown))
	check("unknown", state{facts: "lib.run=?"})
	edit(file(perform))
	check("known again", state{perform: "lib.run|" + perform, facts: "lib.run=perform"})
	edit(file(plain))
	check("no row", state{run: "lib.run|" + plain})

	// collisions
	edit(file(execute))
	check("collision base", state{execute: "lib.run|" + execute, facts: "lib.run=execute"})
	edit(file(execute, "fun execute(x: kotlin.Long) {}"))
	check("same-arity competitor", state{facts: "lib.run=execute"})
	edit(file(execute))
	check("competitor removed", state{execute: "lib.run|" + execute, facts: "lib.run=execute"})
	edit(file(execute, "fun execute(x: kotlin.Int, y: kotlin.Int) {}"))
	check("different-arity competitor", state{execute: "lib.run|" + execute, execute2: "lib.execute|fun execute(x: kotlin.Int, y: kotlin.Int) {}", facts: "lib.run=execute"})
	edit(file(execute, "fun execute(x: Custom) {}"))
	check("unknown sibling", state{facts: "lib.run=execute"})
	edit(file(execute))

	// delete / restore
	r.remove(t, "lib/Actions.kt")
	r.update(t, "lib/Actions.kt")
	check("deleted", state{})
	r.write(t, "lib/Actions.kt", file(execute))
	r.update(t, "lib/Actions.kt")
	check("restored", state{execute: "lib.run|" + execute, facts: "lib.run=execute"})
}

// Object and companion owners follow the same owner-name invalidation: the
// renamed member spelling re-decides when the owner file changes.
func TestKotlinJvmNameMemberOwnerLifecycle(t *testing.T) {
	caller := `package app;
import lib.Service;
import lib.Holder;
class Caller {
    void objA() { Service.INSTANCE.execute(1); }
    void objB() { Service.INSTANCE.perform(1); }
    void objSource() { Service.INSTANCE.run(1); }
    void compA() { Holder.Companion.execute(1); }
    void compB() { Holder.Companion.perform(1); }
    void compStaticA() { Holder.execute(1); }
    void compStaticB() { Holder.perform(1); }
    void compSource() { Holder.Companion.run(1); }
}`
	source := func(annotation string) string {
		return "package lib\nobject Service {\n    " + annotation + "\n    fun run(x: kotlin.Int) {}\n}\nclass Holder {\n    companion object {\n        @JvmStatic " + annotation + "\n        fun run(x: kotlin.Int) {}\n    }\n}\n"
	}
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "lib/Service.kt": source("")})
	check := func(step string, bound map[string]string) {
		t.Helper()
		for _, m := range []string{"objA", "objB", "objSource", "compA", "compB", "compStaticA", "compStaticB", "compSource"} {
			got, _ := jvmNameCall(t, r, "app.Caller."+m)
			want := bound[m]
			if want == "" && got != "" || want != "" && !strings.HasPrefix(got, want+"|") {
				t.Errorf("%s: %s -> %q, want %q", step, m, got, want)
			}
		}
		r.assertFreshParity(t, step)
	}
	edit := func(annotation string) {
		t.Helper()
		r.write(t, "lib/Service.kt", source(annotation))
		if summary := r.update(t, "lib/Service.kt"); summary.ResolveMode != "paths+names" || summary.ResolveCrossFileTargets > 12 {
			t.Fatalf("edit summary mode=%q targets=%d", summary.ResolveMode, summary.ResolveCrossFileTargets)
		}
	}
	obj, comp := "lib.Service.run", "lib.Holder.Companion.run"
	check("plain", map[string]string{"objSource": obj, "compSource": comp})
	edit(`@JvmName("execute")`)
	check("execute", map[string]string{"objA": obj, "compA": comp, "compStaticA": comp})
	edit(`@JvmName("perform")`)
	check("perform", map[string]string{"objB": obj, "compB": comp, "compStaticB": comp})
	edit(`@JvmName(NAME)`)
	check("unknown", nil)
	edit("")
	check("removed", map[string]string{"objSource": obj, "compSource": comp})
}

// kotlinV5WithoutJVMNameEvidence reproduces genuine treesitter:kotlin:v5
// output for renamed functions: no name evidence, no fixed arity, and unknown
// JVM arity evidence.
type kotlinV5WithoutJVMNameEvidence struct{ *tsparser.KotlinAdapter }

func (a kotlinV5WithoutJVMNameEvidence) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:kotlin:v5", EmitsCallEdges: true}
}
func (a kotlinV5WithoutJVMNameEvidence) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	parsed, err := a.KotlinAdapter.Parse(ctx, path, content)
	renamed := map[int]struct{}{}
	for _, fact := range parsed.KotlinJVMNameEvidence {
		renamed[fact.SymbolIndex] = struct{}{}
		parsed.Symbols[fact.SymbolIndex].ArityMin, parsed.Symbols[fact.SymbolIndex].ArityMax = nil, nil
	}
	for i, fact := range parsed.KotlinJVMCallableEvidence {
		if _, ok := renamed[fact.SymbolIndex]; ok {
			parsed.KotlinJVMCallableEvidence[i] = graph.KotlinJVMCallableEvidence{SymbolIndex: fact.SymbolIndex}
		}
	}
	parsed.KotlinJVMNameEvidence = nil
	return parsed, err
}

// An existing v5 database converges on a path-scoped update: stale Kotlin
// reparses to v6 and persists name evidence, untouched Java v2 callers are
// re-decided through owner names, and nothing else reparses.
func TestKotlinV5ToV6JvmNameConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string]string{
		"Caller.java": "package app;\nimport lib.ActionsKt;\nimport lib.Service;\nclass Caller {\n    void call() { ActionsKt.execute(1); }\n    void over() { ActionsKt.oexec(1); }\n    void obj() { Service.INSTANCE.execute(1); }\n}",
		"Actions.kt":  "package lib\n@JvmName(\"execute\") fun run(x: kotlin.Int) {}\n@JvmName(\"oexec\") @JvmOverloads fun orun(a: kotlin.Int, b: kotlin.String = \"\") {}\n",
		"Service.kt":  "package lib\nobject Service {\n    @JvmName(\"execute\")\n    fun run(x: kotlin.Int) {}\n}\n",
		"Other.kt":    "package lib\nfun helper() {}",
		"main.go":     "package main\nfunc main() {}\n",
		"caller.ts":   "export function caller(): void {}\n",
	}
	for path, content := range files {
		writeProfileFile(t, filepath.Join(root, path), content)
	}
	s := newProfileStore(t)
	legacy := New(s.Store, parser.NewRegistry(tsparser.NewJava(), kotlinV5WithoutJVMNameEvidence{tsparser.NewKotlin()}, goparser.New(), tsparser.NewTypeScript()), nil)
	if _, err := legacy.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: New(s.Store, lifecycleRegistry(), nil), repoID: repo}
	var callArity sql.NullInt64
	if err := s.raw(t).QueryRowContext(ctx, `SELECT call_arity FROM edges WHERE repo_id=? AND dst_name='ActionsKt.execute'`, repo).Scan(&callArity); err != nil || !callArity.Valid || callArity.Int64 != 1 {
		t.Fatalf("legacy Java call arity = %v, %v", callArity, err)
	}
	for _, caller := range []string{"call", "over", "obj"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); got != "" {
			t.Fatalf("v5 %s -> %q, want unresolved", caller, got)
		}
	}
	if got := jvmNameFacts(t, r); got != "" {
		t.Fatalf("v5 name facts = %q", got)
	}
	summary, err := r.idx.Update(ctx, Options{RepoRoot: root, Paths: []string{"Actions.kt"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 3 || summary.FilesIndexed != 3 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("v5-to-v6 update=%+v", summary)
	}
	for path, want := range map[string]string{"Actions.kt": "treesitter:kotlin:v11", "Service.kt": "treesitter:kotlin:v11", "Other.kt": "treesitter:kotlin:v11", "Caller.java": "treesitter:java:v3", "main.go": "go-ast:go:v1", "caller.ts": "treesitter:typescript:v1"} {
		if got := fileParserProfile(t, s.raw(t), repo, path); got != want {
			t.Fatalf("%s profile=%q, want %q", path, got, want)
		}
	}
	if got := jvmNameFacts(t, r); got != "lib.Service.run=execute,lib.orun=oexec,lib.run=execute" {
		t.Fatalf("v6 name facts = %q", got)
	}
	var known, minArity, maxArity int
	if err := s.raw(t).QueryRowContext(ctx, `SELECT e.is_known,e.jvm_arity_min,e.jvm_arity_max FROM kotlin_jvm_callable_evidence e JOIN symbols s ON s.id=e.symbol_id WHERE e.repo_id=? AND s.qualified_name='lib.orun'`, repo).Scan(&known, &minArity, &maxArity); err != nil || known != 1 || minArity != 1 || maxArity != 2 {
		t.Fatalf("refreshed arity evidence = (%d,%d,%d), %v", known, minArity, maxArity, err)
	}
	for caller, target := range map[string]string{"call": "lib.run", "over": "lib.orun", "obj": "lib.Service.run"} {
		if got, _ := jvmNameCall(t, r, "app.Caller."+caller); !strings.HasPrefix(got, target+"|") {
			t.Fatalf("v6 %s -> %q, want %s", caller, got, target)
		}
		assertJVMQueryRelation(t, r, "app.Caller."+caller, target)
	}
	r.assertFreshParity(t, "Kotlin v5-to-v6 JvmName convergence")
	again, err := r.idx.Update(ctx, Options{RepoRoot: root})
	if err != nil || again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v, %v; want no-op", again, err)
	}
}
