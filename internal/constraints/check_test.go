package constraints

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// fixture is a seeded graph database. Rows are written with plain SQL so each
// test controls exactly which edges exist, including states no parser emits on
// demand (unresolved, low confidence, deleted files, another repository).
type fixture struct {
	t      *testing.T
	root   string
	st     *store.Store
	db     *sql.DB
	repoID int64
	files  map[string]int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo, err := st.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := st.EnsureCanonicalRepositoryPaths(context.Background(), repo.ID, true); err != nil {
		t.Fatalf("EnsureCanonicalRepositoryPaths: %v", err)
	}
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	f := &fixture{t: t, root: root, st: st, db: db, repoID: repo.ID, files: map[string]int64{}}
	f.exec(`INSERT INTO scans(repo_id, scan_kind, started_at, status) VALUES(?, 'full', '2026-01-01T00:00:00Z', 'completed')`, repo.ID)
	return f
}

func (f *fixture) exec(q string, args ...any) int64 {
	f.t.Helper()
	res, err := f.db.Exec(q, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) fileIn(repoID int64, path string, deleted bool) int64 {
	del := 0
	if deleted {
		del = 1
	}
	id := f.exec(`INSERT INTO files(repo_id, path, language, indexed_at, is_deleted, parser_profile, parser_call_edges)
		VALUES(?, ?, 'go', '2026-01-01T00:00:00Z', ?, 'go', 1)`, repoID, path, del)
	if repoID == f.repoID {
		f.files[path] = id
	}
	return id
}

func (f *fixture) file(path string) int64 { return f.fileIn(f.repoID, path, false) }

// sym adds a symbol in path (creating the file) and returns its id.
func (f *fixture) sym(path, name string, start int) int64 {
	return f.symKey(path, name, start, path+"#"+name)
}

func (f *fixture) symKey(path, name string, start int, key string) int64 {
	fid, ok := f.files[path]
	if !ok {
		fid = f.file(path)
	}
	return f.exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', ?, ?, ?, 1, ?, 1, ?)`, f.repoID, fid, name, name, start, start+1, key)
}

// call adds a resolved high-confidence call edge evidenced in the source file.
func (f *fixture) call(src, dst int64, line int) {
	f.edge(src, dst, line, "calls", "exact_name", "high")
}

func (f *fixture) edge(src, dst int64, line int, kind, strategy, confidence string) {
	var dstArg any
	if dst != 0 {
		dstArg = dst
	}
	f.exec(`INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line, resolution_strategy, resolution_confidence)
		VALUES(?, ?, ?, 'x', ?, '', (SELECT file_id FROM symbols WHERE id = ?), ?, ?, ?)`,
		f.repoID, src, dstArg, kind, src, line, strategy, confidence)
}

func (f *fixture) config(doc string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, ConfigFileName), []byte(doc), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) check(limit, offset int) Result {
	f.t.Helper()
	res, err := Check(context.Background(), Options{RepoRoot: f.root, Limit: limit, Offset: offset},
		func(context.Context) (*store.Store, int64, error) { return f.st, f.repoID, nil })
	if err != nil {
		f.t.Fatalf("Check: %v", err)
	}
	return res
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const layersGroups = `"groups":{
	"api":{"include":["cmd/**","internal/api/**"]},
	"domain":{"include":["internal/domain/**"],"exclude":["internal/domain/**/*_test.go"]},
	"infra":{"include":["internal/infra/**"]}}`

func layersDoc(rules string) string {
	return `{"schema_version":1,` + layersGroups + `,"rules":[` + rules + `]}`
}

func findingKeys(res Result) []string {
	var out []string
	for _, f := range res.Findings {
		sg, tg := "null", "null"
		if f.SourceGroup != nil {
			sg = *f.SourceGroup
		}
		if f.TargetGroup != nil {
			tg = *f.TargetGroup
		}
		out = append(out, f.RuleID+":"+sg+"->"+tg)
	}
	return out
}

func TestRuleKindsPositiveAndNegative(t *testing.T) {
	cases := []struct {
		name, rule string
		edges      func(f *fixture)
		want       []string
	}{
		{"forbidden_dependency positive", `{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`,
			func(f *fixture) {
				f.call(f.sym("internal/domain/a.go", "A", 1), f.sym("internal/infra/b.go", "B", 1), 2)
			},
			[]string{"r:domain->infra"}},
		{"forbidden_dependency negative", `{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`,
			func(f *fixture) {
				f.call(f.sym("internal/infra/b.go", "B", 1), f.sym("internal/domain/a.go", "A", 1), 2)
			},
			nil},
		{"allowed_dependencies positive", `{"id":"r","kind":"allowed_dependencies","from":["api"],"to":["domain"]}`,
			func(f *fixture) { f.call(f.sym("cmd/x/main.go", "M", 1), f.sym("internal/infra/b.go", "B", 1), 2) },
			[]string{"r:api->infra"}},
		{"allowed_dependencies unowned target", `{"id":"r","kind":"allowed_dependencies","from":["api"],"to":["domain"]}`,
			func(f *fixture) { f.call(f.sym("cmd/x/main.go", "M", 1), f.sym("pkg/util/u.go", "U", 1), 12) },
			[]string{"r:api->null"}},
		{"allowed_dependencies negative", `{"id":"r","kind":"allowed_dependencies","from":["api"],"to":["domain"]}`,
			func(f *fixture) {
				m := f.sym("cmd/x/main.go", "M", 1)
				f.call(m, f.sym("internal/domain/a.go", "A", 1), 2)
				f.call(m, f.sym("cmd/x/other.go", "O", 1), 3) // same group
			},
			nil},
		{"allowed_dependents positive", `{"id":"r","kind":"allowed_dependents","of":["domain"],"from":["api"]}`,
			func(f *fixture) {
				f.call(f.sym("internal/infra/b.go", "B", 1), f.sym("internal/domain/a.go", "A", 1), 2)
			},
			[]string{"r:infra->domain"}},
		{"allowed_dependents unowned source", `{"id":"r","kind":"allowed_dependents","of":["domain"],"from":["api"]}`,
			func(f *fixture) { f.call(f.sym("pkg/util/u.go", "U", 1), f.sym("internal/domain/a.go", "A", 1), 2) },
			[]string{"r:null->domain"}},
		{"allowed_dependents negative", `{"id":"r","kind":"allowed_dependents","of":["domain"],"from":["api"]}`,
			func(f *fixture) {
				f.call(f.sym("cmd/x/main.go", "M", 1), f.sym("internal/domain/a.go", "A", 1), 2)
				f.call(f.sym("internal/domain/c.go", "C", 1), f.sym("internal/domain/d.go", "D", 1), 2)
			},
			nil},
		{"excluded test file is unowned", `{"id":"r","kind":"allowed_dependents","of":["infra"],"from":[]}`,
			func(f *fixture) {
				f.call(f.sym("internal/domain/a_test.go", "T", 1), f.sym("internal/infra/b.go", "B", 1), 2)
			},
			[]string{"r:null->infra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.edges(f)
			f.config(layersDoc(tc.rule))
			res := f.check(0, 0)
			got := findingKeys(res)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("findings = %v, want %v\n%s", got, tc.want, marshal(t, res))
			}
			wantStatus := StatusOK
			if len(tc.want) > 0 {
				wantStatus = StatusViolations
			}
			if res.Status != wantStatus || ExitCode(res.Status) != map[string]int{StatusOK: 0, StatusViolations: 1}[wantStatus] {
				t.Fatalf("status = %s, want %s", res.Status, wantStatus)
			}
		})
	}
}

