package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// TestCheckConstraintsToolDoesNotWrite brackets one tools/call with PRAGMA
// data_version and total_changes() read on an independent connection. The
// server evaluates on its read-write handle, so read-only rests on the
// evaluator issuing SELECT only; data_version moves when any other connection
// commits.
func TestCheckConstraintsToolDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "go.mod", "module example.com/m\n\ngo 1.22\n")
	writeRepoFile(t, repoRoot, "internal/domain/a.go", "package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc A() { infra.B() }\n")
	writeRepoFile(t, repoRoot, "internal/infra/b.go", "package infra\n\nfunc B() {}\n")
	writeRepoFile(t, repoRoot, constraints.ConfigFileName, `{"schema_version":1,
		"groups":{"domain":{"include":["internal/domain/**"]},"infra":{"include":["internal/infra/**"]}},
		"rules":[{"id":"r","kind":"forbidden_dependency","from":["domain"],"to":["infra"]}]}`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)

	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	conn, err := observer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	probe := func() (dv, changes int64) {
		if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&dv); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changes); err != nil {
			t.Fatal(err)
		}
		return
	}

	call := func() map[string]any {
		input := bytes.NewBuffer(nil)
		writeFrameToBuffer(t, input, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "check_constraints", "arguments": map[string]any{}}})
		var output bytes.Buffer
		if err := server.Serve(ctx, input, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		responses := readAllFrames(t, &output)
		result := responses[0]["result"].(map[string]any)
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	beforeDV, beforeChanges := probe()
	payload := call()
	afterDV, afterChanges := probe()
	data, _ := payload["data"].(map[string]any)
	if payload["ok"] != true || data["status"] != constraints.StatusViolations {
		t.Fatalf("payload = %v", payload)
	}
	if beforeDV != afterDV || beforeChanges != afterChanges {
		t.Fatalf("data_version %d -> %d, total_changes %d -> %d: check_constraints wrote", beforeDV, afterDV, beforeChanges, afterChanges)
	}
	// The probe is live: a write through the server's own handle moves it.
	if err := st.QueueDirtyFiles(ctx, repo.ID, []string{"internal/domain/a.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	if dv, _ := probe(); dv == afterDV {
		t.Fatal("data_version did not move on a real write; the probe proves nothing")
	}
	if data := call()["data"].(map[string]any); data["status"] != constraints.StatusStale {
		t.Fatalf("status after a queued change = %v, want stale", data["status"])
	}
	if _, ok := payload["data"].(map[string]any)["freshness"]; ok {
		t.Fatal("default call returned a freshness block")
	}
	strictDV, strictChanges := probe()
	res, err := server.handleCheckConstraints(ctx, json.RawMessage(`{"strict_freshness":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if dv, changes := probe(); dv != strictDV || changes != strictChanges {
		t.Fatal("strict check_constraints wrote")
	}
	strict := res["data"].(constraints.Result)
	if strict.Status != constraints.StatusStale || strict.Freshness == nil ||
		strict.Freshness.Verdict != constraints.VerdictKnownStale || strict.Freshness.ExitCode != constraints.ExitKnownStale {
		t.Fatalf("strict data = %+v", strict.Freshness)
	}
}

// TestCheckConstraintsToolArguments: limit and offset are validated, never
// clamped, and a config path cannot be passed.
func TestCheckConstraintsToolArguments(t *testing.T) {
	for _, args := range []string{`{"limit":501}`, `{"limit":-1}`, `{"offset":-1}`, `{"config":"/etc/passwd"}`} {
		if err := validateToolArguments("check_constraints", json.RawMessage(args)); err == nil {
			t.Errorf("validateToolArguments(%s) accepted", args)
		}
	}
	if err := validateToolArguments("check_constraints", json.RawMessage(`{"limit":5,"offset":2}`)); err != nil {
		t.Errorf("valid arguments rejected: %v", err)
	}
	if err := validateToolArguments("check_constraints", json.RawMessage(`{"strict_freshness":true}`)); err != nil {
		t.Errorf("strict_freshness rejected: %v", err)
	}
	if err := validateToolArguments("check_constraints", json.RawMessage(`{"strict_freshness":"yes"}`)); err == nil {
		t.Error("a non-boolean strict_freshness was accepted")
	}
}
