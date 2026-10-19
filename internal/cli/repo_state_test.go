package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

// sourceRepo creates an unindexed git repository holding one Go file.
func sourceRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc Hello() {}\n\nfunc main() { Hello() }\n")
	git(t, root, "init", "-q")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// treeListing lists every path under root except .git, so a before/after
// comparison catches any file or directory a command created.
func treeListing(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func assertUntouched(t *testing.T, root string, before []string) {
	t.Helper()
	if after := treeListing(t, root); !slices.Equal(before, after) {
		t.Fatalf("tree under %s changed:\nbefore %v\nafter  %v", root, before, after)
	}
}

func repoStateCLI(t *testing.T) {
	t.Helper()
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
}

type serveResult struct {
	responses map[float64]map[string]any
	stderr    string
}

// serveSession runs serve over the given tool calls (after initialize and
// tools/list) and indexes the responses by request id.
func serveSession(t *testing.T, args []string, calls ...string) (serveResult, error) {
	t.Helper()
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}
	for i, call := range calls {
		lines = append(lines, `{"jsonrpc":"2.0","id":`+jsonInt(10+i)+`,"method":"tools/call","params":`+call+`}`)
	}
	var stdout, stderr bytes.Buffer
	err := runServeWith(context.Background(), config.Config{DBDir: config.RepoDBDir},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &stdout, &stderr, args)
	res := serveResult{responses: map[float64]map[string]any{}, stderr: stderr.String()}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var resp map[string]any
		if jerr := json.Unmarshal([]byte(line), &resp); jerr != nil {
			t.Fatalf("bad response %q: %v", line, jerr)
		}
		id, _ := resp["id"].(float64)
		res.responses[id] = resp
	}
	return res, err
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

// toolText returns the text of a tools/call response and whether it is an error.
func (r serveResult) toolText(t *testing.T, id float64) (string, bool) {
	t.Helper()
	resp, ok := r.responses[id]
	if !ok {
		t.Fatalf("no response for id %v; stderr: %s", id, r.stderr)
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("response %v has no content: %v", id, resp)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func (r serveResult) toolCount(t *testing.T) int {
	t.Helper()
	result, _ := r.responses[2]["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	return len(tools)
}

const (
	callFindHello  = `{"name":"find_symbol","arguments":{"query":"Hello"}}`
	callGraphStats = `{"name":"graph_stats","arguments":{}}`
	callUpdate     = `{"name":"update_graph","arguments":{}}`
	callIndex      = `{"name":"index_repo","arguments":{}}`
)

func TestServeExplicitUnindexedRootWritesNothing(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	before := treeListing(t, root)

	res, err := serveSession(t, []string{"--repo-root", root}, callFindHello, callGraphStats, callUpdate,
		`{"name":"usage_stats","arguments":{}}`)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if res.toolCount(t) == 0 {
		t.Fatal("tools/list advertised no tools")
	}
	for _, id := range []float64{10, 11, 12} {
		text, isErr := res.toolText(t, id)
		if !isErr || !strings.Contains(text, "not indexed yet") || !strings.Contains(text, "index_repo") {
			t.Errorf("call %v = %q (error %v), want a not-indexed refusal naming index_repo", id, text, isErr)
		}
	}
	if _, isErr := res.toolText(t, 13); isErr {
		t.Error("usage_stats failed before indexing")
	}
	if !strings.Contains(res.stderr, "from --repo-root") || !strings.Contains(res.stderr, "not indexed yet") {
		t.Errorf("stderr does not explain the selected root: %q", res.stderr)
	}
	assertUntouched(t, root, before)
}

func TestServeIndexRepoCreatesThenLaterSessionReuses(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)

	res, err := serveSession(t, []string{"--repo-root", root}, callIndex, callFindHello)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if text, isErr := res.toolText(t, 10); isErr {
		t.Fatalf("index_repo failed: %s", text)
	}
	if text, isErr := res.toolText(t, 11); isErr || !strings.Contains(text, "Hello") {
		t.Fatalf("find_symbol after index_repo = %q (error %v)", text, isErr)
	}
	canonical, _ := store.CanonicalRepoPath(root)
	dbPath := filepath.Join(canonical, config.RepoArtifactsDir, store.RepoDatabaseFileName)
	first, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("index_repo did not create the database: %v", err)
	}

	res, err = serveSession(t, []string{root}, callFindHello)
	if err != nil {
		t.Fatalf("second serve: %v", err)
	}
	if text, isErr := res.toolText(t, 10); isErr || !strings.Contains(text, "Hello") {
		t.Fatalf("reused session find_symbol = %q (error %v)", text, isErr)
	}
	if !strings.Contains(res.stderr, "graph database "+dbPath) {
		t.Errorf("stderr does not name the reused database: %q", res.stderr)
	}
	second, err := os.Stat(dbPath)
	if err != nil || !os.SameFile(first, second) {
		t.Fatalf("database was re-created instead of reused: %v", err)
	}
}

// assertInactive checks a session that found no repository: it connects, and
// every tool, index_repo included, explains how to select one.
func assertInactive(t *testing.T, res serveResult, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if res.toolCount(t) == 0 {
		t.Fatal("tools/list advertised no tools")
	}
	if len(res.responses) < 3 {
		t.Fatalf("got %d responses, want tool calls answered", len(res.responses))
	}
	for id := range res.responses {
		if id < 10 {
			continue
		}
		text, isErr := res.toolText(t, id)
		if !isErr || !strings.Contains(text, "no active repository") || !strings.Contains(text, want) {
			t.Errorf("call %v = %q (error %v), want no active repository / %q", id, text, isErr, want)
		}
	}
	if !strings.Contains(res.stderr, "no active repository") {
		t.Errorf("stderr does not explain the missing repository: %q", res.stderr)
	}
}

func TestServeWithoutRootLeavesUnindexedGitToplevelInactive(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	writeFile(t, filepath.Join(root, "sub", "x.go"), "package sub\n")
	before := treeListing(t, root)
	t.Chdir(filepath.Join(root, "sub"))

	res, err := serveSession(t, nil, callFindHello, callIndex, `{"name":"find_symbol","arguments":{"query":"Hello","format":"compact"}}`)
	assertInactive(t, res, err, "--repo-root")
	assertUntouched(t, root, before)
}

func TestServeWithoutRootOutsideGitNeverFallsBackToCwd(t *testing.T) {
	repoStateCLI(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	before := treeListing(t, dir)
	t.Chdir(dir)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))

	res, err := serveSession(t, nil, callFindHello, callIndex, callUpdate,
		`{"name":"index_repo","arguments":{"repo_root":`+jsonString(dir)+`}}`,
		`{"name":"index_repo","arguments":{"repo_root":"/"}}`)
	assertInactive(t, res, err, "not inside a git repository")
	assertUntouched(t, dir, before)
}

