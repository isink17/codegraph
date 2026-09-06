package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/agent"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// containmentEnv builds an MCP server whose active repository is A, plus a
// sibling repository B that no tool argument may reach.
func containmentEnv(t *testing.T) (*Server, *store.Store, int64, string, string) {
	t.Helper()
	ctx := context.Background()
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	repoB := filepath.Join(parent, "repo-b")
	writeRepoFile(t, repoA, filepath.Join("src", "a.go"), "package src\n\nfunc InsideA() {}\n")
	writeRepoFile(t, repoB, "secret.go", "package repob\n\nfunc OutsideBSecret() {}\n")

	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoA)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoA}); err != nil {
		t.Fatalf("Index(A) error = %v", err)
	}
	return NewServer(repoA, repo.ID, s, idx, query.New(s, nil), io.Discard), s, repo.ID, repoA, repoB
}

func indexArgs(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return raw
}

// mcpGraphState snapshots everything a refused call must leave untouched.
func mcpGraphState(t *testing.T, s *store.Store, repoID int64) (repos []string, files []string, scans int) {
	t.Helper()
	ctx := context.Background()
	repoRows, err := s.ListRepos(ctx, 100, 0)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	for _, r := range repoRows {
		repos = append(repos, r.RootPath)
	}
	if files, err = s.AllFilePaths(ctx, repoID); err != nil {
		t.Fatalf("AllFilePaths() error = %v", err)
	}
	scanRows, err := s.ListScans(ctx, repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ListScans() error = %v", err)
	}
	return repos, files, len(scanRows)
}

// TestRepoRootContainmentMatrix is P22.31 item 25: which repository roots a tool
// call may name. The active repository may be asserted under any alias that
// really identifies it; nothing else is accepted, and a subdirectory of the
// active root is not the active root.
func TestRepoRootContainmentMatrix(t *testing.T) {
	server, _, _, repoA, repoB := containmentEnv(t)

	accepted := map[string]string{
		"exact active root":  repoA,
		"cleaned dot alias":  filepath.Join(repoA, "."),
		"cleaned parent hop": filepath.Join(repoA, "src", ".."),
		"trailing separator": repoA + string(filepath.Separator),
	}
	for name, root := range accepted {
		t.Run("accept/"+name, func(t *testing.T) {
			if err := server.assertActiveRepo(root); err != nil {
				t.Fatalf("assertActiveRepo(%q) error = %v, want accept", root, err)
			}
		})
	}

	rejected := map[string]string{
		"other repository":    repoB,
		"parent directory":    filepath.Dir(repoA),
		"subdirectory":        filepath.Join(repoA, "src"),
		"nonexistent sibling": filepath.Join(filepath.Dir(repoA), "repo-missing"),
		"prefix confusable":   repoA + "2",
		"relative traversal":  filepath.Join("..", "repo-b"),
	}
	for name, root := range rejected {
		t.Run("reject/"+name, func(t *testing.T) {
			err := server.assertActiveRepo(root)
			if err == nil {
				t.Fatalf("assertActiveRepo(%q) = nil, want rejection", root)
			}
			if !errors.Is(err, ErrRepositoryScopeViolation) {
				t.Fatalf("assertActiveRepo(%q) error = %v, want ErrRepositoryScopeViolation", root, err)
			}
			var scopeErr *RepositoryScopeError
			if !errors.As(err, &scopeErr) {
				t.Fatalf("error is not *RepositoryScopeError: %v", err)
			}
			if scopeErr.Active != repoA {
				t.Fatalf("RepositoryScopeError.Active = %q, want %q", scopeErr.Active, repoA)
			}
		})
	}
}

