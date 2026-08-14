package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/usage"
)

// TestServeUsageSummaryGoesToStderrOnly is the load-bearing claim: stdout is the
// MCP transport, so a diagnostic that leaked one byte into it would corrupt
// every session it was measuring.
func TestServeUsageSummaryGoesToStderrOnly(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, _ := indexedRepo(t, nil)

	session := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`,
		"",
	}, "\n")

	var stdout, stderr bytes.Buffer
	cfg := config.Config{DBDir: config.RepoDBDir}
	if err := runServeWith(context.Background(), cfg, strings.NewReader(session),
		&stdout, &stderr, []string{"--repo-root", repoRoot, "--usage-summary"}); err != nil {
		t.Fatalf("runServeWith() error = %v (stderr: %s)", err, stderr.String())
	}

	// stdout carries exactly the three protocol responses and nothing else.
	if got := strings.Count(strings.TrimRight(stdout.String(), "\n"), "\n") + 1; got != 3 {
		t.Fatalf("stdout has %d lines, want 3 responses:\n%s", got, stdout.String())
	}
	if strings.Contains(stdout.String(), usage.Schema) {
		t.Fatalf("the usage summary leaked into the MCP transport:\n%s", stdout.String())
	}
	summary := stderr.String()
	if strings.Count(summary, usage.Schema) != 1 {
		t.Fatalf("stderr summary = %q, want exactly one usage report", summary)
	}
	var report usage.Report
	if err := json.Unmarshal([]byte(summary[strings.Index(summary, "{"):]), &report); err != nil {
		t.Fatalf("stderr summary is not a usage report: %v (%s)", err, summary)
	}
	// It reports the session it just served: one tools/list and one tool call.
	if report.Session.ToolsListCalls != 1 || report.Session.ToolCalls != 1 {
		t.Fatalf("summary session = %+v", report.Session)
	}
}

// TestServeWithoutUsageSummaryStaysSilent: the flag is opt-in, and stderr is not
// a place to write things nobody asked for.
func TestServeWithoutUsageSummaryStaysSilent(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, _ := indexedRepo(t, nil)

	var stdout, stderr bytes.Buffer
	cfg := config.Config{DBDir: config.RepoDBDir}
	if err := runServeWith(context.Background(), cfg,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"),
		&stdout, &stderr, []string{"--repo-root", repoRoot}); err != nil {
		t.Fatalf("runServeWith() error = %v", err)
	}
	if strings.Contains(stderr.String(), usage.Schema) {
		t.Fatalf("serve wrote a usage summary without being asked:\n%s", stderr.String())
	}
}

// TestWriteUsageSummaryIsStderrShaped covers the report's own shape: one line,
// machine-readable, and labelled as an estimate rather than billing.
func TestWriteUsageSummaryIsStderrShaped(t *testing.T) {
	meter := usage.New("gateway", nil)
	meter.RecordToolsList(2, 3585)
	meter.RecordToolCall(usage.Key{Tool: "find_symbol", Via: usage.ViaGateway, Detail: "card"}, 40, 900, false)

	var buf bytes.Buffer
	writeUsageSummary(&buf, meter.Snapshot(false, 0))
	line := buf.String()

	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("summary is not a single line: %q", line)
	}
	if !strings.Contains(line, "not provider billing") {
		t.Fatalf("summary does not label the estimate: %q", line)
	}
	payload := line[strings.Index(line, "{"):]
	var report usage.Report
	if err := json.Unmarshal([]byte(payload), &report); err != nil {
		t.Fatalf("summary payload is not a usage report: %v (%s)", err, payload)
	}
	if report.Schema != usage.Schema || report.Session.ToolMode != "gateway" {
		t.Fatalf("summary report = %+v", report)
	}
	if report.Session.MCPCalls != 2 || report.Discovery.ResponseBytes != 3585 {
		t.Fatalf("summary lost counters: %+v / %+v", report.Session, report.Discovery)
	}
}