func TestForbiddenCyclesPositiveAndNegative(t *testing.T) {
	rule := `{"id":"no-cycles","kind":"forbidden_cycles","groups":["api","domain","infra"]}`
	f := newFixture(t)
	a := f.sym("internal/domain/a.go", "A", 1)
	b := f.sym("internal/infra/b.go", "B", 1)
	f.call(a, b, 5)
	f.config(layersDoc(rule))
	if res := f.check(0, 0); res.Status != StatusOK || res.Summary.Cycles != 0 {
		t.Fatalf("one-directional dependency reported a cycle: %s", marshal(t, res))
	}
	f.call(b, a, 9)
	res := f.check(0, 0)
	if res.Status != StatusViolations || res.Summary.Cycles != 1 || len(res.Cycles) != 1 {
		t.Fatalf("cycle not reported: %s", marshal(t, res))
	}
	c := res.Cycles[0]
	if strings.Join(c.Members, ",") != "domain,infra" || strings.Join(c.Witness, ",") != "domain,infra,domain" || len(c.Hops) != 2 {
		t.Fatalf("cycle = %+v", c)
	}
	if c.Hops[0].Dependency.Source.Line != 5 || c.Hops[1].Dependency.Source.Line != 9 {
		t.Fatalf("hop witnesses = %+v", c.Hops)
	}
	if len(res.Findings) != 0 || res.Summary.ByRule["no-cycles"] != 1 {
		t.Fatalf("cycles leaked into findings or by_rule: %s", marshal(t, res))
	}
}