// TestRepoRootSymlinkAlias covers the identity requirement: a symlink that
// really is the active repository is accepted, one pointing elsewhere is not.
// String normalization alone cannot tell these apart.
func TestRepoRootSymlinkAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	server, _, _, repoA, repoB := containmentEnv(t)
	linkDir := t.TempDir()

	aliasA := filepath.Join(linkDir, "alias-a")
	if err := os.Symlink(repoA, aliasA); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := server.assertActiveRepo(aliasA); err != nil {
		t.Fatalf("assertActiveRepo(symlink to active) error = %v, want accept", err)
	}

	aliasB := filepath.Join(linkDir, "alias-b")
	if err := os.Symlink(repoB, aliasB); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := server.assertActiveRepo(aliasB); !errors.Is(err, ErrRepositoryScopeViolation) {
		t.Fatalf("assertActiveRepo(symlink to other repo) error = %v, want ErrRepositoryScopeViolation", err)
	}
}

// TestRepoRootRepoPathConflictMatrix is P22.31 item 24. Both fields are
// validated, so a conflicting second field can never be silently ignored the way
// first-non-empty-wins ignored it.
func TestRepoRootRepoPathConflictMatrix(t *testing.T) {
	server, _, _, repoA, repoB := containmentEnv(t)

	cases := []struct {
		name          string
		repoRoot      string
		repoPath      string
		wantRejection bool
	}{
		{name: "neither field", wantRejection: false},
		{name: "root=A", repoRoot: repoA, wantRejection: false},
		{name: "path=A", repoPath: repoA, wantRejection: false},
		{name: "root=A path=A", repoRoot: repoA, repoPath: repoA, wantRejection: false},
		{name: "root=A path=B", repoRoot: repoA, repoPath: repoB, wantRejection: true},
		{name: "root=B path=A", repoRoot: repoB, repoPath: repoA, wantRejection: true},
		{name: "root=B path=B", repoRoot: repoB, repoPath: repoB, wantRejection: true},
		{name: "root=B", repoRoot: repoB, wantRejection: true},
		{name: "path=B", repoPath: repoB, wantRejection: true},
		{name: "blank fields fall back to active", repoRoot: "  ", repoPath: "  ", wantRejection: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := server.assertActiveRepoArgs(tc.repoRoot, tc.repoPath)
			if tc.wantRejection {
				if !errors.Is(err, ErrRepositoryScopeViolation) {
					t.Fatalf("assertActiveRepoArgs(%q, %q) error = %v, want ErrRepositoryScopeViolation", tc.repoRoot, tc.repoPath, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("assertActiveRepoArgs(%q, %q) error = %v, want accept", tc.repoRoot, tc.repoPath, err)
			}
		})
	}
}

// TestIndexRepoRootEscapeRejectedZeroMutation is the regression for the original
// vulnerability: index_repo{"repo_root": "<repo-b>"} used to succeed, create a
// repo row for B, scan B's filesystem, and land OutsideBSecret in the store this
// server opened for A.
func TestIndexRepoRootEscapeRejectedZeroMutation(t *testing.T) {
	ctx := context.Background()
	server, s, repoID, _, repoB := containmentEnv(t)
	reposBefore, filesBefore, scansBefore := mcpGraphState(t, s, repoID)

	for _, tool := range []string{"index_repo", "update_graph"} {
		for _, field := range []string{"repo_root", "repo_path"} {
			t.Run(tool+"/"+field, func(t *testing.T) {
				_, err := server.callTool(ctx, tool, indexArgs(t, map[string]any{field: repoB}))
				if !errors.Is(err, ErrRepositoryScopeViolation) {
					t.Fatalf("%s(%s=B) error = %v, want ErrRepositoryScopeViolation", tool, field, err)
				}
				reposAfter, filesAfter, scansAfter := mcpGraphState(t, s, repoID)
				if len(reposAfter) != len(reposBefore) {
					t.Fatalf("repos mutated: %v -> %v", reposBefore, reposAfter)
				}
				if len(filesAfter) != len(filesBefore) {
					t.Fatalf("files mutated: %v -> %v", filesBefore, filesAfter)
				}
				if scansAfter != scansBefore {
					t.Fatalf("rejected call recorded a scan: %d -> %d", scansBefore, scansAfter)
				}
			})
		}
	}
}

// TestIndexRepoRootEscapePrecedesConfigLoad is P22.31 item 10. Repo B carries a
// malformed CodeGraph config; the caller must see the scope violation, never a
// parse error from B, which would prove containment ran after config.LoadRepo.
func TestIndexRepoRootEscapePrecedesConfigLoad(t *testing.T) {
	server, _, _, _, repoB := containmentEnv(t)
	writeRepoFile(t, repoB, filepath.Join(".codegraph", "config.json"), "{ this is not json")

	_, err := server.callTool(context.Background(), "index_repo", indexArgs(t, map[string]any{"repo_root": repoB}))
	if !errors.Is(err, ErrRepositoryScopeViolation) {
		t.Fatalf("index_repo(repo_root=B) error = %v, want ErrRepositoryScopeViolation", err)
	}
	if msg := err.Error(); strings.Contains(msg, "json") || strings.Contains(msg, "unmarshal") {
		t.Fatalf("error leaked repo B config parsing, so containment ran too late: %v", msg)
	}
}

// TestUpdateGraphPathEscapeRejectedZeroMutation is P22.31 item 27: a path-scoped
// call cannot smuggle an outside file through even with repo_root left alone.
func TestUpdateGraphPathEscapeRejectedZeroMutation(t *testing.T) {
	ctx := context.Background()
	server, s, repoID, _, repoB := containmentEnv(t)
	_, filesBefore, scansBefore := mcpGraphState(t, s, repoID)

	for _, path := range []string{
		filepath.Join("..", "repo-b", "secret.go"),
		filepath.Join("src", "..", "..", "repo-b", "secret.go"),
		filepath.Join(repoB, "secret.go"),
		filepath.Join("..", "repo-b", "deleted.go"),
	} {
		t.Run(path, func(t *testing.T) {
			_, err := server.callTool(ctx, "update_graph", indexArgs(t, map[string]any{"paths": []string{path}}))
			if !errors.Is(err, indexer.ErrPathOutsideRepo) {
				t.Fatalf("update_graph(paths=[%q]) error = %v, want ErrPathOutsideRepo", path, err)
			}
			_, filesAfter, scansAfter := mcpGraphState(t, s, repoID)
			if scansAfter != scansBefore {
				t.Fatalf("rejected call recorded a scan: %d -> %d", scansBefore, scansAfter)
			}
			if len(filesAfter) != len(filesBefore) {
				t.Fatalf("files mutated: %v -> %v", filesBefore, filesAfter)
			}
			for _, p := range filesAfter {
				if strings.HasPrefix(p, "..") || filepath.IsAbs(p) {
					t.Fatalf("persisted escaping path %q", p)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repoB, "secret.go")); err != nil {
		t.Fatalf("repo B was disturbed: %v", err)
	}
}

// TestIndexToolsSameRepoRegression is P22.31 item 28: hardening must not cost
// any legitimate shape.
func TestIndexToolsSameRepoRegression(t *testing.T) {
	ctx := context.Background()
	server, s, repoID, repoA, _ := containmentEnv(t)
	relative := filepath.Join("src", "a.go")

	calls := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"index_repo no repo args", "index_repo", map[string]any{}},
		{"update_graph same-root repo_root", "update_graph", map[string]any{"repo_root": repoA}},
		{"update_graph same-root repo_path", "update_graph", map[string]any{"repo_path": repoA}},
		{"update_graph contained relative path", "update_graph", map[string]any{"paths": []string{relative}}},
		{"update_graph contained absolute path", "update_graph", map[string]any{"paths": []string{filepath.Join(repoA, relative)}}},
		{"update_graph interior traversal path", "update_graph", map[string]any{"paths": []string{filepath.Join("src", "x", "..", "a.go")}}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			res, err := server.callTool(ctx, tc.tool, indexArgs(t, tc.args))
			if err != nil {
				t.Fatalf("%s error = %v, want success", tc.tool, err)
			}
			if res["ok"] != true {
				t.Fatalf("%s response = %v, want ok", tc.tool, res)
			}
			_, files, _ := mcpGraphState(t, s, repoID)
			if len(files) != 1 || files[0] != relative {
				t.Fatalf("after %s files = %v, want [%q]", tc.tool, files, relative)
			}
		})
	}
}

// TestGatewayIndexEscapeRejected is P22.31 item 22. The gateway reaches
// index_repo through tool_call, which re-enters the same dispatcher, so it must
// inherit containment without the gateway holding a copy of the rule.
func TestGatewayIndexEscapeRejected(t *testing.T) {
	server, s, repoID, _, repoB := containmentEnv(t)
	if err := server.SetToolMode(ToolModeGateway); err != nil {
		t.Fatalf("SetToolMode() error = %v", err)
	}
	reposBefore, _, scansBefore := mcpGraphState(t, s, repoID)

	responses := rpcRoundTrip(t, server,
		initializeRequest(1),
		map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{
				"name": "tool_call",
				"arguments": map[string]any{
					"name":      "index_repo",
					"arguments": map[string]any{"repo_root": repoB},
				},
			},
		},
	)
	last := responses[len(responses)-1]
	result, ok := last["result"].(map[string]any)
	if !ok {
		t.Fatalf("gateway response has no result: %v", last)
	}
	if result["isError"] != true {
		t.Fatalf("gateway tool_call(index_repo, repo_root=B) succeeded: %v", result)
	}
	if body := containmentResponseText(t, result); !strings.Contains(body, "repository scope violation") {
		t.Fatalf("gateway error text = %q, want a scope violation", body)
	}

	reposAfter, _, scansAfter := mcpGraphState(t, s, repoID)
	if len(reposAfter) != len(reposBefore) || scansAfter != scansBefore {
		t.Fatalf("gateway rejection mutated state: repos %v -> %v, scans %d -> %d",
			reposBefore, reposAfter, scansBefore, scansAfter)
	}
}

