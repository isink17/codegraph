package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// legacyPathFormatTools is one call per MCP tool that reads indexed
// repository data through query.Service. Session, scan-log and process
// metadata tools are not repository-graph reads and are excluded on purpose.
// `audit` reads the store directly through graphaudit.Run, not through the
// Service, and is not gated by this tranche; it is deliberately absent here.
var legacyPathFormatTools = []struct {
	name string
	args map[string]any
}{
	{"find_symbol", map[string]any{"query": "Thing"}},
	{"search_symbols", map[string]any{"query": "Thing"}},
	{"search_semantic", map[string]any{"query": "thing"}},
	{"find_callers", map[string]any{"symbol": "Thing"}},
	{"find_callees", map[string]any{"symbol": "Caller"}},
	{"get_impact_radius", map[string]any{"symbols": []string{"Thing"}}},
	{"find_related_tests", map[string]any{"symbol": "Thing"}},
	{"find_related_tests", map[string]any{"file": "src/pkg/file.go"}},
	{"find_related_tests", map[string]any{"files": []string{"src/pkg/file.go"}}},
	{"trace_dependencies", map[string]any{"symbol": "Thing"}},
	{"context_for_task", map[string]any{"task": "thing"}},
	{"find_dead_code", map[string]any{}},
	{"list_files", map[string]any{}},
	{"architecture_overview", map[string]any{}},
	{"detect_frameworks", map[string]any{}},
	{"benchmark_tokens", map[string]any{"task": "thing"}},
	{"cross_language_links", map[string]any{}},
	{"graph_analytics", map[string]any{"analysis": "pagerank"}},
	{"graph_analytics", map[string]any{"analysis": "coupling"}},
	{"graph_analytics", map[string]any{"analysis": "cycles"}},
	{"graph_stats", map[string]any{}},
}

func snapshotServerDB(t *testing.T, raw *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, hex(path), is_deleted FROM files ORDER BY id`,
		`SELECT hex(path), reason, queued_at FROM dirty_files ORDER BY path`,
		`SELECT key, value FROM settings ORDER BY key`,
		`SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM dirty_files), (SELECT COUNT(*) FROM scans), (SELECT COUNT(*) FROM symbols)`,
	} {
		rows, err := raw.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					b.Write(bs)
				} else {
					fmt.Fprint(&b, v)
				}
				b.WriteByte('|')
			}
			b.WriteByte('\n')
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// TestMCPRepositoryToolsFailClosedOnLegacyPathFormat proves the MCP surface
// inherits the Service gate: every repository-data tool answers a current
// index, and on the same index degraded to a pre-P23 shape every one returns
// an isError tool result carrying the rebuild-required message, never a
// legacy-spelled file path, and never touches the database.
func TestMCPRepositoryToolsFailClosedOnLegacyPathFormat(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "src/pkg/file.go", "package pkg\n\nfunc Thing() {}\n\nfunc Caller() { Thing() }\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)

	// Positive control on the current format.
	for _, tool := range legacyPathFormatTools {
		if isErr, text := callResult(t, server, tool.name, tool.args); isErr {
			t.Fatalf("%s(%v) on a current-format index errored: %s", tool.name, tool.args, text)
		}
	}
	if _, text := callResult(t, server, "find_symbol", map[string]any{"query": "Thing"}); !strings.Contains(text, `src/pkg/file.go`) {
		t.Fatalf("current-format find_symbol did not report the logical path: %s", text)
	}

	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, stmt := range []string{
		`DELETE FROM settings WHERE value = 'logical-slash-v1'`,
		`UPDATE files SET path = replace(path, '/', '\')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'legacy', '2026-01-01T00:00:00Z')`, repo.ID, `src\pkg\file.go`); err != nil {
		t.Fatal(err)
	}
	before := snapshotServerDB(t, raw)
	if !strings.Contains(before, "7372635C") { // hex(`src\`)
		t.Fatalf("fixture did not produce a native-spelled files.path:\n%s", before)
	}

	wantMessage := store.ErrRepositoryPathFormatRebuild.Error()
	check := func(label string, isErr bool, text string) {
		t.Helper()
		if !isErr {
			t.Fatalf("%s on legacy index returned data: %s", label, text)
		}
		if !strings.Contains(text, wantMessage) {
			t.Fatalf("%s error does not carry the rebuild-required message: %s", label, text)
		}
		if strings.Contains(text, `src\\pkg`) || strings.Contains(text, `src/pkg/file.go`) {
			t.Fatalf("%s leaked a repository path from a legacy index: %s", label, text)
		}
		if after := snapshotServerDB(t, raw); after != before {
			t.Fatalf("%s mutated the legacy database:\nbefore:\n%s\nafter:\n%s", label, before, after)
		}
	}
	for _, tool := range legacyPathFormatTools {
		isErr, text := callResult(t, server, tool.name, tool.args)
		check(fmt.Sprintf("%s(%v)", tool.name, tool.args), isErr, text)
	}

	// The gateway surface routes through the same Service and inherits the gate.
	gateway := NewServer(repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)
	if err := gateway.SetToolMode(ToolModeGateway); err != nil {
		t.Fatal(err)
	}
	isErr, text := callResult(t, gateway, "tool_call", map[string]any{"name": "search_symbols", "arguments": map[string]any{"query": "Thing"}})
	check("tool_call(search_symbols)", isErr, text)
}
