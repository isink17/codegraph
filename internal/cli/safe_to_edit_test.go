package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/editsafety"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

// safeToEditFiles is a small Go module: domain.A calls infra.B (no test) and
// infra.C (also called from a test), calls Lonely only through an interface,
// and takes infra.Callback as a function value, which records no edge.
var safeToEditFiles = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.22\n",
	"internal/domain/a.go": `package domain

import "example.com/m/internal/infra"

type Lonelier interface{ Lonely() }

func A(l Lonelier) {
	infra.B()
	infra.C()
	l.Lonely()
	g := infra.Callback
	g()
}
`,
	"internal/infra/b.go": `package infra

type T struct{}

func (T) Lonely() {}

func B() {}

func C() {}

func Callback() {}

func isolated() {}
`,
	"internal/infra/b_test.go": "package infra\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { C() }\n",
	"internal/u1/x.go":         "package util\n\nfunc Dup() {}\n",
	"internal/u2/x.go":         "package util\n\nfunc Dup() {}\n",
}

// safeToEditRepo commits safeToEditFiles (plus extra) to a fresh Git
// repository and indexes it, so the newest full scan covers HEAD.
func safeToEditRepo(t *testing.T, extra map[string]string) (string, *gittest.Repo) {
	t.Helper()
	quietStartup(t)
	gittest.Require(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	repo := &gittest.Repo{T: t, Dir: root}
	repo.Git("init", "-q", "-b", "main")
	repo.Write(".gitignore", ".codegraph/\n")
	for rel, body := range safeToEditFiles {
		repo.Write(rel, body)
	}
	for rel, body := range extra {
		repo.Write(rel, body)
	}
	repo.Commit("", "one")
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatal(err)
	}
	return root, repo
}

// safeToEdit runs the command and returns stdout, the decoded result (zero
// when nothing was printed), the exit code and the error text.
func safeToEdit(t *testing.T, args ...string) (string, editsafety.Result, int, string) {
	t.Helper()
	out, _, err := runCLI(t, append([]string{"safe_to_edit"}, args...)...)
	code, msg := 0, ""
	if err != nil {
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%v returned a non-exit error: %v", args, err)
		}
		code, msg = exitErr.Code, err.Error()
	}
	var res editsafety.Result
	if out != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out)
		}
	}
	return out, res, code, msg
}

func verdictOf(res editsafety.Result) string {
	if res.Verdict == nil {
		return "null"
	}
	return *res.Verdict
}

func ruleOutcome(res editsafety.Result, id string) string {
	for _, r := range res.Rules {
		if r.ID == id {
			return r.Outcome
		}
	}
	return ""
}