// TestCycleWitnessIsShortestThenSmallest: two return paths from api, one of
// length 2 through infra and one of length 3; the witness is the shorter one,
// and among equal lengths the lexicographically smallest.
func TestCycleWitnessIsShortestThenSmallest(t *testing.T) {
	doc := `{"schema_version":1,"groups":{"a":{"include":["a/**"]},"b":{"include":["b/**"]},"c":{"include":["c/**"]},"d":{"include":["d/**"]}},
		"rules":[{"id":"cyc","kind":"forbidden_cycles","groups":["a","b","c","d"]}]}`
	f := newFixture(t)
	a, b, c, d := f.sym("a/x.go", "A", 1), f.sym("b/x.go", "B", 1), f.sym("c/x.go", "C", 1), f.sym("d/x.go", "D", 1)
	f.call(a, d, 1)
	f.call(d, a, 1) // a -> d -> a (length 2)
	f.call(a, b, 1)
	f.call(b, c, 1)
	f.call(c, a, 1) // a -> b -> c -> a (length 3)
	f.call(a, c, 2)
	f.call(c, a, 2) // a -> c -> a (length 2, smaller than a,d,a)
	f.config(doc)
	res := f.check(0, 0)
	if len(res.Cycles) != 1 || strings.Join(res.Cycles[0].Witness, ",") != "a,c,a" || strings.Join(res.Cycles[0].Members, ",") != "a,b,c,d" {
		t.Fatalf("cycles = %s", marshal(t, res.Cycles))
	}
}

// TestCycleCompletenessBeyondLimit: more SCCs than limit; the count stays exact
// and the status and exit do not change with truncation.
func TestCycleCompletenessBeyondLimit(t *testing.T) {
	groups := []string{"a", "b", "c", "d", "e", "f"}
	var gs []string
	for _, g := range groups {
		gs = append(gs, `"`+g+`":{"include":["`+g+`/**"]}`)
	}
	doc := `{"schema_version":1,"groups":{` + strings.Join(gs, ",") + `},"rules":[{"id":"cyc","kind":"forbidden_cycles","groups":["a","b","c","d","e","f"]}]}`
	f := newFixture(t)
	for i := 0; i < len(groups); i += 2 {
		x, y := f.sym(groups[i]+"/x.go", "X", 1), f.sym(groups[i+1]+"/y.go", "Y", 1)
		f.call(x, y, 1)
		f.call(y, x, 1)
	}
	f.config(doc)
	full := f.check(0, 0)
	paged := f.check(1, 0)
	if full.Summary.Cycles != 3 || len(full.Cycles) != 3 || full.CyclesTruncated {
		t.Fatalf("full = %s", marshal(t, full))
	}
	if paged.Summary.Cycles != 3 || len(paged.Cycles) != 1 || !paged.CyclesTruncated || paged.Status != full.Status || paged.Status != StatusViolations {
		t.Fatalf("paged = %s", marshal(t, paged))
	}
	if strings.Join(paged.Cycles[0].Members, ",") != "a,b" {
		t.Fatalf("first cycle = %v, want a,b", paged.Cycles[0].Members)
	}
}

