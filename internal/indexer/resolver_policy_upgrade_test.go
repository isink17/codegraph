//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// A repository graph written before a language's resolver policy existed keeps
// that resolver's old decisions on unchanged source. These tests build such a
// graph by indexing a tree and then writing back the binding the older resolver
// made (the pub(super) glob re-export is the real one), clearing the
// policy marker, and running an ordinary update.

const (
	rustStaleCall   = "crate::c::id"
	rustStaleCaller = "src/a/y.rs"
	rustStaleWrong  = "crate::a::x::id"
)

func rustStaleTree() tree {
	return tree{
		"src/lib.rs":    "mod a; mod c;\nfn local() {}\npub fn go() {\n    local();\n}\n",
		"src/a.rs":      "pub mod x; pub mod y;",
		"src/a/x.rs":    "pub(super) fn id() -> u32 { 7 }",
		"src/a/y.rs":    "pub fn caller() {\n    crate::c::id();\n}\n",
		"src/c.rs":      "pub use crate::a::x::*;\npub use std::process::*;\n",
		"tool/main.go":  "package main\n\nfunc helper() {}\n\nfunc main() {\n\thelper()\n}\n",
		"jv/A.java":     "package jv;\npublic class A {\n  void f() { g(); }\n  void g() {}\n}\n",
		"cs/Svc.cs":     "namespace App;\npublic class Svc {\n public void F() { G(); }\n public void G() {}\n}\n",
		"Cargo.toml":    "[package]\nname = \"stale\"\n",
		"src/main.rs":   "fn main() {}\n",
		"jv/Other.java": "package jv;\npublic class Other { void g() {} }\n",
	}
}

func policyRegistry() *parser.Registry {
	return parser.NewRegistry(goparser.New(), tsparser.NewTypeScript(), tsparser.NewPython(), tsparser.NewCpp(), tsparser.NewJava(), tsparser.NewKotlin(), tsparser.NewRust(), tsparser.NewCSharp())
}

// newPolicyRepo indexes files under a staged registry: Rust only unless the
// caller names others, so a test controls exactly which languages are stale.
func newPolicyRepo(t *testing.T, files tree, policies map[string]int) *lifecycleRepo {
	t.Helper()
	if policies == nil {
		policies = map[string]int{"rust": 1}
	}
	return buildPolicyRepo(t, files, policies)
}

// newRegisteredPolicyRepo indexes under the registry this binary ships.
func newRegisteredPolicyRepo(t *testing.T, files tree) *lifecycleRepo {
	t.Helper()
	return buildPolicyRepo(t, files, nil)
}

func buildPolicyRepo(t *testing.T, files tree, policies map[string]int) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "codegraph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if policies != nil {
		s.SetResolverPolicies(policies)
	}
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: dbPath, store: s, idx: New(s, policyRegistry(), nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	r.repoID = repo.ID
	return r
}

func (r *lifecycleRepo) markers(t *testing.T) string {
	t.Helper()
	var out []string
	rows, err := r.raw(t).QueryContext(r.ctx, `SELECT key, value FROM settings WHERE key LIKE 'resolver.policy.%' ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, k+"="+v)
	}
	return strings.Join(out, ",")
}

func (r *lifecycleRepo) markerKey(language string) string {
	return fmt.Sprintf("resolver.policy.%d.%s", r.repoID, language)
}

func (r *lifecycleRepo) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := r.raw(t).ExecContext(r.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// bind writes the binding an older resolver made: the edge and the reference
// identity derived from it. target "" leaves the call unresolved.
func (r *lifecycleRepo) bind(t *testing.T, srcPath, dstName, target string) {
	t.Helper()
	var id any
	if target != "" {
		id = r.symbolID(t, target)
	}
	strategy := ""
	if target != "" {
		strategy = "exact_name"
	}
	r.exec(t, `UPDATE edges SET dst_symbol_id=?, resolution_strategy=?, resolution_confidence=? WHERE repo_id=? AND dst_name=? AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`,
		id, strategy, map[bool]string{true: "stale", false: ""}[target != ""], r.repoID, dstName, r.repoID, srcPath)
	r.exec(t, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=? AND ref_kind='call' AND (qualified_name=? OR (qualified_name='' AND name=?)) AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`,
		id, r.repoID, dstName, dstName, r.repoID, srcPath)
}

