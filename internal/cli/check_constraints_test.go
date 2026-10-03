package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/store"
)

const cliConstraintsConfig = `{"schema_version":1,
	"groups":{"domain":{"include":["internal/domain/**"]},"infra":{"include":["internal/infra/**"]}},
	"rules":[{"id":"domain-no-infra","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}]}`

// constraintsRepo writes a small Go module whose domain package calls infra.
func constraintsRepo(t *testing.T, withConfig bool) string {
	t.Helper()
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	files := map[string]string{
		"go.mod":               "module example.com/m\n\ngo 1.22\n",
		"internal/domain/a.go": "package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc A() { infra.B() }\n",
		"internal/infra/b.go":  "package infra\n\nfunc B() {}\n",
	}
	if withConfig {
		files[constraints.ConfigFileName] = cliConstraintsConfig
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// checkCLI runs the command and returns stdout, the decoded result and the
// process exit code main would use.
func checkCLI(t *testing.T, args ...string) (string, constraints.Result, int) {
	t.Helper()
	out, _, err := runCLI(t, args...)
	code := 0
	if err != nil {
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%v returned a non-exit error: %v", args, err)
		}
		code = exitErr.Code
	}
	var res constraints.Result
	if out != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out)
		}
	}
	return out, res, code
}

func TestCheckConstraintsCLIStatusesAndExitCodes(t *testing.T) {
	t.Run("not_configured", func(t *testing.T) {
		root := constraintsRepo(t, false)
		_, res, code := checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusNotConfigured || code != 2 {
			t.Fatalf("status %s exit %d", res.Status, code)
		}
	})
	t.Run("config_error", func(t *testing.T) {
		root := constraintsRepo(t, false)
		if err := os.WriteFile(filepath.Join(root, constraints.ConfigFileName), []byte(`{"schema_version":2}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, res, code := checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusConfigError || code != 2 || res.Errors[0].Code != constraints.CodeSchemaVersion {
			t.Fatalf("status %s exit %d errors %+v", res.Status, code, res.Errors)
		}
	})
	t.Run("not_indexed", func(t *testing.T) {
		root := constraintsRepo(t, true)
		out, res, code := checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusNotIndexed || code != 2 {
			t.Fatalf("status %s exit %d", res.Status, code)
		}
		if strings.Contains(out, root) {
			t.Fatalf("result leaks the host repo root:\n%s", out)
		}
	})
	t.Run("violations ok and stale", func(t *testing.T) {
		root := constraintsRepo(t, true)
		if _, _, err := runCLI(t, "index", root); err != nil {
			t.Fatal(err)
		}
		out, res, code := checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusViolations || code != 1 || len(res.Findings) != 1 {
			t.Fatalf("status %s exit %d\n%s", res.Status, code, out)
		}
		if strings.Contains(out, root) {
			t.Fatalf("result leaks the host repo root:\n%s", out)
		}
		// The alias is the same command.
		aliasOut, _, aliasCode := checkCLI(t, "check-constraints", root)
		if aliasOut != out || aliasCode != code {
			t.Fatalf("alias output differs:\n%s\n%s", out, aliasOut)
		}

		// A watcher-queued change marks the graph stale; findings stay.
		opened, err := openIndexedRepoReadOnly(context.Background(), loadTestConfig(t), root)
		if err != nil {
			t.Fatal(err)
		}
		dbPath, repoID := opened.DBPath, opened.Repo.ID
		opened.Close()
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.QueueDirtyFiles(context.Background(), repoID, []string{"internal/domain/a.go"}, "modified"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		_, res, code = checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusStale || code != 2 || len(res.Findings) != 1 {
			t.Fatalf("stale: status %s exit %d", res.Status, code)
		}

		// Removing the forbidden call and re-indexing gives ok.
		if err := os.WriteFile(filepath.Join(root, "internal/domain/a.go"), []byte("package domain\n\nfunc A() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err = store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DrainDirtyFiles(context.Background(), repoID); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if _, _, err := runCLI(t, "index", root); err != nil {
			t.Fatal(err)
		}
		_, res, code = checkCLI(t, "check_constraints", root)
		if res.Status != constraints.StatusOK || code != 0 {
			t.Fatalf("ok: status %s exit %d", res.Status, code)
		}
	})
	t.Run("argument errors exit 2", func(t *testing.T) {
		root := constraintsRepo(t, true)
		for _, args := range [][]string{{"--limit", "501"}, {"--limit", "-1"}, {"--offset", "-1"}} {
			_, _, code := checkCLI(t, append([]string{"check_constraints", root}, args...)...)
			if code != 2 {
				t.Errorf("%v exit %d, want 2", args, code)
			}
		}
	})
	t.Run("external config", func(t *testing.T) {
		root := constraintsRepo(t, false)
		external := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(external, []byte(cliConstraintsConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		out, res, _ := checkCLI(t, "check_constraints", root, "--config", external)
		if res.Config.Source != "external" || res.Config.Path != nil || strings.Contains(out, external) {
			t.Fatalf("external config reported as %+v\n%s", res.Config, out)
		}
	})
}

func loadTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestCheckConstraintsMCPParity drives the production serve path in both tool
// modes: the tool is listed in full mode, reachable through tool_search in
// gateway mode, and its `data` is byte-equal to the CLI output. The no-write
// assertion lives in the mcp package, where it can bracket the call alone
// (opening a serve session does its own bookkeeping writes).
func TestCheckConstraintsMCPParity(t *testing.T) {
	root := constraintsRepo(t, true)
	// A path with a character JSON encoders may HTML-escape: parity is
	// byte-equality, so both surfaces must spell it the same way.
	amp := filepath.Join(root, "internal", "domain", "r&d.go")
	if err := os.WriteFile(amp, []byte("package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc D() { infra.B() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatal(err)
	}
	cliOut, _, code := checkCLI(t, "check_constraints", root)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(cliOut, `r\u0026d.go`) {
		t.Fatalf("CLI output lacks the r&d.go finding:\n%s", cliOut)
	}
	cfg := loadTestConfig(t)
	serve := func(mode string, calls ...string) []map[string]json.RawMessage {
		t.Helper()
		lines := []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`}
		lines = append(lines, calls...)
		var stdout, stderr bytes.Buffer
		args := []string{"--repo-root", root}
		if mode != "" {
			args = append(args, "--tool-mode", mode)
		}
		if err := runServeWith(context.Background(), cfg, strings.NewReader(strings.Join(lines, "\n")+"\n"), &stdout, &stderr, args); err != nil {
			t.Fatalf("serve: %v (%s)", err, stderr.String())
		}
		var out []map[string]json.RawMessage
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("response is not JSON: %v (%s)", err, line)
			}
			out = append(out, m)
		}
		return out
	}
	listed := func(resp map[string]json.RawMessage) []string {
		var r struct {
			Tools []struct{ Name string } `json:"tools"`
		}
		if err := json.Unmarshal(resp["result"], &r); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range r.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	text := func(resp map[string]json.RawMessage) string {
		var r struct {
			IsError bool                    `json:"isError"`
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(resp["result"], &r); err != nil || r.IsError || len(r.Content) == 0 {
			t.Fatalf("tool call failed: %s", resp["result"])
		}
		return r.Content[0].Text
	}

	full := serve("", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"check_constraints","arguments":{}}}`)
	if names := listed(full[1]); !contains(names, "check_constraints") {
		t.Fatalf("full tools/list = %v, want check_constraints", names)
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text(full[2])), &envelope); err != nil || !envelope.OK {
		t.Fatalf("envelope = %s (%v)", text(full[2]), err)
	}
	var mcpCompact, cliCompact bytes.Buffer
	if err := json.Compact(&mcpCompact, envelope.Data); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&cliCompact, []byte(cliOut)); err != nil {
		t.Fatal(err)
	}
	if mcpCompact.String() != cliCompact.String() {
		t.Fatalf("MCP data differs from CLI output\nmcp: %s\ncli: %s", mcpCompact.String(), cliCompact.String())
	}

	gateway := serve("gateway",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"architectural constraints"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tool_call","arguments":{"name":"check_constraints","arguments":{}}}}`)
	if names := listed(gateway[1]); contains(names, "check_constraints") {
		t.Fatalf("gateway tools/list = %v; check_constraints must not be gateway core", names)
	}
	if !strings.Contains(text(gateway[2]), "check_constraints") {
		t.Fatalf("tool_search did not find check_constraints: %s", text(gateway[2]))
	}
	if err := json.Unmarshal([]byte(text(gateway[3])), &envelope); err != nil || !envelope.OK {
		t.Fatalf("gateway tool_call envelope = %s", text(gateway[3]))
	}
	mcpCompact.Reset()
	if err := json.Compact(&mcpCompact, envelope.Data); err != nil {
		t.Fatal(err)
	}
	if mcpCompact.String() != cliCompact.String() {
		t.Fatalf("gateway data differs from CLI output\n%s\n%s", mcpCompact.String(), cliCompact.String())
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