func TestNegativeFences(t *testing.T) {
	f := newFixture(t)
	rule := `{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`
	a := f.sym("internal/domain/a.go", "A", 1)
	b := f.sym("internal/infra/b.go", "B", 1)
	// Unresolved edge across the forbidden pair: blind spot, never a violation.
	f.edge(a, 0, 3, "calls", "", "")
	// Import string: file_imports is never read.
	f.exec(`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, 'internal/infra')`, f.repoID, f.files["internal/domain/a.go"])
	// Low-confidence and cross-language rows: excluded by the trust filter.
	f.edge(a, b, 4, "calls", "dot_suffix", "low")
	f.edge(a, b, 0, "cross_language_ref", "cross_language", "low")
	// Deleted target file.
	delTarget := f.fileIn(f.repoID, "internal/infra/gone.go", true)
	gone := f.exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'G', 'G', 1, 1, 2, 1, 'g')`, f.repoID, delTarget)
	f.call(a, gone, 5)
	// Deleted source file.
	delSource := f.fileIn(f.repoID, "internal/domain/gone.go", true)
	ghost := f.exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'H', 'H', 1, 1, 2, 1, 'h')`, f.repoID, delSource)
	f.call(ghost, b, 6)
	// Deleted evidence file: the edge row is attributed to a retired file.
	evid := f.fileIn(f.repoID, "internal/domain/evidence.go", true)
	f.exec(`INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line, resolution_strategy, resolution_confidence)
		VALUES(?, ?, ?, 'B', 'calls', ?, 7, 'exact_name', 'high')`, f.repoID, a, b, evid)
	// Another repository's rows in the same database, same paths.
	other := f.exec(`INSERT INTO repos(root_path, canonical_path, created_at, updated_at) VALUES('/other', '/other', '', '')`)
	oa := f.fileIn(other, "internal/domain/a.go", false)
	ob := f.fileIn(other, "internal/infra/b.go", false)
	osa := f.exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'A', 'A', 1, 1, 2, 1, 'a')`, other, oa)
	osb := f.exec(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'B', 'B', 1, 1, 2, 1, 'b')`, other, ob)
	f.exec(`INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line, resolution_strategy, resolution_confidence)
		VALUES(?, ?, ?, 'B', 'calls', ?, 8, 'exact_name', 'high')`, other, osa, osb, oa)

	f.config(layersDoc(rule))
	res := f.check(0, 0)
	if res.Status != StatusOK || len(res.Findings) != 0 {
		t.Fatalf("a fenced row became a violation: %s", marshal(t, res))
	}
	cov := res.Coverage
	if cov.UnresolvedDependencyRowsBySourceGroup["domain"] != 1 {
		t.Errorf("unresolved coverage = %v, want domain:1", cov.UnresolvedDependencyRowsBySourceGroup)
	}
	if cov.ExcludedByTrustFilter["domain→infra"] != 2 {
		t.Errorf("excluded_by_trust_filter = %v, want domain→infra:2", cov.ExcludedByTrustFilter)
	}
	if cov.TrustedDependenciesEvaluated != 0 {
		t.Errorf("trusted_dependencies_evaluated = %d, want 0", cov.TrustedDependenciesEvaluated)
	}
}

