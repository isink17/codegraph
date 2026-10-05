//go:build cgo

package indexer

import (
	"errors"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func (r *lifecycleRepo) repoUpdatedAt(t *testing.T) string {
	t.Helper()
	var got string
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT updated_at FROM repos`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A refused scan must not even bump the repository row: root_path and
// updated_at are written by UpsertRepo, which now runs only after every plan
// has passed.
func TestResolverPolicyRefusalLeavesRepositoryRowUnchanged(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.exec(t, `UPDATE repos SET updated_at='sentinel'`)
	r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.markerKey("rust"), "2")
	before := r.state(t)
	_, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
	var pe *store.ResolverPolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if got := r.repoUpdatedAt(t); got != "sentinel" {
		t.Fatalf("refused run wrote repos.updated_at = %q", got)
	}
	if after := r.state(t); after != before {
		t.Fatal("refused run mutated the database")
	}
}

// A repository the database has not seen has no marker to refuse, so it is
// created and indexed normally.
func TestResolverPolicyUnknownRepositoryIsCreated(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	root := t.TempDir()
	other := &lifecycleRepo{ctx: r.ctx, root: root, dbPath: r.dbPath, store: r.store, idx: r.idx}
	other.write(t, "src/lib.rs", "pub fn f() {}\n")
	if _, err := r.idx.Index(r.ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.raw(t).QueryRowContext(r.ctx, `SELECT COUNT(*) FROM repos`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("repos = %d, %v", n, err)
	}
}

// An older resolver left a call unresolved in another language. A redecision of
// Rust, or of a language whose marker says a newer binary decided it, must not
// write that language: neither the edge nor its reference identity.
func TestResolverPolicyRedecisionDoesNotWriteOtherLanguages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policies map[string]int
		goMarker string
		langs    []string
	}{
		{"go without a policy", nil, "", nil},
		{"go decided by a newer binary and filtered out", map[string]int{"rust": 1, "go": 1}, "2", []string{"rust"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPolicyRepo(t, rustStaleTree(), tc.policies)
			r.clearMarkers(t)
			if tc.goMarker != "" {
				r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?)`, r.markerKey("go"), tc.goMarker)
			}
			r.exec(t, `UPDATE edges SET dst_symbol_id=NULL, resolution_strategy='', resolution_confidence='' WHERE dst_name='helper'`)
			r.exec(t, `UPDATE references_tbl SET symbol_id=NULL WHERE name='helper'`)
			r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
			r.watchLanguages(t, "go")
			if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Languages: tc.langs}); err != nil {
				t.Fatal(err)
			}
			requireRustWrongEdgeGone(t, r, "rust redecided")
			if n := r.touched(t); n != 0 {
				t.Fatalf("Rust pass wrote %d Go rows; helper reference now %q", n, r.refTarget(t, "tool/main.go", "helper"))
			}
		})
	}
}

// The shipped registry covers every language with a resolver, so a graph
// written before policies existed is decided once per language, and a staged
// bump of one language leaves the rest alone.
func TestResolverPolicyShippedRegistryUpgradesEveryLanguage(t *testing.T) {
	r := newRegisteredPolicyRepo(t, rustStaleTree())
	for _, language := range []string{"cpp", "csharp", "go", "java", "kotlin", "php", "python", "ruby", "rust", "swift", "typescript"} {
		if !strings.Contains(r.markers(t), r.markerKey(language)+"=") {
			t.Fatalf("fresh index did not record %s: %q", language, r.markers(t))
		}
	}
	want := r.projection(t)
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.bind(t, "jv/A.java", "g", "jv.Other.g")
	r.bind(t, "cs/Svc.cs", "G", "App.Svc.F")
	r.clearMarkers(t)

	summary := r.update(t)
	if summary.FilesIndexed != 0 || summary.ParseMS != 0 {
		t.Fatalf("policy upgrade parsed source: %+v", summary)
	}
	for _, language := range []string{"csharp", "java", "rust"} {
		if !strings.Contains(strings.Join(summary.ResolverPolicyLanguages, ","), language) {
			t.Fatalf("languages = %v", summary.ResolverPolicyLanguages)
		}
	}
	if diff := projectionDiff(want, r.projection(t)); diff != "" {
		t.Fatalf("upgrade diverges from the fresh graph:\n%s", diff)
	}
	if again := r.update(t); again.ResolveMode != "none" || len(again.ResolverPolicyLanguages) != 0 {
		t.Fatalf("second update: mode=%q languages=%v", again.ResolveMode, again.ResolverPolicyLanguages)
	}
}

