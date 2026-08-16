//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// cppRepo is a C/C++ repository under test whose files can be rewritten and
// re-indexed, so a fixture can compare a fresh index against any update history.
type cppRepo struct {
	t      *testing.T
	root   string
	dbPath string
	store  *store.Store
	idx    *Indexer
	repo   graph.Repo
}

func newCppRepo(t *testing.T) *cppRepo {
	t.Helper()
	r := &cppRepo{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.sqlite")}
	s, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	r.store = s
	r.idx = New(s, parser.NewRegistry(tsparser.NewCpp()), nil)
	return r
}

func (r *cppRepo) write(name, src string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, name), []byte(src), 0o644); err != nil {
		r.t.Fatalf("WriteFile(%s) error = %v", name, err)
	}
}

func (r *cppRepo) remove(name string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.root, name)); err != nil {
		r.t.Fatalf("Remove(%s) error = %v", name, err)
	}
}

func (r *cppRepo) run(scanKind string) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.idx.Index(ctx, Options{RepoRoot: r.root, ScanKind: scanKind}); err != nil {
		r.t.Fatalf("Index(%s) error = %v", scanKind, err)
	}
	repo, err := r.store.UpsertRepo(ctx, r.root)
	if err != nil {
		r.t.Fatalf("UpsertRepo() error = %v", err)
	}
	r.repo = repo
}