func (r *lifecycleRepo) symbolID(t *testing.T, qualified string) int64 {
	t.Helper()
	var id int64
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT id FROM symbols WHERE repo_id=? AND qualified_name=?`, r.repoID, qualified).Scan(&id); err != nil {
		t.Fatalf("symbol %s: %v", qualified, err)
	}
	return id
}

func (r *lifecycleRepo) refTarget(t *testing.T, srcPath, name string) string {
	t.Helper()
	var target sql.NullString
	err := r.raw(t).QueryRowContext(r.ctx, `SELECT (SELECT qualified_name FROM symbols WHERE id=r.symbol_id) FROM references_tbl r WHERE r.repo_id=? AND r.ref_kind='call' AND (r.qualified_name=? OR (r.qualified_name='' AND r.name=?)) AND r.file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`,
		r.repoID, name, name, r.repoID, srcPath).Scan(&target)
	if err != nil {
		t.Fatalf("reference %s:%s: %v", srcPath, name, err)
	}
	return target.String
}

func (r *lifecycleRepo) clearMarkers(t *testing.T) {
	t.Helper()
	r.exec(t, `DELETE FROM settings WHERE key LIKE 'resolver.policy.%'`)
}

// state is every table a scan could touch, id-inclusive, so "zero mutation" is
// byte-level rather than semantic.
func (r *lifecycleRepo) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id||'|'||root_path||'|'||canonical_path||'|'||created_at||'|'||updated_at FROM repos ORDER BY id`,
		`SELECT key||'='||COALESCE(value,'') FROM settings ORDER BY key`,
		`SELECT id||'|'||scan_kind||'|'||status FROM scans ORDER BY id`,
		`SELECT id||'|'||dst_name||'|'||COALESCE(dst_symbol_id,'')||'|'||resolution_strategy||'|'||resolution_confidence FROM edges ORDER BY id`,
		`SELECT id||'|'||COALESCE(symbol_id,'')||'|'||COALESCE(context_symbol_id,'') FROM references_tbl ORDER BY id`,
		`SELECT id||'|'||content_sha256||'|'||is_deleted||'|'||last_scan_id FROM files ORDER BY id`,
	} {
		rows, err := r.raw(t).QueryContext(r.ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// assertPolicyParity is assertFreshParity over the registry these tests index
// with, which carries C# as well.
func (r *lifecycleRepo) assertPolicyParity(t *testing.T, step string) {
	t.Helper()
	want := newPolicyRepo(t, r.currentTree(t), nil).projection(t)
	if diff := projectionDiff(want, r.projection(t)); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
}

// watchLanguages records, in the database, every edge or reference update made
// to a file of these languages, so a test can prove a pass never wrote them.
func (r *lifecycleRepo) watchLanguages(t *testing.T, languages ...string) {
	t.Helper()
	list := "'" + strings.Join(languages, "','") + "'"
	r.exec(t, `CREATE TABLE policy_touched(what TEXT)`)
	r.exec(t, `CREATE TRIGGER policy_touch_edges AFTER UPDATE ON edges WHEN (SELECT language FROM files WHERE id=NEW.file_id) IN (`+list+`) BEGIN INSERT INTO policy_touched VALUES('edge '||NEW.id); END`)
	r.exec(t, `CREATE TRIGGER policy_touch_refs AFTER UPDATE ON references_tbl WHEN (SELECT language FROM files WHERE id=NEW.file_id) IN (`+list+`) BEGIN INSERT INTO policy_touched VALUES('ref '||NEW.id); END`)
}

func (r *lifecycleRepo) touched(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT COUNT(*) FROM policy_touched`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func requireRustWrongEdgeGone(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	assertRustCallTarget(t, r, step, rustStaleCaller, rustStaleCall, "")
	if got := r.refTarget(t, rustStaleCaller, rustStaleCall); got != "" {
		t.Fatalf("%s: reference identity still %q", step, got)
	}
}

func TestResolverPolicyRustUpgradeRefusesOldReexportBindingOnUnchangedSource(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	requireRustWrongEdgeGone(t, r, "fresh")
	if r.markers(t) != r.markerKey("rust")+"=1" {
		t.Fatalf("fresh index markers = %q", r.markers(t))
	}

	// What the pre-#326 resolver wrote, with the marker that build never had.
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)
	if got := r.refTarget(t, rustStaleCaller, rustStaleCall); got != rustStaleWrong {
		t.Fatalf("setup: reference = %q", got)
	}
	r.watchLanguages(t, "go", "java", "csharp")

	summary := r.update(t)
	requireRustWrongEdgeGone(t, r, "upgrade update")
	if summary.FilesChanged != 0 || summary.FilesIndexed != 0 || summary.ParseMS != 0 {
		t.Fatalf("policy-only upgrade parsed source: %+v", summary)
	}
	if got := strings.Join(summary.ResolverPolicyLanguages, ","); got != "rust" || summary.ResolveMode != "resolver_policy" {
		t.Fatalf("summary languages=%q mode=%q", got, summary.ResolveMode)
	}
	if r.markers(t) != r.markerKey("rust")+"=1" {
		t.Fatalf("markers = %q", r.markers(t))
	}
	if n := r.touched(t); n != 0 {
		t.Fatalf("%d edge/reference rows of other languages were rewritten", n)
	}

	// Fresh parity, then steady state does no resolver work.
	r.assertPolicyParity(t, "after upgrade")
	edges, refs := r.edgeAndRefState(t)
	markers := r.markers(t)
	again := r.update(t)
	if len(again.ResolverPolicyLanguages) != 0 || again.ResolveMode != "none" || again.ResolverPolicyMS != 0 {
		t.Fatalf("second update did resolver work: languages=%v mode=%q", again.ResolverPolicyLanguages, again.ResolveMode)
	}
	// Scan bookkeeping aside, a current repository's update writes nothing.
	if e, f := r.edgeAndRefState(t); e != edges || f != refs || r.markers(t) != markers {
		t.Fatal("steady-state update changed the graph")
	}
}

func TestResolverPolicyRustUpgradeRebindsWhatTheOldResolverRefused(t *testing.T) {
	files := tree{
		"src/lib.rs":  "mod a;",
		"src/a.rs":    "pub mod x; pub mod y;\npub use x::*;\n",
		"src/a/x.rs":  "pub(super) fn id() -> u32 { 7 }",
		"src/a/y.rs":  "pub fn caller() {\n    crate::a::id();\n}\n",
		"src/main.rs": "fn main() {}\n",
	}
	r := newPolicyRepo(t, files, nil)
	assertRustCallTarget(t, r, "fresh", "src/a/y.rs", "crate::a::id", "src/a/x.rs:crate::a::x::id")
	r.bind(t, "src/a/y.rs", "crate::a::id", "")
	r.clearMarkers(t)
	assertRustCallTarget(t, r, "setup", "src/a/y.rs", "crate::a::id", "")

	r.update(t)
	assertRustCallTarget(t, r, "upgrade", "src/a/y.rs", "crate::a::id", "src/a/x.rs:crate::a::x::id")
	if got := r.refTarget(t, "src/a/y.rs", "crate::a::id"); got != "crate::a::x::id" {
		t.Fatalf("reference identity = %q", got)
	}
	r.assertPolicyParity(t, "after upgrade")
}

// A policy change in another language goes through the same path: a staged
// Java and C# policy re-decides only that language's edges.
func TestResolverPolicyRepresentativeJavaAndCSharpUpgrades(t *testing.T) {
	for _, tc := range []struct {
		language, src, call, right, wrong string
	}{
		{"java", "jv/A.java", "g", "jv.A.g", "jv.Other.g"},
		{"csharp", "cs/Svc.cs", "G", "App.Svc.G", "App.Svc.F"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			r := newPolicyRepo(t, rustStaleTree(), map[string]int{"rust": 1, "java": 1, "csharp": 1})
			if r.markers(t) != strings.Join([]string{r.markerKey("csharp") + "=1", r.markerKey("java") + "=1", r.markerKey("rust") + "=1"}, ",") {
				t.Fatalf("markers = %q", r.markers(t))
			}
			want := r.edgeState(t, tc.src, tc.call)
			if !strings.Contains(want, tc.right+"(") {
				t.Fatalf("fresh %s edge = %s", tc.language, want)
			}
			r.bind(t, tc.src, tc.call, tc.wrong)
			r.exec(t, `DELETE FROM settings WHERE key=?`, r.markerKey(tc.language))
			// Another language's marker survives and must not be redecided.
			r.watchLanguages(t, "go", "rust")

			summary := r.update(t)
			if got := strings.Join(summary.ResolverPolicyLanguages, ","); got != tc.language {
				t.Fatalf("redecided %q, want %q", got, tc.language)
			}
			if got := r.edgeState(t, tc.src, tc.call); got != want {
				t.Fatalf("after upgrade: %s, want %s", got, want)
			}
			if n := r.touched(t); n != 0 {
				t.Fatalf("%d rows of languages with a current policy were rewritten", n)
			}
			r.assertPolicyParity(t, "after "+tc.language+" upgrade")
		})
	}
}

func TestResolverPolicyUnsupportedMarkerRefusesWithZeroMutation(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
		unreadable       bool
	}{
		{"newer", "rust", "2", false},
		{"malformed text", "rust", "abc", true},
		{"zero", "rust", "0", true},
		{"non canonical", "rust", "01", true},
		{"empty", "rust", "", true},
		{"unknown language", "cobol", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPolicyRepo(t, rustStaleTree(), nil)
			r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.markerKey(tc.key), tc.value)
			// Source changed too, so a non-refusing run would have plenty to do.
			r.write(t, "src/a/y.rs", "pub fn caller() {\n    crate::c::id();\n    crate::c::id();\n}\n")
			before := r.state(t)
			_, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
			var pe *store.ResolverPolicyError
			if !errors.As(err, &pe) || pe.Language != tc.key {
				t.Fatalf("err = %v", err)
			}
			if tc.unreadable != errors.Is(err, store.ErrResolverPolicyUnreadable) || tc.unreadable == errors.Is(err, store.ErrResolverPolicyNewer) {
				t.Fatalf("wrong reason: %v", err)
			}
			if after := r.state(t); after != before {
				t.Fatalf("refused run mutated the database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			// A scan that cannot touch the language is not stalled by it.
			if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Languages: []string{"go"}}); err != nil {
				t.Fatalf("go-only update: %v", err)
			}
		})
	}
}

func TestResolverPolicyParserDowngradeStaysRefusedBeforeMutation(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.clearMarkers(t)
	// A Rust adapter that emits no call edges while the graph holds call edges.
	noCalls := parser.NewRegistry(goparser.New(), symbolsOnly("rust", ".rs", "heuristic:rust:v1"))
	before := r.state(t)
	_, err := New(r.store, noCalls, nil).Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
	if !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("err = %v, want parser downgrade refusal", err)
	}
	if after := r.state(t); after != before {
		t.Fatal("refused downgrade mutated the database")
	}
}

func TestResolverPolicyFailureRollsBackEdgesReferencesAndMarker(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"reference reconciliation", `CREATE TRIGGER fail_refs BEFORE UPDATE ON references_tbl BEGIN SELECT RAISE(ABORT, 'injected reference failure'); END`},
		{"marker stamp", `CREATE TRIGGER fail_marker BEFORE INSERT ON settings WHEN NEW.key LIKE 'resolver.policy.%' BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`},
		{"rebind", `CREATE TRIGGER fail_rebind BEFORE UPDATE OF dst_symbol_id ON edges WHEN NEW.dst_symbol_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected rebind failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPolicyRepo(t, rustStaleTree(), nil)
			r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
			r.clearMarkers(t)
			r.exec(t, tc.trigger)
			r.exec(t, `DROP TRIGGER IF EXISTS never`)
			edgesBefore, refsBefore := r.edgeAndRefState(t)
			_, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("err = %v", err)
			}
			edgesAfter, refsAfter := r.edgeAndRefState(t)
			if edgesAfter != edgesBefore || refsAfter != refsBefore {
				t.Fatal("failed redecision left partial edge or reference changes")
			}
			if r.markers(t) != "" {
				t.Fatalf("marker stamped after failure: %q", r.markers(t))
			}
			// The failure is retried, and converges, once the fault is gone.
			r.exec(t, `DROP TRIGGER IF EXISTS fail_refs`)
			r.exec(t, `DROP TRIGGER IF EXISTS fail_marker`)
			r.exec(t, `DROP TRIGGER IF EXISTS fail_rebind`)
			r.update(t)
			requireRustWrongEdgeGone(t, r, "retry")
			r.assertPolicyParity(t, "retry")
		})
	}
}