func TestPathIdentity(t *testing.T) {
	f := newFixture(t)
	f.file("internal/x.go")
	if runtime.GOOS != "windows" {
		f.file(`dir/a\b`)
	}
	f.config(`{"schema_version":1,"groups":{"s":{"include":["Internal/**"]},"w":{"include":["dir/a?b"]}},"rules":[]}`)
	res := f.check(0, 0)
	want := []string{"Internal/**"}
	if runtime.GOOS == "windows" {
		want = []string{"Internal/**", "dir/a?b"}
	}
	if strings.Join(res.Coverage.UnmatchedPatterns, ",") != strings.Join(want, ",") {
		t.Fatalf("unmatched_patterns = %v, want %v", res.Coverage.UnmatchedPatterns, want)
	}
	wantUnowned := 1 // internal/x.go: the case-mismatched pattern owns nothing
	if res.Coverage.UnownedFiles != wantUnowned {
		t.Fatalf("unowned_files = %d, want %d", res.Coverage.UnownedFiles, wantUnowned)
	}
}

func TestGroupOverlapIsConfigError(t *testing.T) {
	f := newFixture(t)
	f.file("internal/store/a.go")
	f.config(`{"schema_version":1,"groups":{"a":{"include":["internal/**"]},"b":{"include":["internal/store/**"]}},"rules":[]}`)
	res := f.check(0, 0)
	if res.Status != StatusConfigError || len(res.Errors) != 1 ||
		res.Errors[0] != (Error{Location: "internal/store/a.go", Code: CodeGroupOverlap, Message: "groups a, b"}) {
		t.Fatalf("result = %s", marshal(t, res))
	}
	// A per-group exclude resolves it.
	f.config(`{"schema_version":1,"groups":{"a":{"include":["internal/**"],"exclude":["internal/store/**"]},"b":{"include":["internal/store/**"]}},"rules":[]}`)
	if res := f.check(0, 0); res.Status != StatusOK {
		t.Fatalf("exclude did not resolve the overlap: %s", marshal(t, res))
	}
}

func TestOverlapErrorsAreBounded(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 25; i++ {
		f.file("x/" + string(rune('a'+i)) + ".go")
	}
	f.config(`{"schema_version":1,"groups":{"a":{"include":["x/**"]},"b":{"include":["**"]}},"rules":[]}`)
	res := f.check(0, 0)
	if res.Status != StatusConfigError || len(res.Errors) != maxOverlapErrors || res.Errors[0].Location != "x/a.go" {
		t.Fatalf("overlap errors = %+v", res.Errors)
	}
}

func TestConstraintsFileIsExcludedFromCoverage(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, ?, '', '')`, f.repoID, ConfigFileName)
	f.config(`{"schema_version":1,"groups":{"a":{"include":["**"]}},"rules":[]}`)
	res := f.check(0, 0)
	if res.Status != StatusOK || res.Coverage.UnownedFiles != 0 || len(res.Coverage.UnmatchedPatterns) != 1 {
		t.Fatalf("constraints file counted: %s", marshal(t, res))
	}
}

func TestFilesWithoutCallEdgesCoverage(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO files(repo_id, path, language, indexed_at, parser_profile, parser_call_edges) VALUES(?, 'app/a.rb', 'ruby', '', 'ruby-heuristic', 0)`, f.repoID)
	f.exec(`INSERT INTO files(repo_id, path, language, indexed_at, parser_profile, parser_call_edges) VALUES(?, 'README.md', '', '', '', 0)`, f.repoID)
	f.config(`{"schema_version":1,"groups":{"app":{"include":["app/**"]}},"rules":[]}`)
	res := f.check(0, 0)
	got := res.Coverage.FilesWithoutCallEdgesByGroupAndLanguage
	if len(got) != 1 || got["app"]["ruby"] != 1 {
		t.Fatalf("files_without_call_edges = %v", got)
	}
}