// TestAgenticQueryLoopCannotEscape drives the real agent.Agent ReAct loop with a
// stubbed LLM that tries to retarget the server, wiring it to the same tool
// function handleAgenticQuery uses (server.go: `return s.callTool(...)`).
//
// The agent prompt advertises read tools only, but that list is prompt text, not
// a gate: dispatchTool accepts any registry name, so a model emitting
// `Action: index_repo` does reach handleIndex. What must hold is that it lands
// on the contained handler and the refusal comes back as an observation instead
// of an indexed repository. The LLM backend itself is out of the loop here --
// per P22.31 item 23, the call path is structurally shared and unit-tested.
func TestAgenticQueryLoopCannotEscape(t *testing.T) {
	server, s, repoID, _, repoB := containmentEnv(t)
	reposBefore, _, scansBefore := mcpGraphState(t, s, repoID)

	step := 0
	llm := func(ctx context.Context, prompt string) (string, error) {
		step++
		if step == 1 {
			return "Thought: retarget the server\n" +
				"Action: index_repo\n" +
				"Args: {\"repo_root\": " + strconv.Quote(repoB) + "}", nil
		}
		return "Thought: done\nAnswer: finished", nil
	}
	toolFn := func(ctx context.Context, name string, args json.RawMessage) (map[string]any, error) {
		return server.callTool(ctx, name, args)
	}

	result, err := agent.New(llm, toolFn, 3).Run(context.Background(), "index another repository")
	if err != nil {
		t.Fatalf("agent.Run() error = %v", err)
	}
	if len(result.Steps) == 0 || result.Steps[0].Action != "index_repo" {
		t.Fatalf("agent did not reach index_repo: %+v", result.Steps)
	}
	if got := result.Steps[0].Result; !strings.Contains(got, "repository scope violation") {
		t.Fatalf("agent step observation = %q, want a scope violation", got)
	}

	reposAfter, _, scansAfter := mcpGraphState(t, s, repoID)
	if len(reposAfter) != len(reposBefore) || scansAfter != scansBefore {
		t.Fatalf("agent loop mutated state: repos %v -> %v, scans %d -> %d",
			reposBefore, reposAfter, scansBefore, scansAfter)
	}
}

func containmentResponseText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no content: %v", result)
	}
	var sb strings.Builder
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}