func (r *lifecycleRepo) edgeAndRefState(t *testing.T) (string, string) {
	t.Helper()
	var edges, refs []string
	for _, q := range []struct {
		dst  *[]string
		sqlq string
	}{
		{&edges, `SELECT id||'|'||COALESCE(dst_symbol_id,'')||'|'||resolution_strategy FROM edges ORDER BY id`},
		{&refs, `SELECT id||'|'||COALESCE(symbol_id,'') FROM references_tbl ORDER BY id`},
	} {
		rows, err := r.raw(t).QueryContext(r.ctx, q.sqlq)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			*q.dst = append(*q.dst, line)
		}
		rows.Close()
	}
	return strings.Join(edges, "\n"), strings.Join(refs, "\n")
}

func TestResolverPolicyFilteredAndPathScopedRunsDoNotStampStaleLanguages(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)

	for _, opts := range []Options{
		{RepoRoot: r.root, ScanKind: "update", Languages: []string{"go"}},
		{RepoRoot: r.root, ScanKind: "update", Paths: []string{"tool/main.go"}},
	} {
		if _, err := r.idx.Update(r.ctx, opts); err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		if r.markers(t) != "" {
			t.Fatalf("%+v stamped a language it could not decide: %q", opts, r.markers(t))
		}
		if got := r.refTarget(t, rustStaleCaller, rustStaleCall); got != rustStaleWrong {
			t.Fatalf("%+v touched Rust: %q", opts, got)
		}
	}

	// A path-scoped request for one Rust file decides the whole language, since
	// the policy is the language's and not the file's.
	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"src/main.rs"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(summary.ResolverPolicyLanguages, ",") != "rust" {
		t.Fatalf("languages = %v", summary.ResolverPolicyLanguages)
	}
	requireRustWrongEdgeGone(t, r, "path-scoped rust")
	if r.markers(t) != r.markerKey("rust")+"=1" {
		t.Fatalf("markers = %q", r.markers(t))
	}
}