// TestDuplicateStableKeyTargetsStayDistinct: stable_key is not unique, so two
// targets sharing it but starting on different lines are two findings, while
// two rows with the full identity collapse into one with occurrences 2.
func TestDuplicateStableKeyTargetsStayDistinct(t *testing.T) {
	f := newFixture(t)
	a := f.sym("internal/domain/a.go", "A", 1)
	t1 := f.symKey("internal/infra/b.go", "B", 1, "dup")
	t2 := f.symKey("internal/infra/b.go", "B", 10, "dup")
	f.call(a, t1, 3)
	f.call(a, t2, 3)
	f.call(a, t2, 3)
	f.config(layersDoc(`{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`))
	res := f.check(0, 0)
	if len(res.Findings) != 2 || res.Findings[0].Target.StartLine != 1 || res.Findings[1].Target.StartLine != 10 ||
		res.Findings[1].Occurrences != 2 || res.Summary.Occurrences != 3 || res.Summary.Findings != 2 {
		t.Fatalf("findings = %s", marshal(t, res))
	}
}

func TestDeterminismShuffledRulesRepeatedRunsAndPaging(t *testing.T) {
	f := newFixture(t)
	m := f.sym("cmd/x/main.go", "M", 1)
	a := f.sym("internal/domain/a.go", "A", 1)
	b := f.sym("internal/infra/b.go", "B", 1)
	u := f.sym("pkg/u.go", "U", 1)
	for line := 1; line <= 4; line++ {
		f.call(a, b, line)
		f.call(m, b, line)
		f.call(u, a, line)
	}
	f.call(b, a, 9)
	rules := []string{
		`{"id":"r1","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`,
		`{"id":"r2","kind":"allowed_dependencies","from":["api"],"to":["domain"]}`,
		`{"id":"r3","kind":"allowed_dependents","of":["domain"],"from":["api"]}`,
		`{"id":"r4","kind":"forbidden_cycles","groups":["infra","domain"]}`,
	}
	f.config(layersDoc(strings.Join(rules, ",")))
	base := marshal(t, f.check(0, 0))
	for i := 0; i < 3; i++ {
		if again := marshal(t, f.check(0, 0)); again != base {
			t.Fatalf("repeated run differs:\n%s\n%s", base, again)
		}
	}
	shuffled := `{"rules":[` + strings.Join([]string{rules[3], rules[1], rules[0], rules[2]}, ",") + `],"schema_version":1,"groups":{
		"infra":{"include":["internal/infra/**"]},
		"domain":{"exclude":["internal/domain/**/*_test.go"],"include":["internal/domain/**"]},
		"api":{"include":["cmd/**","internal/api/**"]}}}`
	f.config(shuffled)
	got := f.check(0, 0)
	got.Config = Result{}.Config
	want := Result{}
	if err := json.Unmarshal([]byte(base), &want); err != nil {
		t.Fatal(err)
	}
	want.Config = Result{}.Config
	if marshal(t, got) != marshal(t, want) {
		t.Fatalf("shuffled config changed the result:\n%s\n%s", marshal(t, want), marshal(t, got))
	}

	// Paging: limit-1 pages concatenate to the full list.
	var paged []Finding
	for off := 0; ; off++ {
		page := f.check(1, off)
		paged = append(paged, page.Findings...)
		if !page.FindingsTruncated {
			break
		}
	}
	if marshal(t, paged) != marshal(t, want.Findings) || len(paged) != want.Summary.Findings {
		t.Fatalf("paged findings differ:\n%s\n%s", marshal(t, paged), marshal(t, want.Findings))
	}
}