func TestServeWithoutRootAdoptsIndexedGitToplevel(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatalf("index: %v", err)
	}
	writeFile(t, filepath.Join(root, "sub", "x.go"), "package sub\n")
	t.Chdir(filepath.Join(root, "sub"))

	res, err := serveSession(t, nil, callFindHello)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if text, isErr := res.toolText(t, 10); isErr || !strings.Contains(text, "Hello") {
		t.Fatalf("find_symbol = %q (error %v)", text, isErr)
	}
	if !strings.Contains(res.stderr, "git toplevel") {
		t.Errorf("stderr does not give the reason: %q", res.stderr)
	}
}

// A linked worktree is its own repository: it never borrows the main
// checkout's database, with or without an explicit root.
func TestServeWorktreeNeverSharesMainCheckoutDatabase(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "init")
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatalf("index: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", wt)
	before := treeListing(t, wt)
	t.Chdir(wt)

	res, err := serveSession(t, nil, callFindHello, callIndex, callGraphStats)
	assertInactive(t, res, err, wt)
	res, err = serveSession(t, []string{"--repo-root", wt}, callFindHello)
	if err != nil {
		t.Fatalf("serve --repo-root worktree: %v", err)
	}
	if text, isErr := res.toolText(t, 10); !isErr || !strings.Contains(text, "not indexed yet") {
		t.Fatalf("worktree find_symbol = %q (error %v), want not indexed", text, isErr)
	}
	assertUntouched(t, wt, before)
}

