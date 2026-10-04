package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/store"
)

// The CLI command and the MCP tool return the same document for the same
// request, and --no-history is recorded as a distinct absent reason.
func TestFileHistoryCLIAndMCPAgree(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	quietStartup(t)
	r := gittest.Init(t)
	r.Write("main.go", "package main\n\nfunc main() {}\n")
	r.Write("lib/a.go", "package lib\n\nfunc A() {}\n")
	r.Commit("", "one")
	r.Write("lib/a.go", "package lib\n\nfunc A() { B() }\n\nfunc B() {}\n")
	r.Commit("bob@example.com", "two")
	r.Write("main.go", "package main\n\nfunc main() { _ = 1 }\n")
	ctx := context.Background()

	run := func(args ...string) store.GitHistoryResult {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := Run(ctx, args, &out, &errOut); err != nil {
			t.Fatalf("Run(%v): %v\n%s", args, err, errOut.String())
		}
		var got store.GitHistoryResult
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("Run(%v): %v\n%s", args, err, out.String())
		}
		return got
	}
	var discard bytes.Buffer
	if err := Run(ctx, []string{"index", r.Dir}, &discard, &discard); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, repo, repoID, err := openApp(ctx, cfg, r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	callTool := func(args map[string]any) store.GitHistoryResult {
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "file_history", "arguments": args}})
		var out bytes.Buffer
		server := mcp.NewServer(repo.RootPath, repoID, app.Store, app.Indexer, app.Query, io.Discard)
		if err := server.Serve(ctx, bytes.NewReader(append(frame, '\n')), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil || len(resp.Result.Content) != 1 {
			t.Fatalf("MCP response %s: %v", out.String(), err)
		}
		var envelope struct {
			Data store.GitHistoryResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	mcpList := callTool(map[string]any{"limit": 1, "offset": 1})
	mcpFiles := callTool(map[string]any{"files": []string{"lib/a.go", "main.go"}})
	mcpSymbols := callTool(map[string]any{"files": []string{"lib/a.go", "main.go"}, "include_symbols": true})
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	cliList := run("file_history", r.Dir, "--limit", "1", "--offset", "1")
	cliFiles := run("file_history", "--repo-root", r.Dir, "--file", "lib/a.go", "--file", "main.go")
	if !reflect.DeepEqual(mcpList, cliList) || !reflect.DeepEqual(mcpFiles, cliFiles) {
		t.Fatalf("CLI and MCP differ:\nmcp %+v\ncli %+v\nmcp %+v\ncli %+v", mcpList, cliList, mcpFiles, cliFiles)
	}
	// The repository database under .codegraph/ is untracked but never listed.
	if cliList.History.Status != githistory.StatusOK || cliList.Total != 2 || len(cliList.Files) != 1 || cliList.Files[0].Path != "main.go" {
		t.Fatalf("listing = %+v", cliList)
	}
	cliSymbols := run("file_history", r.Dir, "--file", "lib/a.go", "--file", "main.go", "--symbols")
	if !reflect.DeepEqual(mcpSymbols, cliSymbols) {
		t.Fatalf("CLI and MCP symbols differ:\nmcp %+v\ncli %+v", mcpSymbols, cliSymbols)
	}
	if sa, sm := cliSymbols.Files[0], cliSymbols.Files[1]; sa.SymbolHistory != store.SymbolHistoryOK || len(sa.Symbols) != 2 ||
		sa.Symbols[0].Name != "A" || sa.Symbols[0].LastAuthor != "bob@example.com" || sa.Symbols[0].LastCommit == nil ||
		sm.SymbolHistory != store.SymbolHistoryWorktreeDiffers || sm.Symbols != nil {
		t.Fatalf("symbols = %+v", cliSymbols.Files)
	}
	if cliFiles.Files[0].SymbolHistory != "" || cliFiles.Files[0].Symbols != nil {
		t.Fatalf("symbols without --symbols: %+v", cliFiles.Files[0])
	}
	a, m := cliFiles.Files[0], cliFiles.Files[1]
	if a.CommitCount != 2 || a.AuthorCount != 2 || a.WorktreeDiffers || !m.WorktreeDiffers || m.CommitCount != 1 {
		t.Fatalf("files = %+v", cliFiles.Files)
	}

	if err := Run(ctx, []string{"update", r.Dir, "--no-history"}, &discard, &discard); err != nil {
		t.Fatal(err)
	}
	if got := run("file_history", r.Dir); got.History.AbsentReason != githistory.ReasonDisabled || len(got.Files) != 0 {
		t.Fatalf("after --no-history: %+v", got)
	}
}
