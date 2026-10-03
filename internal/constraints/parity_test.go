package constraints

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

const parityConfig = `{"schema_version":1,
	"groups":{"domain":{"include":["internal/domain/**"]},"infra":{"include":["internal/infra/**"]}},
	"rules":[
		{"id":"domain-no-infra","kind":"forbidden_dependency","from":["domain"],"to":["infra"]},
		{"id":"domain-owned","kind":"allowed_dependents","of":["domain"],"from":[]},
		{"id":"no-cycles","kind":"forbidden_cycles","groups":["domain","infra"]}]}`

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if body == "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// evaluated returns the result with the index block (history metadata) removed.
func evaluated(t *testing.T, root string, st *store.Store, repoID int64) (string, Result) {
	t.Helper()
	res, err := Check(context.Background(), Options{RepoRoot: root}, func(context.Context) (*store.Store, int64, error) { return st, repoID, nil })
	if err != nil {
		t.Fatal(err)
	}
	res.Index = nil
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), res
}

func indexFresh(t *testing.T, root string) (*store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	repo, err := st.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.New(st, parser.NewRegistry(goparser.New()), nil).Index(context.Background(), indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	return st, repo.ID
}

// TestFreshEqualsIncremental: after each add, rename and remove of a
// cross-group call and of a group file, the incrementally updated graph and a
// fresh index of the same tree give byte-equal findings, cycles, summary and
// coverage.
func TestFreshEqualsIncremental(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                     "module example.com/m\n\ngo 1.22\n",
		ConfigFileName:               parityConfig,
		"internal/domain/a.go":       "package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc A() { infra.B() }\n",
		"internal/infra/b.go":        "package infra\n\nfunc B() {}\n",
		"internal/infra/b_helper.go": "package infra\n\nfunc helper() { B() }\n",
	})
	inc, repoID := indexFresh(t, root)
	idx := indexer.New(inc, parser.NewRegistry(goparser.New()), nil)

	steps := []struct {
		name  string
		files map[string]string
	}{
		{"initial", nil},
		{"add cross-group call", map[string]string{
			"internal/infra/b.go": "package infra\n\nimport \"example.com/m/internal/domain\"\n\nfunc B() { domain.A() }\n"}},
		{"add group file", map[string]string{
			"internal/infra/c.go": "package infra\n\nimport \"example.com/m/internal/domain\"\n\nfunc C() { domain.A() }\n"}},
		{"rename group file", map[string]string{
			"internal/infra/c.go": "",
			"internal/infra/d.go": "package infra\n\nimport \"example.com/m/internal/domain\"\n\nfunc C() { domain.A() }\n"}},
		{"rename cross-group call", map[string]string{
			"internal/domain/a.go": "package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc A() { infra.B2() }\n",
			"internal/infra/b.go":  "package infra\n\nimport \"example.com/m/internal/domain\"\n\nfunc B2() { domain.A() }\n\nfunc B() {}\n"}},
		{"remove cross-group call", map[string]string{
			"internal/infra/b.go": "package infra\n\nfunc B2() {}\n\nfunc B() {}\n"}},
		{"remove group file", map[string]string{"internal/infra/d.go": ""}},
	}
	sawFinding, sawCycle := false, false
	for _, step := range steps {
		writeTree(t, root, step.files)
		if step.files != nil {
			if _, err := idx.Update(context.Background(), indexer.Options{RepoRoot: root}); err != nil {
				t.Fatalf("%s: Update: %v", step.name, err)
			}
		}
		fresh, freshID := indexFresh(t, root)
		got, res := evaluated(t, root, inc, repoID)
		want, _ := evaluated(t, root, fresh, freshID)
		if got != want {
			t.Fatalf("%s: incremental differs from fresh\nincremental: %s\nfresh:       %s", step.name, got, want)
		}
		sawFinding = sawFinding || len(res.Findings) > 0
		sawCycle = sawCycle || len(res.Cycles) > 0
		if res.Coverage.TrustedDependenciesEvaluated == 0 {
			t.Fatalf("%s: no trusted dependency evaluated; the fixture does not resolve: %s", step.name, got)
		}
	}
	if !sawFinding || !sawCycle {
		t.Fatalf("fixture never produced a finding (%v) or a cycle (%v)", sawFinding, sawCycle)
	}
}
