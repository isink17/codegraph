//go:build cgo

package indexer

import (
	"errors"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// allTables is every table, id-inclusive, so "zero mutation" is byte-level.
func (r *lifecycleRepo) allTables(t *testing.T) string {
	t.Helper()
	return dumpTables(t, r.raw(t), func(string) bool { return true }, func(string) bool { return false })
}

// A full or forced index resolves the whole repository, so --languages cannot
// narrow which markers it must honour: a newer or unreadable marker of an
// unselected language refuses the run before any write.
func TestResolverPolicyFullIndexRefusesUnsupportedMarkerOfUnselectedLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, language, value string
		unreadable            bool
	}{
		{"newer go", "go", "2", false},
		{"unregistered future language", "cobol", "1", true},
		{"malformed go", "go", "x", true},
	} {
		for _, force := range []bool{false, true} {
			name := tc.name + " index"
			if force {
				name = tc.name + " force"
			}
			t.Run(name, func(t *testing.T) {
				r := newPolicyRepo(t, rustStaleTree(), map[string]int{"rust": 1, "go": 1})
				r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.markerKey(tc.language), tc.value)
				r.write(t, "src/a/y.rs", "pub fn caller() {\n    crate::c::id();\n    crate::c::id();\n}\n")
				before := r.allTables(t)
				_, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index", Force: force, Languages: []string{"rust"}})
				var pe *store.ResolverPolicyError
				if !errors.As(err, &pe) || pe.Language != tc.language {
					t.Fatalf("err = %v", err)
				}
				if tc.unreadable != errors.Is(err, store.ErrResolverPolicyUnreadable) {
					t.Fatalf("wrong reason: %v", err)
				}
				if after := r.allTables(t); after != before {
					t.Fatalf("refused full run mutated the database")
				}
				// An incremental run for the selected language still works: its
				// writes are language-scoped.
				if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Languages: []string{"rust"}}); err != nil {
					t.Fatalf("rust-only update: %v", err)
				}
				if got := r.markers(t); !strings.Contains(got, r.markerKey(tc.language)+"="+tc.value) {
					t.Fatalf("foreign marker rewritten: %q", got)
				}
			})
		}
	}
}

// The store-level unscoped resolve is the other writer of every language's
// edges; it must not bypass the marker check either.
func TestResolveEdgesRecordingPoliciesRefusesUnsupportedMarkerAndRollsBack(t *testing.T) {
	for _, tc := range []struct{ language, value string }{{"go", "2"}, {"cobol", "1"}, {"go", "bad"}} {
		r := newPolicyRepo(t, rustStaleTree(), map[string]int{"rust": 1, "go": 1})
		r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.markerKey(tc.language), tc.value)
		before := r.allTables(t)
		for _, languages := range [][]string{nil, {"rust"}} {
			if _, err := r.store.ResolveEdgesRecordingPolicies(r.ctx, r.repoID, languages); err == nil {
				t.Fatalf("%v %v: resolved over unsupported marker", tc, languages)
			}
			if after := r.allTables(t); after != before {
				t.Fatalf("%v %v: failed resolve left writes", tc, languages)
			}
		}
		if _, err := r.store.ResolveEdges(r.ctx, r.repoID); err == nil {
			t.Fatalf("%v: ResolveEdges bypassed marker check", tc)
		}
	}
}

// Supported filtered full runs keep fresh semantic convergence: the resolve
// decides every language, so every stale language is recorded.
func TestResolverPolicyFilteredFullIndexConvergesEveryStaleLanguage(t *testing.T) {
	r := newPolicyRepo(t, rustStaleTree(), map[string]int{"rust": 1, "go": 1})
	r.bind(t, rustStaleCaller, rustStaleCall, rustStaleWrong)
	r.clearMarkers(t)
	summary, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index", Force: true, Languages: []string{"rust"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ResolveMode != "repo" || strings.Join(summary.ResolverPolicyLanguages, ",") != "go,rust" {
		t.Fatalf("mode=%q languages=%v", summary.ResolveMode, summary.ResolverPolicyLanguages)
	}
	requireRustWrongEdgeGone(t, r, "filtered forced index")
	if want := r.markerKey("go") + "=1," + r.markerKey("rust") + "=1"; r.markers(t) != want {
		t.Fatalf("markers = %q want %q", r.markers(t), want)
	}
}