// projection is the semantic call-graph projection: no row ids, no timestamps.
// Two indexes of the same tree must produce the same one however they got there.
func (r *cppRepo) projection() []string {
	r.t.Helper()
	r.store.Close()
	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		r.t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() {
		db.Close()
		s, err := store.Open(r.dbPath)
		if err != nil {
			r.t.Fatalf("store.Open() reopen error = %v", err)
		}
		r.store = s
		r.idx = New(s, parser.NewRegistry(tsparser.NewCpp()), nil)
	}()

	rows, err := db.Query(`
		SELECT f.path, e.line, e.dst_name, e.evidence,
		       COALESCE(src.qualified_name, ''),
		       COALESCE(dst.qualified_name, '<unresolved>'),
		       COALESCE(e.resolution_strategy, ''), COALESCE(e.resolution_confidence, '')
		FROM edges e
		JOIN files f ON f.id = e.file_id
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		LEFT JOIN symbols dst ON dst.id = e.dst_symbol_id
		WHERE e.edge_kind = 'calls'`)
	if err != nil {
		r.t.Fatalf("projection query error = %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path, dstName, evidence, src, dst, strategy, confidence string
		var line int
		if err := rows.Scan(&path, &line, &dstName, &evidence, &src, &dst, &strategy, &confidence); err != nil {
			r.t.Fatalf("projection scan error = %v", err)
		}
		out = append(out, fmt.Sprintf("%s:%d %s [%s] %s -> %s (%s/%s)",
			path, line, dstName, evidence, src, dst, strategy, confidence))
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("projection rows error = %v", err)
	}
	sort.Strings(out)
	return out
}

func (r *cppRepo) unresolved(dstName string) int {
	r.t.Helper()
	n, err := r.store.CountUnresolvedEdgesByDstName(context.Background(), r.repo.ID, dstName)
	if err != nil {
		r.t.Fatalf("CountUnresolvedEdgesByDstName(%s) error = %v", dstName, err)
	}
	return n
}

func (r *cppRepo) callers(symbol string) []string {
	r.t.Helper()
	found, err := r.store.FindCallers(context.Background(), r.repo.ID, symbol, 0, 50, 0)
	if err != nil {
		r.t.Fatalf("FindCallers(%s) error = %v", symbol, err)
	}
	out := make([]string, 0, len(found))
	for _, sym := range found {
		out = append(out, sym.QualifiedName)
	}
	sort.Strings(out)
	return out
}

// --- fixtures ---------------------------------------------------------------

const cppSizeCaller = `void caller(A* a, B* b) {
    a->size();
    b->size();
}
`

// TestCppUnknownReceiverIgnoresGlobalUniqueness is the uniqueness/lifecycle
// control (P22.11): a variable receiver is not type evidence, so a member call
// must not bind a method merely because that method's name became globally
// unique. The binding must be refused with three candidates, with one, and with
// none -- the answer cannot depend on how many other classes happen to declare
// the same member.
func TestCppUnknownReceiverIgnoresGlobalUniqueness(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.hpp", "struct A { int size() { return 1; } };\n")
	r.write("b.hpp", "struct B { int size() { return 2; } };\n")
	r.write("c.hpp", "struct C { int size() { return 3; } };\n")
	r.write("caller.cpp", cppSizeCaller)
	r.run("index")

	for _, dst := range []string{"a.size", "b.size"} {
		if got := r.unresolved(dst); got != 1 {
			t.Fatalf("with 3 candidates: unresolved %q = %d, want 1", dst, got)
		}
	}

	// Delete two of the three declarations: `C::size` is now the only `size` in
	// the repository. Global uniqueness is still not receiver-type evidence.
	r.remove("a.hpp")
	r.remove("b.hpp")
	r.run("update")
	for _, dst := range []string{"a.size", "b.size"} {
		if got := r.unresolved(dst); got != 1 {
			t.Fatalf("with 1 candidate: unresolved %q = %d, want 1", dst, got)
		}
	}
	if got := r.callers("C::size"); len(got) != 0 {
		t.Fatalf("FindCallers(C::size) = %v, want none", got)
	}

	// Re-resolve the caller through the incremental binder while `C::size` is the
	// only candidate: the path-scoped pipeline is a second implementation of the
	// same policy (P22.8) and must refuse the same binding the repo-wide one does.
	r.write("caller.cpp", cppSizeCaller+"\n// touched\n")
	r.run("update")
	for _, dst := range []string{"a.size", "b.size"} {
		if got := r.unresolved(dst); got != 1 {
			t.Fatalf("after incremental re-resolve: unresolved %q = %d, want 1", dst, got)
		}
	}
	if got := r.callers("C::size"); len(got) != 0 {
		t.Fatalf("after incremental re-resolve: FindCallers(C::size) = %v, want none", got)
	}

	// And with no candidate left at all.
	r.remove("c.hpp")
	r.run("update")
	for _, dst := range []string{"a.size", "b.size"} {
		if got := r.unresolved(dst); got != 1 {
			t.Fatalf("with 0 candidates: unresolved %q = %d, want 1", dst, got)
		}
	}
}

// TestCppUnknownReceiverInsertionOrder pins that the refusal does not depend on
// which file was indexed first, second, or in the same batch.
func TestCppUnknownReceiverInsertionOrder(t *testing.T) {
	histories := []struct {
		name  string
		build func(r *cppRepo)
	}{
		{"fresh", func(r *cppRepo) {
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.write("b.hpp", "struct B { int size() { return 2; } };\n")
			r.write("caller.cpp", cppSizeCaller)
			r.run("index")
		}},
		{"callerFirst", func(r *cppRepo) {
			r.write("caller.cpp", cppSizeCaller)
			r.run("index")
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.run("update")
			r.write("b.hpp", "struct B { int size() { return 2; } };\n")
			r.run("update")
		}},
		{"targetsFirst", func(r *cppRepo) {
			r.write("b.hpp", "struct B { int size() { return 2; } };\n")
			r.run("index")
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.run("update")
			r.write("caller.cpp", cppSizeCaller)
			r.run("update")
		}},
		{"competitorAddedThenRemoved", func(r *cppRepo) {
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.write("caller.cpp", cppSizeCaller)
			r.run("index")
			r.write("b.hpp", "struct B { int size() { return 2; } };\n")
			r.run("update")
			r.write("z.hpp", "struct Z { int size() { return 9; } };\n")
			r.run("update")
			r.remove("z.hpp")
			r.run("update")
		}},
		{"targetRecreated", func(r *cppRepo) {
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.write("b.hpp", "struct B { int size() { return 2; } };\n")
			r.write("caller.cpp", cppSizeCaller)
			r.run("index")
			r.remove("a.hpp")
			r.run("update")
			r.write("a.hpp", "struct A { int size() { return 1; } };\n")
			r.run("update")
		}},
	}

	var want []string
	for _, h := range histories {
		r := newCppRepo(t)
		h.build(r)
		got := r.projection()
		if want == nil {
			want = got
			continue
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("history %q projection differs from fresh:\ngot:\n%s\nwant:\n%s",
				h.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	for _, line := range want {
		if strings.Contains(line, "a.size") && !strings.Contains(line, "<unresolved>") {
			t.Fatalf("unknown receiver bound a target: %s", line)
		}
	}
}

// TestCppExternalReceiverDoesNotBindProjectMember is the stdlib/external
// negative corpus: nothing in the graph identifies the type of `v` or `s`, so a
// project member of the same name must not be claimed. This is the class that
// produced `expected_message.size() -> MinimalistCustomType::size` in
// googletest.
func TestCppExternalReceiverDoesNotBindProjectMember(t *testing.T) {
	r := newCppRepo(t)
	r.write("project.hpp", `struct ProjectType {
    int size() { return 0; }
    bool empty() { return true; }
    int begin() { return 0; }
    int end() { return 0; }
    int get() { return 0; }
    int data() { return 0; }
};
`)
	r.write("user.cpp", `#include <string>
#include <vector>
void user() {
    std::vector<int> v;
    v.size();
    v.begin();
    v.end();
    v.data();
    std::string s;
    s.empty();
    s.size();
}
`)
	r.run("index")

	for _, member := range []string{"size", "empty", "begin", "end", "get", "data"} {
		if got := r.callers("ProjectType::" + member); len(got) != 0 {
			t.Fatalf("FindCallers(ProjectType::%s) = %v, want none (external receiver)", member, got)
		}
	}
	for _, dst := range []string{"v.size", "v.begin", "v.end", "v.data", "s.empty", "s.size"} {
		if got := r.unresolved(dst); got != 1 {
			t.Fatalf("unresolved %q = %d, want 1", dst, got)
		}
	}
}

// TestCppQualifiedCallFullUpdateParity pins the evidence-backed half over the
// P22.8 lifecycle matrix: fresh, caller first, target first, and each side
// edited, all converge on the same projection.
func TestCppQualifiedCallFullUpdateParity(t *testing.T) {
	const target = "struct Buf {\n    int size();\n};\nint Buf::size() { return 1; }\n"
	const targetEdited = "struct Buf {\n    int size();\n};\nint Buf::size() { return 2; }\n"
	const caller = "void use() {\n    Buf::size();\n}\n"
	const callerEdited = "void use() {\n    Buf::size();\n}\nvoid use2() {\n    Buf::size();\n}\n"

	histories := []struct {
		name  string
		build func(r *cppRepo)
	}{
		{"fresh", func(r *cppRepo) {
			r.write("buf.hpp", targetEdited)
			r.write("use.cpp", callerEdited)
			r.run("index")
		}},
		{"callerFirst", func(r *cppRepo) {
			r.write("use.cpp", callerEdited)
			r.run("index")
			r.write("buf.hpp", targetEdited)
			r.run("update")
		}},
		{"targetFirst", func(r *cppRepo) {
			r.write("buf.hpp", targetEdited)
			r.run("index")
			r.write("use.cpp", callerEdited)
			r.run("update")
		}},
		{"callerChanged", func(r *cppRepo) {
			r.write("buf.hpp", targetEdited)
			r.write("use.cpp", caller)
			r.run("index")
			r.write("use.cpp", callerEdited)
			r.run("update")
		}},
		{"targetChanged", func(r *cppRepo) {
			r.write("buf.hpp", target)
			r.write("use.cpp", callerEdited)
			r.run("index")
			r.write("buf.hpp", targetEdited)
			r.run("update")
		}},
		{"targetDeletedAndRecreated", func(r *cppRepo) {
			r.write("buf.hpp", targetEdited)
			r.write("use.cpp", callerEdited)
			r.run("index")
			r.remove("buf.hpp")
			r.run("update")
			r.write("buf.hpp", targetEdited)
			r.run("update")
		}},
	}

	var want []string
	for _, h := range histories {
		r := newCppRepo(t)
		h.build(r)
		got := r.projection()
		if want == nil {
			want = got
			bound := 0
			for _, line := range want {
				if strings.Contains(line, "Buf::size ") || strings.Contains(line, "-> Buf::size") {
					bound++
				}
			}
			if bound == 0 {
				t.Fatalf("fresh projection bound no qualified call: %s", strings.Join(want, "\n"))
			}
			continue
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("history %q projection differs from fresh:\ngot:\n%s\nwant:\n%s",
				h.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// TestCppLegacyDatabaseLosesReceiverDiscardedBindings is the upgrade contract
// (P22.11). A database written by an older release holds the lossy bare-tail
// destination and the binding it produced; the parser fix alone would only
// reach files that happen to change afterwards, so the false relation would
// survive indefinitely on an existing index. One normal `update` run with the
// new binary must remove it, and the run after that must change nothing.
func TestCppLegacyDatabaseLosesReceiverDiscardedBindings(t *testing.T) {
	r := newCppRepo(t)
	r.write("project.hpp", "struct ProjectType { int size() { return 0; } };\n")
	r.write("user.cpp", "#include <vector>\nvoid user() {\n    std::vector<int> v;\n    v.size();\n}\n")
	r.run("index")

	fresh := r.projection()

	// Rewrite the graph into the shape an older release persisted: the receiver
	// discarded from `dst_name`, the edge bound to the project's own `size`.
	legacyDowngrade(t, r.dbPath)
	clearCppUpgradeMarker(t, r.dbPath)
	if got := legacyBoundTargets(t, r.dbPath); got != 1 {
		t.Fatalf("legacy downgrade left %d bound edges, want 1", got)
	}

	r.run("update")
	if got := legacyBoundTargets(t, r.dbPath); got != 0 {
		t.Fatalf("after upgrade update: %d receiver-discarded bindings remain, want 0", got)
	}
	upgraded := r.projection()
	if strings.Join(upgraded, "\n") != strings.Join(fresh, "\n") {
		t.Fatalf("upgraded projection differs from fresh:\ngot:\n%s\nwant:\n%s",
			strings.Join(upgraded, "\n"), strings.Join(fresh, "\n"))
	}

	r.run("update")
	again := r.projection()
	if strings.Join(again, "\n") != strings.Join(upgraded, "\n") {
		t.Fatalf("second upgrade run was not idempotent:\ngot:\n%s\nwant:\n%s",
			strings.Join(again, "\n"), strings.Join(upgraded, "\n"))
	}
}

// legacyDowngrade rewrites a current database into the pre-P22.11 shape: member
// call destinations lose their receiver and bind the project method whose bare
// name matches.
func legacyDowngrade(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		UPDATE edges
		SET dst_name = 'size',
		    dst_symbol_id = (SELECT id FROM symbols WHERE qualified_name = 'ProjectType::size'),
		    resolution_strategy = 'exact_name',
		    resolution_confidence = 'high'
		WHERE edge_kind = 'calls' AND dst_name = 'v.size'`); err != nil {
		t.Fatalf("legacy downgrade error = %v", err)
	}
}

func legacyBoundTargets(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM edges e JOIN symbols s ON s.id = e.dst_symbol_id
		WHERE e.edge_kind = 'calls' AND s.qualified_name = 'ProjectType::size'`).Scan(&n); err != nil {
		t.Fatalf("count error = %v", err)
	}
	return n
}

// TestCppQualifiedCallAmbiguityFailsClosed: an explicit `Type::method` spelling
// is strong evidence, but only while it names one thing. Two declarations of
// the same qualified identity (two headers each declaring `struct Dup`) leave
// the call undecidable at CodeGraph's model level -- signatures are not part of
// a symbol identity here -- and undecidable means unresolved, never the first
// row.
func TestCppQualifiedCallAmbiguityFailsClosed(t *testing.T) {
	r := newCppRepo(t)
	r.write("one.cpp", "struct Dup { int run(); };\nint Dup::run() { return 1; }\n")
	r.write("two.cpp", "struct Dup { int run(int); };\nint Dup::run(int n) { return n; }\n")
	r.write("call.cpp", "void call() {\n    Dup::run();\n}\n")
	r.run("index")

	if got := r.unresolved("Dup::run"); got != 1 {
		t.Fatalf("ambiguous Dup::run unresolved = %d, want 1", got)
	}

	// Insertion order cannot decide it either.
	swapped := newCppRepo(t)
	swapped.write("two.cpp", "struct Dup { int run(int); };\nint Dup::run(int n) { return n; }\n")
	swapped.run("index")
	swapped.write("one.cpp", "struct Dup { int run(); };\nint Dup::run() { return 1; }\n")
	swapped.write("call.cpp", "void call() {\n    Dup::run();\n}\n")
	swapped.run("update")
	if got := swapped.unresolved("Dup::run"); got != 1 {
		t.Fatalf("ambiguous Dup::run (other order) unresolved = %d, want 1", got)
	}

	// With one declaration the same spelling is evidence and does bind.
	unique := newCppRepo(t)
	unique.write("one.cpp", "struct Dup { int run(); };\nint Dup::run() { return 1; }\n")
	unique.write("call.cpp", "void call() {\n    Dup::run();\n}\n")
	unique.run("index")
	if got := unique.unresolved("Dup::run"); got != 0 {
		t.Fatalf("unique Dup::run unresolved = %d, want 0", got)
	}
}

// TestCppSameClassBareCallUnchanged is the P22.11 scope boundary (section 10):
// `foo()` inside `A::caller` is not receiver-discard syntax and its handling is
// the repository-wide bare-name rule every language shares. Preserving
// receivers must not change it.
func TestCppSameClassBareCallUnchanged(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void helper() {}\n    void caller();\n};\nvoid A::caller() {\n    helper();\n}\n")
	r.run("index")
	if got := r.unresolved("helper"); got != 0 {
		t.Fatalf("bare same-class call unresolved = %d, want 0 (bare-name rule unchanged)", got)
	}
	if got := r.callers("A::helper"); len(got) != 1 || got[0] != "A::caller" {
		t.Fatalf("FindCallers(A::helper) = %v, want [A::caller]", got)
	}
}

// TestCppThisReceiverStaysUnresolved records the deliberate recall decision.
// `this->foo()` could in principle be answered from the calling symbol's own
// container, but the adapter does not track the enclosing class at call
// extraction and inferring one would be a guess in exactly the cases that
// matter (templates, nested classes, inherited members). Measured cost: 38 such
// calls in fmt and 5 in googletest. Fails closed rather than guesses.
func TestCppThisReceiverStaysUnresolved(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void helper() {}\n    void caller() { this->helper(); }\n};\n")
	r.run("index")
	if got := r.unresolved("this.helper"); got != 1 {
		t.Fatalf("this->helper unresolved = %d, want 1", got)
	}
	if got := r.callers("A::helper"); len(got) != 0 {
		t.Fatalf("FindCallers(A::helper) = %v, want none", got)
	}
}

// TestCppUpgradeSkippedWithoutCallCapableAdapter guards the non-cgo build. The
// heuristic C/C++ adapter emits symbols but no call edges, so forcing the
// P22.11 reparse there would delete a C++ call graph a cgo build had produced
// rather than rebuild it. The mark must not fire, and the graph must survive an
// ordinary update run.
func TestCppUpgradeSkippedWithoutCallCapableAdapter(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A { int size() { return 1; } };\nvoid caller(A* a) {\n    a->size();\n}\n")
	r.run("index")
	before := r.projection()
	if len(before) == 0 {
		t.Fatalf("fixture produced no call edges")
	}

	// Make the database look like one an older release wrote, so the upgrade is
	// still pending, then swap in the registry a non-cgo build would use.
	clearCppUpgradeMarker(t, r.dbPath)
	r.idx = New(r.store, parser.NewRegistry(heuristicparser.NewCAndCpp()), nil)
	r.run("update")
	after := r.projection()
	if strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("heuristic-adapter update changed the C++ call graph:\ngot:\n%s\nwant:\n%s",
			strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
}

// clearCppUpgradeMarker makes a database look like one written before P22.11,
// with the one-time C/C++ reparse still pending.
func clearCppUpgradeMarker(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM settings WHERE key LIKE 'parser.cpp_receiver_reparsed%'`); err != nil {
		t.Fatalf("clear upgrade marker error = %v", err)
	}
}

// TestCppUpgradeSkippedWhenLanguageExcluded guards the `languages` allowlist.
// processFileTask stops at the allowlist check before it ever reads the file,
// so metadata cleared for a language the run will not parse would never be
// rewritten: the row would keep the sentinel mtime and the empty hash for the
// life of the index, and `file_state` would report them.
func TestCppUpgradeSkippedWhenLanguageExcluded(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A { int size() { return 1; } };\n")
	r.run("index")
	clearCppUpgradeMarker(t, r.dbPath)

	ctx := context.Background()
	if _, err := r.idx.Index(ctx, Options{RepoRoot: r.root, ScanKind: "update", Languages: []string{"go"}}); err != nil {
		t.Fatalf("Index(languages=[go]) error = %v", err)
	}

	mtime, hash := cppFileMeta(t, r.dbPath, "a.cpp")
	if mtime < 0 || hash == "" {
		t.Fatalf("a.cpp metadata = (%d, %q); a run that excludes cpp must not clear it", mtime, hash)
	}
}

func cppFileMeta(t *testing.T, dbPath, path string) (int64, string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	var mtime int64
	var hash string
	if err := db.QueryRow(`SELECT mtime_unix_ns, content_sha256 FROM files WHERE path = ?`, path).Scan(&mtime, &hash); err != nil {
		t.Fatalf("file meta error = %v", err)
	}
	return mtime, hash
}