func TestServeRefusesAmbiguousOrMissingRoots(t *testing.T) {
	repoStateCLI(t)
	a, b := sourceRepo(t), sourceRepo(t)
	if _, err := serveSession(t, []string{"--repo-root", a, b}); err == nil || !strings.Contains(err.Error(), "one repository root") {
		t.Fatalf("two roots error = %v", err)
	}
	if _, err := serveSession(t, []string{a, b}); err == nil || !strings.Contains(err.Error(), "one repository root") {
		t.Fatalf("two positional roots error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := serveSession(t, []string{"--repo-root", missing}); err == nil {
		t.Fatal("serve accepted a missing root")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("serve created the missing root: %v", err)
	}
}

// A moved repository carries a database recorded for its old root. It is
// reported, never rebound by inserting a second repo row.
func TestDatabaseRecordedForAnotherRootIsReportedNotRebound(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatalf("index: %v", err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}

	res, err := serveSession(t, []string{"--repo-root", moved}, callFindHello, callIndex)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	for _, id := range []float64{10, 11} {
		if text, isErr := res.toolText(t, id); !isErr || !strings.Contains(text, "holds the graph of") {
			t.Errorf("call %v = %q (error %v), want the recorded root reported", id, text, isErr)
		}
	}
	if _, _, err := runCLI(t, "stats", moved); err == nil || !strings.Contains(err.Error(), "holds the graph of") {
		t.Fatalf("stats error = %v, want the recorded root reported", err)
	}
	canonical, _ := store.CanonicalRepoPath(moved)
	s, err := store.OpenReadOnly(filepath.Join(canonical, config.RepoArtifactsDir, store.RepoDatabaseFileName), store.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repos, err := s.ListRepos(context.Background(), 10, 0)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos = %v (%v), want only the original row", repos, err)
	}
}

func TestSymlinkedRootUsesCanonicalDatabase(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatalf("index: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	canonical, _ := store.CanonicalRepoPath(root)
	res, err := serveSession(t, []string{"--repo-root", link}, callFindHello)
	if err != nil {
		t.Fatalf("serve via symlink: %v", err)
	}
	if text, isErr := res.toolText(t, 10); isErr || !strings.Contains(text, "Hello") {
		t.Fatalf("find_symbol via symlink = %q (error %v)", text, isErr)
	}
	if !strings.Contains(res.stderr, "serving "+canonical) {
		t.Errorf("stderr does not name the canonical root %s: %q", canonical, res.stderr)
	}
	if _, _, err := runCLI(t, "stats", link); err != nil {
		t.Fatalf("stats via symlink: %v", err)
	}
}

func TestReadOnlyCommandsOnUnindexedRepoCreateNothing(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	before := treeListing(t, root)
	out := filepath.Join(t.TempDir(), "graph.html")

	notIndexed := [][]string{
		{"stats", root},
		{"find_symbol", root, "Hello"},
		{"find_callers", root, "Hello"},
		{"get_impact_radius", root, "--symbol", "Hello"},
		{"graph", "export", root},
		{"visualize", "--repo-root", root, "--output", out},
		{"affected-tests", "--repo-root", root, "main.go"},
		{"clean", root},
		{"audit", root},
		{"recover-scans", root},
	}
	for _, args := range notIndexed {
		if _, _, err := runCLI(t, args...); err == nil || !strings.Contains(err.Error(), "not indexed") {
			t.Errorf("%v error = %v, want not indexed", args, err)
		}
		assertUntouched(t, root, before)
	}
	for _, args := range [][]string{{"doctor", root}, {"version"}, {"--help"}, {"stats", "--help"}} {
		_, _, _ = runCLI(t, args...)
		assertUntouched(t, root, before)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("visualize wrote output for an unindexed repo: %v", err)
	}
}

func TestExplicitInitAndIndexCreateState(t *testing.T) {
	repoStateCLI(t)
	for _, cmd := range []string{"init", "index"} {
		root := sourceRepo(t)
		if _, _, err := runCLI(t, cmd, root); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		canonical, _ := store.CanonicalRepoPath(root)
		if _, err := os.Stat(filepath.Join(canonical, config.RepoArtifactsDir, store.RepoDatabaseFileName)); err != nil {
			t.Fatalf("%s did not create the database: %v", cmd, err)
		}
		if _, _, err := runCLI(t, "stats", root); err != nil {
			t.Fatalf("stats after %s: %v", cmd, err)
		}
	}
}

// CodeGraph's own state is never indexed, even when a persisted repo config
// carries an exclude list that predates the current defaults.
func TestIndexingExcludesCodeGraphState(t *testing.T) {
	repoStateCLI(t)
	root := sourceRepo(t)
	writeFile(t, filepath.Join(root, config.RepoArtifactsDir, "config.json"), `{"exclude":[]}`)
	writeFile(t, filepath.Join(root, config.RepoArtifactsDir, "stray.go"), "package stray\n\nfunc Stray() {}\n")
	for _, name := range []string{"codegraph.v2.sqlite.scan.lock", "codegraph.v2-0123456789abcdef.sqlite", "codegraph.v2-0123456789abcdef.sqlite-wal", "codegraph.sqlite-journal"} {
		writeFile(t, filepath.Join(root, name), "x")
	}
	out, _, err := runCLI(t, "index", root)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var result struct {
		Summary store.ScanSummary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("index output: %v\n%s", err, out)
	}
	if result.Summary.FilesSeen != 1 {
		t.Fatalf("files_seen = %d, want only main.go", result.Summary.FilesSeen)
	}

	for _, rel := range []string{".codegraph/stray.go", ".codegraph/codegraph.v2.sqlite-wal", "sub/codegraph.v2-0123456789abcdef.sqlite-shm", "codegraph.v2.sqlite.scan.lock"} {
		if !indexer.ShouldIgnorePath(rel, nil) && !indexer.ShouldSkipFile(rel, nil, nil) {
			t.Errorf("%s is not excluded", rel)
		}
	}

	out, _, err = runCLI(t, "update", root)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	result.Summary = store.ScanSummary{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("update output: %v\n%s", err, out)
	}
	if result.Summary.ScanID == 0 || result.Summary.FilesChanged != 0 || result.Summary.FilesIndexed != 0 {
		t.Fatalf("update of an unchanged tree rewrote files: %+v", result.Summary)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func dbArtifacts(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if b, err := os.ReadFile(p); err == nil {
			out[p] = string(b)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return out
}

func storageConfigs(t *testing.T) map[string]config.Config {
	return map[string]config.Config{
		"repo-local": {DBDir: config.RepoDBDir},
		"central":    {DBDir: filepath.Join(t.TempDir(), "central")},
	}
}

func createRepoDatabase(t *testing.T, cfg config.Config, root string) string {
	t.Helper()
	app, _, _, err := openApp(context.Background(), cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	canonical, _ := store.CanonicalRepoPath(root)
	dbPath, err := existingRepoDBPath(cfg, root, canonical)
	if err != nil || dbPath == "" {
		t.Fatalf("existingRepoDBPath = %q, %v", dbPath, err)
	}
	return dbPath
}

// A database deleted after existingRepoDBPath found it is reported as not
// indexed; neither it nor its directory is recreated.
func TestOpenIndexedAppNeverRecreatesVanishedDatabase(t *testing.T) {
	repoStateCLI(t)
	for name, cfg := range storageConfigs(t) {
		root := sourceRepo(t)
		dbPath := createRepoDatabase(t, cfg, root)
		dir := filepath.Dir(dbPath)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openIndexedApp(context.Background(), cfg, root, dbPath); !errors.Is(err, store.ErrRepoNotIndexed) {
			t.Errorf("%s: err = %v, want not indexed", name, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: database directory recreated: %v", name, err)
		}
	}
}

func TestOpenIndexedAppRejectsForeignDatabaseUnchanged(t *testing.T) {
	repoStateCLI(t)
	for name, cfg := range storageConfigs(t) {
		owner := sourceRepo(t)
		ownerDB := createRepoDatabase(t, cfg, owner)
		other := sourceRepo(t)
		canonical, _ := store.CanonicalRepoPath(other)
		paths, err := repoDBPathsForRepo(cfg, other, canonical)
		if err != nil {
			t.Fatal(err)
		}
		foreign := paths[0]
		data, err := os.ReadFile(ownerDB)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, foreign, string(data))
		want := dbArtifacts(t, foreign)

		_, _, err = openIndexedApp(context.Background(), cfg, other, foreign)
		if !errors.As(err, new(repoMismatchError)) {
			t.Errorf("%s: err = %v, want repoMismatchError", name, err)
		}
		if got := dbArtifacts(t, foreign); !maps.Equal(want, got) {
			t.Errorf("%s: foreign database changed", name)
		}
		if app, _, err := openIndexedApp(context.Background(), cfg, owner, ownerDB); err != nil {
			t.Errorf("%s: owner open: %v", name, err)
		} else {
			app.Close()
		}
	}
}