func TestResolverPolicyIsPerRepository(t *testing.T) {
	a := newPolicyRepo(t, rustStaleTree(), nil)
	// A second repository in the same database.
	rootB := t.TempDir()
	b := &lifecycleRepo{ctx: a.ctx, root: rootB, dbPath: a.dbPath, store: a.store, idx: a.idx}
	for rel, content := range rustStaleTree() {
		b.write(t, rel, content)
	}
	if _, err := a.idx.Index(a.ctx, Options{RepoRoot: rootB, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	repoB, err := a.store.UpsertRepo(a.ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	b.repoID = repoB.ID
	if a.repoID == b.repoID {
		t.Fatal("same repo id")
	}

	a.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	b.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	a.exec(t, `DELETE FROM settings WHERE key=?`, a.markerKey("rust"))

	if _, err := a.idx.Update(a.ctx, Options{RepoRoot: a.root, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	}
	requireRustWrongEdgeGone(t, a, "repo A")
	if got := b.refTarget(t, rustStaleCaller, rustStaleCall); got != rustStaleWrong {
		t.Fatalf("repo B reference changed by repo A's upgrade: %q", got)
	}
	if got := a.markers(t); !strings.Contains(got, b.markerKey("rust")+"=1") || !strings.Contains(got, a.markerKey("rust")+"=1") {
		t.Fatalf("markers = %q", got)
	}

	// B's own marker is what decides B.
	b.exec(t, `DELETE FROM settings WHERE key=?`, b.markerKey("rust"))
	if _, err := a.idx.Update(a.ctx, Options{RepoRoot: rootB, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	}
	requireRustWrongEdgeGone(t, b, "repo B")
}

func TestResolverPolicyFullIndexRecordsPolicyWithoutSecondPass(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)
	summary, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ResolveMode != "repo" || strings.Join(summary.ResolverPolicyLanguages, ",") != "rust" {
		t.Fatalf("mode=%q languages=%v", summary.ResolveMode, summary.ResolverPolicyLanguages)
	}
	requireRustWrongEdgeGone(t, r, "forced index")
	if r.markers(t) != r.markerKey("rust")+"=1" {
		t.Fatalf("markers = %q", r.markers(t))
	}
}