func TestSafeToEditCLIAssessments(t *testing.T) {
	root, _ := safeToEditRepo(t, nil)
	cases := []struct {
		name        string
		args        []string
		status      string
		verdict     string
		reason      string
		code        int
		dependents  int
		fromTests   int
		unresolved  int
		symbolCount int
	}{
		// A known dependent with no test evidence: CAUTION (untested).
		{"dependent without tests", []string{"--symbol", "infra.B"}, editsafety.StatusAssessed, editsafety.VerdictCaution, editsafety.RuleUntestedDependents, 1, 1, 0, 0, 1},
		// A known dependent and a test caller: CAUTION (known dependents).
		{"dependent with tests", []string{"--symbol", "infra.C"}, editsafety.StatusAssessed, editsafety.VerdictCaution, editsafety.RuleKnownDependents, 1, 2, 1, 0, 1},
		// No edge at all is apparent isolation, never SAFE.
		{"apparent isolation", []string{"--symbol", "infra.isolated"}, editsafety.StatusUnknown, "null", editsafety.ReasonIsolationUnproven, 4, 0, 0, 0, 1},
		// Used only as a function value: the graph records no edge, so the
		// dynamic use is invisible and the answer stays unknown.
		{"function value use", []string{"--symbol", "infra.Callback"}, editsafety.StatusUnknown, "null", editsafety.ReasonIsolationUnproven, 4, 0, 0, 0, 1},
		// Reached only through an interface call: an unresolved edge names
		// it, with no proven dependent, so unknown.
		{"unresolved candidate only", []string{"--symbol", "infra.T.Lonely"}, editsafety.StatusUnknown, "null", editsafety.ReasonIsolationUnproven, 4, 0, 0, 1, 1},
		// A file target: every declared symbol, dependents from other files.
		{"file", []string{"--file", "internal/infra/b.go"}, editsafety.StatusAssessed, editsafety.VerdictCaution, editsafety.RuleKnownDependents, 1, 3, 1, 1, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, res, code, msg := safeToEdit(t, append([]string{root}, tc.args...)...)
			if res.Schema != editsafety.SchemaID || res.Status != tc.status || verdictOf(res) != tc.verdict || res.Reason != tc.reason || code != tc.code {
				t.Fatalf("got status %s verdict %s reason %s exit %d (%s)\n%s", res.Status, verdictOf(res), res.Reason, code, msg, out)
			}
			if res.Evidence.DependentsTotal != tc.dependents || res.Evidence.DependentsFromTests != tc.fromTests ||
				res.Evidence.UnresolvedCandidates != tc.unresolved || len(res.Target.Symbols) != tc.symbolCount {
				t.Fatalf("evidence %+v symbols %d\n%s", res.Evidence, len(res.Target.Symbols), out)
			}
			if res.Freshness.Verdict != constraints.VerdictFullCoverageAtHead || !res.Coverage.Sufficient {
				t.Fatalf("freshness %+v coverage %+v", res.Freshness, res.Coverage)
			}
			if ruleOutcome(res, editsafety.RuleIsolation) != editsafety.OutcomeNotEvaluated {
				t.Fatalf("isolation rule = %q", ruleOutcome(res, editsafety.RuleIsolation))
			}
			if strings.Contains(out, root) {
				t.Fatalf("result leaks the host repo root:\n%s", out)
			}
		})
	}
}

func TestSafeToEditCLIInvalidTargets(t *testing.T) {
	root, _ := safeToEditRepo(t, nil)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"ambiguous qualified name", []string{"--symbol", "util.Dup"}, "ambiguous"},
		{"short name is not a qualified name", []string{"--symbol", "B"}, "not found"},
		{"nonexistent", []string{"--symbol", "infra.Nope"}, "not found"},
		{"directory", []string{"--file", "internal/infra"}, "directories are not supported"},
		{"missing file", []string{"--file", "internal/infra/zz.go"}, "not found"},
		{"no target", nil, "exactly one"},
		{"two targets", []string{"--symbol", "infra.B", "--file", "internal/infra/b.go"}, "exactly one"},
		{"negative id", []string{"--symbol-id", "-3"}, "positive"},
		{"escaping path", []string{"--file", "../x.go"}, "not a repository-relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, code, msg := safeToEdit(t, append([]string{root}, tc.args...)...)
			if code != editsafety.ExitInvalid || out != "" || !strings.Contains(msg, tc.want) {
				t.Fatalf("exit %d msg %q out %q", code, msg, out)
			}
		})
	}
	// The ambiguity names its candidates; an exact id resolves it.
	_, _, _, msg := safeToEdit(t, root, "--symbol", "util.Dup")
	if !strings.Contains(msg, "internal/u1/x.go") || !strings.Contains(msg, "internal/u2/x.go") {
		t.Fatalf("ambiguity does not list candidates: %s", msg)
	}
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	syms, err := opened.Store.FindSymbolExact(context.Background(), opened.Repo.ID, "util.Dup", 10, 0)
	opened.Close()
	if err != nil || len(syms) != 2 {
		t.Fatalf("FindSymbolExact = %v, %v", syms, err)
	}
	_, res, code, _ := safeToEdit(t, root, "--symbol-id", strconv.FormatInt(syms[0].ID, 10))
	if code != editsafety.ExitUnknown || len(res.Target.Symbols) != 1 || res.Target.Symbols[0].SymbolID != syms[0].ID {
		t.Fatalf("symbol_id: exit %d target %+v", code, res.Target)
	}
}

func TestSafeToEditCLINotIndexed(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code, msg := safeToEdit(t, root, "--symbol", "a.A")
	if code != editsafety.ExitInvalid || out != "" || !strings.Contains(msg, "not indexed") {
		t.Fatalf("exit %d msg %q out %q", code, msg, out)
	}
	if _, err := os.Stat(filepath.Join(root, ".codegraph")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unindexed assessment created artifacts: %v", err)
	}
}