// A newer binary stamps the language after this scan was planned. The marker is
// re-read in the transaction that would certify the edges: the transaction fails
// closed and nothing it wrote survives.
func TestResolverPolicyConcurrentNewerStampRollsBack(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)
	// The first rebind of a Rust edge inside the redecision installs the newer
	// marker, as a concurrent binary's committed stamp would be seen.
	r.exec(t, `CREATE TRIGGER newer_binary AFTER UPDATE OF resolution_strategy ON edges
		WHEN NEW.resolution_strategy <> '' BEGIN
		INSERT INTO settings(key,value) VALUES('`+r.markerKey("rust")+`','2') ON CONFLICT(key) DO UPDATE SET value='2'; END`)
	edges, refs := r.edgeAndRefState(t)
	_, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
	var pe *store.ResolverPolicyError
	if !errors.As(err, &pe) || !errors.Is(err, store.ErrResolverPolicyNewer) {
		t.Fatalf("err = %v", err)
	}
	r.exec(t, `DROP TRIGGER newer_binary`)
	if e, f := r.edgeAndRefState(t); e != edges || f != refs {
		t.Fatal("failed redecision left edge or reference changes")
	}
	if r.markers(t) != "" {
		t.Fatalf("markers = %q", r.markers(t))
	}
}

// A forced full index with stale markers records them in the resolve
// transaction and parity holds; mixed source change plus stale marker agrees
// with a fresh index too.
func TestResolverPolicyMixedSourceChangeAndStaleMarker(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), nil)
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)
	r.write(t, "tool/main.go", "package main\n\nfunc helper() {}\nfunc extra() {}\n\nfunc main() {\n\thelper()\n\textra()\n}\n")
	summary := r.update(t)
	if strings.Join(summary.ResolverPolicyLanguages, ",") != "rust" || !strings.HasSuffix(summary.ResolveMode, "+resolver_policy") {
		t.Fatalf("mode=%q languages=%v", summary.ResolveMode, summary.ResolverPolicyLanguages)
	}
	requireRustWrongEdgeGone(t, r, "mixed")
	r.assertPolicyParity(t, "mixed")
}

// A development build stamped C# 1 before the namespace policy shipped. The
// shipped registry is at epoch 4, so that graph is decided again on unchanged
// source, without parsing, and a newer marker is refused with nothing written.
func TestResolverPolicyShippedCSharpEpochRepairsDevelopmentStamp(t *testing.T) {
	r := newRegisteredPolicyRepo(t, rustStaleTree())
	if !strings.Contains(r.markers(t), r.markerKey("csharp")+"=4") {
		t.Fatalf("fresh index markers = %q", r.markers(t))
	}
	want := r.projection(t)
	wantEdge := r.edgeState(t, "cs/Svc.cs", "G")
	if !strings.Contains(wantEdge, "App.Svc.G(") {
		t.Fatalf("fresh csharp edge = %s", wantEdge)
	}

	r.bind(t, "cs/Svc.cs", "G", "App.Svc.F")
	r.exec(t, `UPDATE settings SET value='1' WHERE key=?`, r.markerKey("csharp"))
	r.watchLanguages(t, "go", "java", "rust")

	summary := r.update(t)
	if summary.FilesChanged != 0 || summary.FilesIndexed != 0 || summary.ParseMS != 0 {
		t.Fatalf("policy upgrade parsed source: %+v", summary)
	}
	if got := strings.Join(summary.ResolverPolicyLanguages, ","); got != "csharp" {
		t.Fatalf("redecided %q, want csharp", got)
	}
	if got := r.edgeState(t, "cs/Svc.cs", "G"); got != wantEdge {
		t.Fatalf("edge = %s, want %s", got, wantEdge)
	}
	if n := r.touched(t); n != 0 {
		t.Fatalf("%d rows of other languages were rewritten", n)
	}
	if diff := projectionDiff(want, r.projection(t)); diff != "" {
		t.Fatalf("upgrade diverges from the fresh graph:\n%s", diff)
	}
	if !strings.Contains(r.markers(t), r.markerKey("csharp")+"=4") {
		t.Fatalf("markers = %q", r.markers(t))
	}
	if again := r.update(t); again.ResolveMode != "none" || len(again.ResolverPolicyLanguages) != 0 {
		t.Fatalf("second update: mode=%q languages=%v", again.ResolveMode, again.ResolverPolicyLanguages)
	}

	r.exec(t, `UPDATE settings SET value='5' WHERE key=?`, r.markerKey("csharp"))
	before := r.state(t)
	_, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
	var pe *store.ResolverPolicyError
	if !errors.As(err, &pe) || pe.Language != "csharp" || !errors.Is(err, store.ErrResolverPolicyNewer) {
		t.Fatalf("err = %v", err)
	}
	if after := r.state(t); after != before {
		t.Fatal("refused run mutated the database")
	}
}