func TestStatuses(t *testing.T) {
	t.Run("not_configured", func(t *testing.T) {
		f := newFixture(t)
		res := f.check(0, 0)
		if res.Status != StatusNotConfigured || ExitCode(res.Status) != 2 || res.Errors[0].Location != ConfigFileName {
			t.Fatalf("result = %s", marshal(t, res))
		}
	})
	t.Run("config_error before index", func(t *testing.T) {
		f := newFixture(t)
		f.config(`{"schema_version":1,"groups":{"a":{"include":["a\\b"]}}}`)
		res, err := Check(context.Background(), Options{RepoRoot: f.root}, func(context.Context) (*store.Store, int64, error) {
			t.Fatal("the index was opened for an invalid config")
			return nil, 0, nil
		})
		if err != nil || res.Status != StatusConfigError || ExitCode(res.Status) != 2 || res.Config.SHA256 == nil {
			t.Fatalf("result = %s, err %v", marshal(t, res), err)
		}
	})
	t.Run("not_indexed", func(t *testing.T) {
		f := newFixture(t)
		f.config(`{"schema_version":1,"rules":[]}`)
		res, err := Check(context.Background(), Options{RepoRoot: f.root}, func(context.Context) (*store.Store, int64, error) {
			return nil, 0, errors.New("wrapped: " + store.ErrRepoNotIndexed.Error())
		})
		if err == nil {
			t.Fatalf("an unrelated open error became a status: %s", marshal(t, res))
		}
		res, err = Check(context.Background(), Options{RepoRoot: f.root}, func(context.Context) (*store.Store, int64, error) {
			return nil, 0, store.ErrRepoNotIndexed
		})
		if err != nil || res.Status != StatusNotIndexed || ExitCode(res.Status) != 2 {
			t.Fatalf("result = %s, err %v", marshal(t, res), err)
		}
	})
	t.Run("never scanned", func(t *testing.T) {
		f := newFixture(t)
		f.exec(`DELETE FROM scans`)
		f.config(`{"schema_version":1,"rules":[]}`)
		if res := f.check(0, 0); res.Status != StatusNotIndexed {
			t.Fatalf("result = %s", marshal(t, res))
		}
	})
	t.Run("stale keeps findings", func(t *testing.T) {
		f := newFixture(t)
		f.call(f.sym("internal/domain/a.go", "A", 1), f.sym("internal/infra/b.go", "B", 1), 2)
		f.exec(`INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, 'internal/domain/a.go', 'modified', '')`, f.repoID)
		f.config(layersDoc(`{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`))
		res := f.check(0, 0)
		if res.Status != StatusStale || ExitCode(res.Status) != 2 || len(res.Findings) != 1 || res.Index.DirtyFiles != 1 {
			t.Fatalf("result = %s", marshal(t, res))
		}
	})
	t.Run("paging arguments", func(t *testing.T) {
		for _, p := range [][2]int{{-1, 0}, {501, 0}, {0, -1}} {
			if err := ValidatePage(p[0], p[1]); err == nil {
				t.Errorf("ValidatePage(%d, %d) accepted", p[0], p[1])
			}
		}
	})
}

// TestCheckDoesNotWrite: the evaluator runs on a read-write handle in MCP, so
// read-only rests on it issuing SELECT only. PRAGMA data_version on a separate
// connection changes when any other connection commits a write.
func TestCheckDoesNotWrite(t *testing.T) {
	f := newFixture(t)
	f.call(f.sym("internal/domain/a.go", "A", 1), f.sym("internal/infra/b.go", "B", 1), 2)
	f.config(layersDoc(`{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`))
	conn, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	version := func() (dv, changes int64) {
		if err := conn.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&dv); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), `SELECT total_changes()`).Scan(&changes); err != nil {
			t.Fatal(err)
		}
		return
	}
	beforeDV, beforeChanges := version()
	if res := f.check(0, 0); res.Status != StatusViolations {
		t.Fatalf("status = %s", res.Status)
	}
	afterDV, afterChanges := version()
	if beforeDV != afterDV || beforeChanges != afterChanges {
		t.Fatalf("data_version %d -> %d, total_changes %d -> %d: the evaluation wrote", beforeDV, afterDV, beforeChanges, afterChanges)
	}
	// Sanity: the probe does notice a write made through the store's handle.
	if err := f.st.QueueDirtyFiles(context.Background(), f.repoID, []string{"internal/domain/a.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	if dv, _ := version(); dv == afterDV {
		t.Fatal("data_version did not move on a real write; the probe proves nothing")
	}
}

func TestLogicalConfigPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		filepath.Join(root, ConfigFileName):         ConfigFileName,
		filepath.Join(root, "conf", "rules.json"):   "conf/rules.json",
		filepath.Join(t.TempDir(), "rules.json"):    "",
		filepath.Join(root, "..", "elsewhere.json"): "",
	}
	for in, want := range cases {
		if got := logicalConfigPath(root, in); got != want {
			t.Errorf("logicalConfigPath(%q) = %q, want %q", in, got, want)
		}
	}
	// Spelled through a symlink to the repository, the file is still inside.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err == nil {
		if got := logicalConfigPath(root, filepath.Join(link, "conf", "rules.json")); got != "conf/rules.json" {
			t.Errorf("through a symlink = %q, want conf/rules.json", got)
		}
	}
}

// TestMediumConfidenceEdgesAreEvaluated: the trust filter admits high and
// medium edges, so a medium edge across a forbidden pair is a finding.
func TestMediumConfidenceEdgesAreEvaluated(t *testing.T) {
	f := newFixture(t)
	a := f.sym("internal/domain/a.go", "A", 1)
	b := f.sym("internal/infra/b.go", "B", 1)
	f.edge(a, b, 5, "calls", "dot_suffix", "medium")
	f.config(layersDoc(`{"id":"domain-no-infra","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}`))
	res := f.check(0, 0)
	if res.Status != StatusViolations || len(res.Findings) != 1 {
		t.Fatalf("medium edge not evaluated: %s", marshal(t, res))
	}
}

// TestDiamondIsNoCycle: a->b, a->c, c->b reaches b twice but has no cycle.
// Without Tarjan's on-stack check the cross edge c->b would merge a and c.
func TestDiamondIsNoCycle(t *testing.T) {
	doc := `{"schema_version":1,"groups":{"a":{"include":["a/**"]},"b":{"include":["b/**"]},"c":{"include":["c/**"]}},
		"rules":[{"id":"cyc","kind":"forbidden_cycles","groups":["a","b","c"]}]}`
	f := newFixture(t)
	a, b, c := f.sym("a/x.go", "A", 1), f.sym("b/x.go", "B", 1), f.sym("c/x.go", "C", 1)
	f.call(a, b, 1)
	f.call(a, c, 1)
	f.call(c, b, 1)
	f.config(doc)
	if res := f.check(0, 0); res.Status != StatusOK || res.Summary.Cycles != 0 {
		t.Fatalf("diamond reported a cycle: %s", marshal(t, res))
	}
}

// TestCycleWitnessPrefersShorterOverSmallerFirstHop: depth-first search from a
// would follow b first and return a,b,c,a; the shortest return is a,d,a.
func TestCycleWitnessPrefersShorterOverSmallerFirstHop(t *testing.T) {
	doc := `{"schema_version":1,"groups":{"a":{"include":["a/**"]},"b":{"include":["b/**"]},"c":{"include":["c/**"]},"d":{"include":["d/**"]}},
		"rules":[{"id":"cyc","kind":"forbidden_cycles","groups":["a","b","c","d"]}]}`
	f := newFixture(t)
	a, b, c, d := f.sym("a/x.go", "A", 1), f.sym("b/x.go", "B", 1), f.sym("c/x.go", "C", 1), f.sym("d/x.go", "D", 1)
	f.call(a, b, 1)
	f.call(b, c, 1)
	f.call(c, a, 1)
	f.call(a, d, 1)
	f.call(d, a, 1)
	f.config(doc)
	res := f.check(0, 0)
	if len(res.Cycles) != 1 || strings.Join(res.Cycles[0].Witness, ",") != "a,d,a" {
		t.Fatalf("cycles = %s", marshal(t, res.Cycles))
	}
}
