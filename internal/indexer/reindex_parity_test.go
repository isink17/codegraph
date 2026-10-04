package indexer

import (
	"strings"
	"testing"
)

// Re-running a full index over an existing database skips unchanged files and
// keeps their edges. A declaration added in another file can make such an
// edge's binding ambiguous, and removing it makes the edge bindable again; the
// re-index must give the graph a fresh index of the same tree gives, both ways.
func TestReindexRedecidesBindingsOfUnchangedFiles(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"app/util.py":  "def helper():\n    return 1\n",
		"app/main.py":  "def run():\n    return helper()\n",
		"app/other.py": "x = 1\n",
	})
	reindex := func() {
		t.Helper()
		if _, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
			t.Fatalf("index: %v", err)
		}
	}
	if got := r.edgeState(t, "app/main.py", "helper"); !strings.Contains(got, "util.helper") {
		t.Fatalf("unique helper not bound: %s", got)
	}
	r.write(t, "app/other.py", "def helper():\n    return 2\n")
	reindex()
	if got := r.edgeState(t, "app/main.py", "helper"); strings.Contains(got, "util.helper") {
		t.Fatalf("competing helper left the old binding: %s", got)
	}
	r.assertFreshParity(t, "competing declaration added")
	r.write(t, "app/other.py", "x = 1\n")
	reindex()
	if got := r.edgeState(t, "app/main.py", "helper"); !strings.Contains(got, "util.helper") {
		t.Fatalf("helper not rebound after the competitor left: %s", got)
	}
	r.assertFreshParity(t, "competing declaration removed")
}