// A constraint violation through the target is UNSAFE on a fresh graph, but a
// stale graph withholds every verdict, even with the violation in evidence.
func TestSafeToEditCLIConstraintsAndFreshness(t *testing.T) {
	root, repo := safeToEditRepo(t, map[string]string{constraints.ConfigFileName: cliConstraintsConfig})
	out, res, code, _ := safeToEdit(t, root, "--symbol", "infra.B")
	if res.Status != editsafety.StatusAssessed || verdictOf(res) != editsafety.VerdictUnsafe || res.Reason != editsafety.RuleConstraintViolation || code != 1 {
		t.Fatalf("violation: %s %s %s exit %d\n%s", res.Status, verdictOf(res), res.Reason, code, out)
	}
	if len(res.Evidence.ConstraintViolationRules) != 1 || res.Evidence.ConstraintViolationRules[0] != "domain-no-infra" {
		t.Fatalf("violation rules %v", res.Evidence.ConstraintViolationRules)
	}
	// A symbol outside every violation keeps its own verdict.
	if _, res, code, _ := safeToEdit(t, root, "--symbol", "infra.isolated"); verdictOf(res) != "null" || code != editsafety.ExitUnknown {
		t.Fatalf("isolated with constraints: %s exit %d", verdictOf(res), code)
	}

	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath, repoID := opened.DBPath, opened.Repo.ID
	opened.Close()

	// Path-scoped indexing of a new commit: no full scan covers HEAD.
	repo.Write("internal/infra/b.go", safeToEditFiles["internal/infra/b.go"]+"\nfunc Extra() {}\n")
	repo.Commit("", "two")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.New(st, newDefaultRegistry(), nil).Update(context.Background(), indexer.Options{RepoRoot: root, Paths: []string{"internal/infra/b.go"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, res, code, _ := safeToEdit(t, root, "--symbol", "infra.B"); res.Status != editsafety.StatusUnknown || res.Verdict != nil ||
		res.Reason != editsafety.ReasonInsufficientCoverage || code != editsafety.ExitInsufficientCoverage || len(res.Evidence.ConstraintViolations) == 0 {
		t.Fatalf("path-scoped: %s %s %s exit %d", res.Status, verdictOf(res), res.Reason, code)
	}
	if _, _, err := runCLI(t, "update", root); err != nil {
		t.Fatal(err)
	}
	if _, res, code, _ := safeToEdit(t, root, "--symbol", "infra.B"); verdictOf(res) != editsafety.VerdictUnsafe || code != 1 {
		t.Fatalf("after full update: %s exit %d", verdictOf(res), code)
	}

	// A queued change is known stale: exit 3, no verdict.
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.QueueDirtyFiles(context.Background(), repoID, []string{"internal/domain/a.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	out, res, code, _ = safeToEdit(t, root, "--symbol", "infra.B")
	if res.Status != editsafety.StatusUnknown || res.Verdict != nil || res.Reason != editsafety.ReasonKnownStale || code != editsafety.ExitKnownStale {
		t.Fatalf("stale: %s %s %s exit %d\n%s", res.Status, verdictOf(res), res.Reason, code, out)
	}
	if !strings.Contains(out, `"verdict": null`) || ruleOutcome(res, editsafety.RuleFreshness) != editsafety.OutcomeFired ||
		ruleOutcome(res, editsafety.RuleConstraintViolation) != editsafety.OutcomeFired {
		t.Fatalf("stale result must keep the evidence and a null verdict:\n%s", out)
	}
}

func TestSafeToEditCLIFreshnessUnknownOutsideGit(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	for rel, body := range safeToEditFiles {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatal(err)
	}
	_, res, code, _ := safeToEdit(t, root, "--symbol", "infra.B")
	if res.Status != editsafety.StatusUnknown || res.Verdict != nil || res.Reason != editsafety.ReasonFreshnessUnknown || code != editsafety.ExitUnknown ||
		res.Evidence.DependentsTotal != 1 {
		t.Fatalf("no git: %s %s %s exit %d evidence %+v", res.Status, verdictOf(res), res.Reason, code, res.Evidence)
	}
}

// Repeated queries are byte-identical across database reopens and never
// change the database file.
func TestSafeToEditCLIDeterministicAndReadOnly(t *testing.T) {
	root, _ := safeToEditRepo(t, nil)
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := opened.DBPath
	opened.Close()
	hash := func() [32]byte {
		b, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := hash()
	for _, args := range [][]string{{"--symbol", "infra.B"}, {"--file", "internal/infra/b.go"}, {"--symbol", "infra.isolated"}, {"--symbol", "util.Dup"}} {
		first, _, firstCode, _ := safeToEdit(t, append([]string{root}, args...)...)
		second, _, secondCode, _ := safeToEdit(t, append([]string{root}, args...)...)
		if first != second || firstCode != secondCode {
			t.Fatalf("%v not deterministic:\n%s\n%s", args, first, second)
		}
	}
	if hash() != before {
		t.Fatal("safe_to_edit changed the database file")
	}
}

func TestSafeToEditExitCodes(t *testing.T) {
	v := func(s string) *string { return &s }
	for _, tc := range []struct {
		res  editsafety.Result
		want int
	}{
		{editsafety.Result{Status: editsafety.StatusAssessed, Verdict: v(editsafety.VerdictSafe)}, 0},
		{editsafety.Result{Status: editsafety.StatusAssessed, Verdict: v(editsafety.VerdictCaution)}, 1},
		{editsafety.Result{Status: editsafety.StatusAssessed, Verdict: v(editsafety.VerdictUnsafe)}, 1},
		{editsafety.Result{Status: editsafety.StatusUnknown, Reason: editsafety.ReasonKnownStale}, 3},
		{editsafety.Result{Status: editsafety.StatusUnknown, Reason: editsafety.ReasonFreshnessUnknown}, 4},
		{editsafety.Result{Status: editsafety.StatusUnknown, Reason: editsafety.ReasonIsolationUnproven}, 4},
		{editsafety.Result{Status: editsafety.StatusUnknown, Reason: editsafety.ReasonParserSemanticUpgrade}, 4},
		{editsafety.Result{Status: editsafety.StatusUnknown, Reason: editsafety.ReasonInsufficientCoverage}, 5},
		// An unknown result never exits 0, even carrying a stray verdict.
		{editsafety.Result{Status: editsafety.StatusUnknown, Verdict: v(editsafety.VerdictSafe), Reason: editsafety.ReasonIsolationUnproven}, 4},
	} {
		if got := editsafety.ExitCode(tc.res); got != tc.want {
			t.Errorf("ExitCode(%+v) = %d, want %d", tc.res, got, tc.want)
		}
	}
}

// An unresolved edge from inside a file target is not a candidate for it,
// while the same edge still counts for a symbol target in that file. p.Ping()
// names S.Ping, which the same file declares.
func TestSafeToEditCLIUnresolvedCandidatesExcludeTheTargetFile(t *testing.T) {
	root, _ := safeToEditRepo(t, map[string]string{
		"internal/infra/self.go": "package infra\n\ntype S struct{}\n\nfunc (S) Ping() {}\n\nfunc useSelf(p interface{ Ping() }) { p.Ping() }\n",
	})
	_, sym, _, _ := safeToEdit(t, root, "--symbol", "infra.S.Ping")
	if sym.Evidence.UnresolvedCandidates != 1 {
		t.Fatalf("symbol target: unresolved candidates %d, want 1 (p.Ping in self.go)", sym.Evidence.UnresolvedCandidates)
	}
	_, file, _, _ := safeToEdit(t, root, "--file", "internal/infra/self.go")
	if file.Evidence.UnresolvedCandidates != 0 {
		t.Fatalf("file target: unresolved candidates %d, want 0 (its own edge)", file.Evidence.UnresolvedCandidates)
	}
}

// Ambiguity is decided on exact qualified names alone and lists a bounded
// set of candidates.
func TestSafeToEditCLIManyExactMatchesStayAmbiguous(t *testing.T) {
	extra := map[string]string{}
	for i := 0; i < 25; i++ {
		extra["internal/many/d"+strconv.Itoa(i)+"/x.go"] = "package util\n\nfunc Dup() {}\n"
	}
	root, _ := safeToEditRepo(t, extra)
	_, _, code, msg := safeToEdit(t, root, "--symbol", "util.Dup")
	if code != editsafety.ExitInvalid || !strings.Contains(msg, "ambiguous") || !strings.HasSuffix(msg, ", ...") {
		t.Fatalf("exit %d msg %q", code, msg)
	}
}

// Edges decided by an older resolver policy are not evidence: the result is
// unknown until an update re-decides them.
func TestSafeToEditCLIPendingResolverPolicy(t *testing.T) {
	root, _ := safeToEditRepo(t, nil)
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	dbPath, repoID := opened.DBPath, opened.Repo.ID
	opened.Close()
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE settings SET value = '1' WHERE key = ?`, "resolver.policy."+strconv.FormatInt(repoID, 10)+".go")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("lowered %d go policy markers, want 1", n)
	}
	db.Close()

	out, r, code, _ := safeToEdit(t, root, "--symbol", "infra.B")
	if r.Status != editsafety.StatusUnknown || r.Verdict != nil || r.Reason != editsafety.ReasonResolverPolicyPending || code != editsafety.ExitUnknown ||
		r.Evidence.DependentsTotal != 1 {
		t.Fatalf("pending policy: %s %s %s exit %d\n%s", r.Status, verdictOf(r), r.Reason, code, out)
	}
	if _, _, err := runCLI(t, "update", root); err != nil {
		t.Fatal(err)
	}
	if _, r, code, _ := safeToEdit(t, root, "--symbol", "infra.B"); verdictOf(r) != editsafety.VerdictCaution || code != 1 {
		t.Fatalf("after update: %s %s exit %d", r.Status, verdictOf(r), code)
	}
}

// Stable keys are not unique: two declarations, in different files or in one
// file, can share one. A constraint violation through one of them must not
// make the other UNSAFE, and must still make its own declaration UNSAFE.
func TestSafeToEditCLIConstraintMatchesExactDeclaration(t *testing.T) {
	const cfg = `{"schema_version":1,
	"groups":{"u1":{"include":["internal/u1/**"]},"infra":{"include":["internal/infra/**"]}},
	"rules":[{"id":"u1-no-infra","kind":"forbidden_dependency","from":["u1"],"to":["infra"]}]}`
	root, _ := safeToEditRepo(t, map[string]string{
		constraints.ConfigFileName: cfg,
		"internal/u1/x.go":         "package util\n\nimport \"example.com/m/internal/infra\"\n\nfunc Dup() { infra.B() }\n",
		"internal/u1/y.go":         "package util\n\nimport \"example.com/m/internal/infra\"\n\nfunc init() {}\n\nfunc init() { infra.C() }\n",
	})
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	find := func(name string) []graph.Symbol {
		syms, err := opened.Store.FindSymbolExact(context.Background(), opened.Repo.ID, name, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		return syms
	}
	dups, inits := find("util.Dup"), find("util.init")
	opened.Close()
	if len(dups) != 2 || dups[0].StableKey == "" || dups[0].StableKey != dups[1].StableKey {
		t.Fatalf("fixture needs two util.Dup sharing a stable key: %+v", dups)
	}
	if len(inits) != 2 || inits[0].FilePath != inits[1].FilePath || inits[0].StableKey != inits[1].StableKey {
		t.Fatalf("fixture needs two init in one file sharing a stable key: %+v", inits)
	}
	byID := func(id int64) (editsafety.Result, int) {
		_, res, code, _ := safeToEdit(t, root, "--symbol-id", strconv.FormatInt(id, 10))
		return res, code
	}
	for _, pair := range [][]graph.Symbol{dups, inits} {
		for _, s := range pair {
			res, code := byID(s.ID)
			violates := s.FilePath == "internal/u1/x.go" || (s.Name == "init" && s.Range.StartLine > 5)
			got := ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired
			if got != violates {
				t.Fatalf("%s %s:%d constraint fired=%v want %v (exit %d)\n%+v", s.QualifiedName, s.FilePath, s.Range.StartLine, got, violates, code, res.Evidence)
			}
			if violates && (verdictOf(res) != editsafety.VerdictUnsafe || code != 1) {
				t.Fatalf("%s %s:%d: %s exit %d", s.QualifiedName, s.FilePath, s.Range.StartLine, verdictOf(res), code)
			}
		}
	}
	// A file target matches by exact path only.
	for file, want := range map[string]bool{"internal/u1/x.go": true, "internal/u2/x.go": false, "internal/u1/y.go": true} {
		_, res, _, _ := safeToEdit(t, root, "--file", file)
		if got := ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired; got != want {
			t.Fatalf("file %s constraint fired=%v want %v", file, got, want)
		}
	}
}

// TestSafeToEditCLIContainerIncludesNestedMembers pins the scope of a symbol
// target that contains declarations: editing a class edits the methods and
// classes declared inside its source span, so a violation by one of them is
// the class's violation too. Declarations outside the span (a same-named
// method of another class, the same class text in another file) are not.
func TestSafeToEditCLIContainerIncludesNestedMembers(t *testing.T) {
	const cfg = `{"schema_version":1,
	"groups":{"domain":{"include":["internal/domain/**"]},"infra":{"include":["internal/infra/**"]}},
	"rules":[{"id":"domain-no-infra","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}]}`
	const svc = `from internal.infra.helpers import helper


class Service:
    def run(self):
        helper()


class Other:
    def run(self):
        return 1


class Outer:
    class Inner:
        def go(self):
            helper()


class Clean:
    def run(self):
        return 2
`
	root, _ := safeToEditRepo(t, map[string]string{
		constraints.ConfigFileName:       cfg,
		"internal/__init__.py":           "",
		"internal/infra/__init__.py":     "",
		"internal/infra/helpers.py":      "def helper():\n    return 0\n",
		"internal/domain/__init__.py":    "",
		"internal/domain/svc.py":         svc,
		"internal/domain/use.py":         "from internal.domain.svc import Service, Other, Outer, Clean\n\n\ndef use():\n    Service()\n    Other()\n    Outer()\n    Clean()\n",
		"internal/elsewhere/__init__.py": "",
		"internal/elsewhere/svc.py":      svc,
		"internal/elsewhere/use.py":      "from internal.elsewhere.svc import Service\n\n\ndef use():\n    Service()\n",
	})
	opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
	if err != nil {
		t.Fatal(err)
	}
	ft, err := opened.Store.EditSafetyFile(context.Background(), opened.Repo.ID, "internal/domain/svc.py")
	opened.Close()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, s := range ft.Symbols {
		ids[s.Kind+" "+s.Name+":"+strconv.Itoa(s.Range.StartLine)] = s.ID
	}
	idOf := func(key string) string {
		id, ok := ids[key]
		if !ok {
			t.Fatalf("no symbol %q in %v", key, ids)
		}
		return strconv.FormatInt(id, 10)
	}
	for _, tc := range []struct {
		key      string
		violates bool
	}{
		{"class Service:4", true},
		{"class Outer:14", true},
		{"class Inner:15", true},
		{"class Other:9", false},
		{"class Clean:20", false},
	} {
		out, res, code, _ := safeToEdit(t, root, "--symbol-id", idOf(tc.key))
		fired := ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired
		if fired != tc.violates {
			t.Fatalf("%s: constraint fired=%v want %v\n%s", tc.key, fired, tc.violates, out)
		}
		if tc.violates {
			if res.Status != editsafety.StatusAssessed || verdictOf(res) != editsafety.VerdictUnsafe || code != 1 {
				t.Fatalf("%s: %s %s exit %d\n%s", tc.key, res.Status, verdictOf(res), code, out)
			}
			for _, v := range res.Evidence.ConstraintViolations {
				if v.Source.Path != "internal/domain/svc.py" {
					t.Fatalf("%s: violation from another file %+v", tc.key, v)
				}
			}
		} else if verdictOf(res) == editsafety.VerdictUnsafe {
			t.Fatalf("%s: unrelated nested symbol made it UNSAFE\n%s", tc.key, out)
		}
	}
	// The same class text in an unconstrained directory takes no violation
	// from its twin.
	if _, res, _, _ := safeToEdit(t, root, "--file", "internal/elsewhere/svc.py"); ruleOutcome(res, editsafety.RuleConstraintViolation) == editsafety.OutcomeFired {
		t.Fatalf("elsewhere file fired: %+v", res.Evidence)
	}
	// Repeated runs give byte-identical output.
	a, _, _, _ := safeToEdit(t, root, "--symbol-id", idOf("class Outer:14"))
	b, _, _, _ := safeToEdit(t, root, "--symbol-id", idOf("class Outer:14"))
	if a != b {
		t.Fatalf("output is not deterministic")
	}
}
