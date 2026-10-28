package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/diff"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func TestDiffCLIAndMCPReturnSameDocument(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc A() {}\n")
	r.Write("util.go", "package main\nfunc U() {}\n")
	r.Write("lib.go", "package main\nfunc Gone() {}\nfunc C() { Gone() }\n")
	base := r.Commit("", "base")
	r.Write("main.go", "package main\nfunc A() { B() }\nfunc B() {}\n")
	r.Write("lib.go", "package main\nfunc C() { Gone() }\n")
	r.Git("mv", "util.go", "helpers_linux.go")
	head := r.Commit("", "head")

	var cliOut, cliErr bytes.Buffer
	if err := Run(context.Background(), []string{"diff", base, head, "--repo-root", r.Dir}, &cliOut, &cliErr); err != nil {
		t.Fatalf("CLI diff: %v (%s)", err, cliErr.String())
	}

	dbPath := t.TempDir() + "/graph.sqlite"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	idx := indexer.New(st, newDefaultRegistry(), nil)
	server := mcp.NewServer(r.Dir, 1, st, idx, query.New(st, nil), io.Discard)
	activeDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	activeHash := sha256.Sum256(activeDB)
	arguments, _ := json.Marshal(map[string]any{"base": base, "head": head})
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "diff", "arguments": json.RawMessage(arguments)}})
	var mcpOut bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(string(request)+"\n"), &mcpOut, io.Discard); err != nil {
		t.Fatalf("MCP diff: %v", err)
	}
	activeDB, err = os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(activeDB); got != activeHash {
		t.Fatal("diff tool modified the active repository database")
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpOut.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, mcpOut.String())
	}
	if len(response.Result.Content) != 1 {
		t.Fatalf("MCP content = %+v", response.Result.Content)
	}
	var viaCLI any
	if err := json.Unmarshal(cliOut.Bytes(), &viaCLI); err != nil {
		t.Fatal(err)
	}
	var viaMCP struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &viaMCP); err != nil {
		t.Fatalf("decode MCP diff document: %v", err)
	}
	if !viaMCP.OK {
		t.Fatalf("MCP diff payload not ok: %s", response.Result.Content[0].Text)
	}
	var renames struct {
		Diff struct {
			FileRenames struct {
				Exact []struct{ From, To string } `json:"exact"`
			} `json:"file_renames"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(cliOut.Bytes(), &renames); err != nil || len(renames.Diff.FileRenames.Exact) != 1 || renames.Diff.FileRenames.Exact[0].From != "util.go" || renames.Diff.FileRenames.Exact[0].To != "helpers_linux.go" {
		t.Fatalf("CLI renames = %+v (%v)", renames.Diff.FileRenames, err)
	}
	var risk struct {
		Diff struct{ Risk diff.Risk } `json:"diff"`
	}
	if err := json.Unmarshal(cliOut.Bytes(), &risk); err != nil || risk.Diff.Risk.Assessment != diff.AssessmentRiskEvidence || risk.Diff.Risk.Score == nil || *risk.Diff.Risk.Score != 20 {
		t.Fatalf("CLI risk = %+v (%v)", risk.Diff.Risk, err)
	}
	filenameUncertainty := false
	bodyUncertainty := false
	for _, u := range risk.Diff.Risk.Uncertainty {
		if u.Reason == "go_filename_build_role_change_unmodeled" && u.Identity == "util.go" {
			filenameUncertainty = true
		}
		if u.Reason == "go_function_body_change_unmodeled" && u.Identity == "main.go" && u.Detail == "func A" {
			bodyUncertainty = true
		}
	}
	if !filenameUncertainty {
		t.Fatalf("CLI diff omitted filename build-role uncertainty: %+v", risk.Diff.Risk.Uncertainty)
	}
	if !bodyUncertainty {
		t.Fatalf("CLI diff omitted function-body uncertainty: %+v", risk.Diff.Risk.Uncertainty)
	}
	cliJSON, _ := json.Marshal(viaCLI)
	var mcpDocument any
	if err := json.Unmarshal(viaMCP.Data, &mcpDocument); err != nil {
		t.Fatal(err)
	}
	mcpJSON, _ := json.Marshal(mcpDocument)
	if !bytes.Equal(cliJSON, mcpJSON) {
		t.Fatalf("CLI/MCP documents differ:\nCLI %s\nMCP %s", cliJSON, mcpJSON)
	}
}

func TestParseDiffGate(t *testing.T) {
	for _, tc := range []struct {
		gated            bool
		risk             string
		insufficientSet  bool
		insufficient     string
		failUncertainty  bool
		threshold        int
		failInsufficient bool
		err              bool
	}{
		{gated: false, threshold: 0},
		{insufficientSet: true, insufficient: "pass", err: true},
		{gated: true, err: true},
		{gated: true, risk: "low", insufficientSet: true, err: true},
		{gated: true, risk: "low", threshold: 1, failInsufficient: true},
		{gated: true, risk: "medium", insufficientSet: true, insufficient: "fail", threshold: 25, failInsufficient: true},
		{gated: true, risk: "high", insufficientSet: true, insufficient: "pass", threshold: 50},
		{gated: true, risk: "1", threshold: 1, failInsufficient: true},
		{gated: true, risk: "100", threshold: 100, failInsufficient: true},
		{gated: true, risk: "0", err: true},
		{gated: true, risk: "101", err: true},
		{gated: true, risk: "-5", err: true},
		{gated: true, risk: "HIGH", err: true},
		{gated: true, risk: "high", insufficientSet: true, insufficient: "maybe", err: true},
		{gated: true, failUncertainty: true},
		{gated: true, risk: "low", failUncertainty: true, threshold: 1, failInsufficient: true},
	} {
		g, err := parseDiffGate(tc.gated, tc.risk, tc.insufficientSet, tc.insufficient, tc.failUncertainty)
		if (err != nil) != tc.err || g.threshold != tc.threshold || g.failInsufficient != tc.failInsufficient || g.failUncertainty != tc.failUncertainty {
			t.Fatalf("parseDiffGate(%v, %q, %v, %q, %v) = %+v, %v", tc.gated, tc.risk, tc.insufficientSet, tc.insufficient, tc.failUncertainty, g, err)
		}
	}
}

func TestDiffGateEvaluateBoundaries(t *testing.T) {
	score := func(n int) diff.Risk { return diff.Risk{Score: &n} }
	g := diffGate{threshold: 25, failInsufficient: true}
	for _, tc := range []struct {
		r    diff.Risk
		gate diffGate
		want int
	}{
		{score(0), g, diffExitPass},
		{score(24), g, diffExitPass},
		{score(25), g, diffExitRisk},
		{score(100), g, diffExitRisk},
		{diff.Risk{}, g, diffExitInsufficient},
		{diff.Risk{}, diffGate{threshold: 25}, diffExitPass},
		{diff.Risk{Assessment: diff.AssessmentLimitedEvidence, Uncertainty: []diff.RiskUncertainty{{Reason: "unmodeled"}}}, diffGate{failUncertainty: true}, diffExitRisk},
		{diff.Risk{Assessment: diff.AssessmentLimitedEvidence, Uncertainty: []diff.RiskUncertainty{{Reason: "unmodeled"}}}, diffGate{threshold: 25, failInsufficient: true, failUncertainty: true}, diffExitRisk},
		{diff.Risk{Assessment: diff.AssessmentRiskEvidence, Uncertainty: []diff.RiskUncertainty{{Reason: "unmodeled"}}}, diffGate{failUncertainty: true}, diffExitPass},
		{diff.Risk{Assessment: diff.AssessmentNoRiskEvidence}, diffGate{failUncertainty: true}, diffExitPass},
		{diff.Risk{Assessment: diff.AssessmentInsufficientEvidence, Uncertainty: []diff.RiskUncertainty{{Reason: "incomplete"}}}, diffGate{failUncertainty: true}, diffExitPass},
	} {
		if got, _ := tc.gate.evaluate(tc.r); got != tc.want {
			t.Fatalf("evaluate(%v, %+v) = %d, want %d", tc.r.Score, tc.gate, got, tc.want)
		}
	}
}

func TestDiffCLIRiskGateExitCodes(t *testing.T) {
	r := gittest.Init(t)
	r.Write("lib.go", "package main\nfunc Gone() {}\n")
	r.Write("main.go", "package main\nfunc main() { Gone() }\n")
	base := r.Commit("", "base")
	r.Write("lib.go", "package main\n")
	removed := r.Commit("", "remove Gone")
	// A changed indexing policy makes the two graphs incomparable.
	r.Write(".codegraph/config.json", `{"exclude":["lib.go"]}`)
	r.Git("add", "-f", ".codegraph/config.json")
	incomparable := r.Commit("", "policy change")

	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		err := Run(context.Background(), append([]string{"diff", "--repo-root", r.Dir}, args...), &out, &errOut)
		code := 0
		if err != nil {
			code = 1
			var exit *ExitError
			if errors.As(err, &exit) {
				code = exit.Code
			}
		}
		return out.String(), errOut.String(), code
	}
	plain, _, code := run(base, removed)
	if code != 0 || !strings.Contains(plain, `"assessment": "risk_evidence"`) {
		t.Fatalf("default diff exit %d:\n%s", code, plain)
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{base, removed, "--fail-on-risk", "20"}, diffExitRisk},
		{[]string{base, removed, "--fail-on-risk=low"}, diffExitRisk},
		{[]string{base, removed, "--fail-on-risk", "21"}, diffExitPass},
		{[]string{base, base, "--fail-on-risk", "low"}, diffExitPass},
		{[]string{removed, incomparable, "--fail-on-risk", "low"}, diffExitInsufficient},
		{[]string{removed, incomparable, "--fail-on-risk", "low", "--insufficient-evidence", "pass"}, diffExitPass},
	} {
		out, stderr, code := run(tc.args...)
		if code != tc.code {
			t.Fatalf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, stderr)
		}
		// The gate never changes the document: it equals the ungated output.
		ungated, _, _ := run(tc.args[:2]...)
		if out != ungated || !strings.Contains(stderr, "diff risk: ") {
			t.Fatalf("%v: gated output differs from plain output or no summary (%s)", tc.args, stderr)
		}
	}
	for _, args := range [][]string{
		{base, removed, "--fail-on-risk", "0"},
		{base, removed, "--fail-on-risk", "low", "--fail-on-risk", "high"},
		{base, removed, "--fail-on-risk", "low", "--insufficient-evidence", "maybe"},
		{base, "--fail-on-risk", "low"},
		{base, removed, "--fail-on-risk"},
		{base, removed, "--fail-on-risk="},
		{base, removed, "--fail-on-risk", "low", "--insufficient-evidence="},
	} {
		if out, _, code := run(args...); code != diffExitInvalid || out != "" {
			t.Fatalf("%v: exit %d, output %q", args, code, out)
		}
	}
	if out, _, code := run(base, removed, "--insufficient-evidence", "pass"); code != 1 || out != "" {
		t.Fatalf("--insufficient-evidence without a gate: exit %d", code)
	}
	if out, stderr, code := run(base, base, "--fail-on-uncertainty"); code != diffExitPass || out == "" || !strings.Contains(stderr, "fail_on_uncertainty true") {
		t.Fatalf("uncertainty-only gate: exit %d, stderr %q", code, stderr)
	}
	if out, _, code := run(base, removed, "--fail-on-risk", "low", "--fail-on-uncertainty"); code != diffExitRisk || out == "" {
		t.Fatalf("combined gates: exit %d", code)
	}
	if out, _, code := run(base, removed, "--fail-on-uncertainty", "--fail-on-uncertainty"); code != diffExitInvalid || out != "" {
		t.Fatalf("repeated uncertainty gate: exit %d, output %q", code, out)
	}
	// Ungated parsing is unchanged: a repeated original flag still takes the last value.
	if out, _, code := run(base, removed, "--limit", "5", "--limit", "1"); code != 0 || !strings.Contains(out, `"limit": 1`) {
		t.Fatalf("repeated --limit: exit %d", code)
	}
}
