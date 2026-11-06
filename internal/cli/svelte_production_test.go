package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/doctor"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/parser"
)

func svelteProductionFixture(t *testing.T) string {
	t.Helper()
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	src, err := os.ReadFile("../parser/treesitter/svelte/fixtures/Tick.svelte")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"Tick.svelte": src,
		"main.go":     []byte("package main\nfunc main() {}\n"),
	} {
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, stderr, err := runCLI(t, "index", root); err != nil {
		t.Fatalf("production index: %v\n%s\n%s", err, out, stderr)
	}
	return root
}

func TestSvelteProductionCapabilities(t *testing.T) {
	available, ok := noCGOCapabilities()["svelte"]
	if !ok || available.Parser || available.CallGraph {
		t.Fatalf("Svelte no-CGO availability = %+v, known=%v; want explicitly unavailable", available, ok)
	}
	registry := newDefaultRegistry()
	profile, supported := registry.ProfileForLanguage("svelte")
	adapter := registry.AdapterFor("Panel.SVELTE")
	if supported {
		if adapter == nil || adapter.Language() != "svelte" || profile.ID != "treesitter:svelte:v1" || profile.SemanticEpoch != 1 || profile.EmitsCallEdges {
			t.Fatalf("Svelte adapter/profile = %v / %+v", adapter, profile)
		}
	} else if adapter != nil {
		t.Fatalf("unsupported Svelte has adapter %v", adapter)
	}
	if collisions := registry.ExtensionCollisions(); len(collisions) != 0 {
		t.Fatalf("production extension collisions = %+v", collisions)
	}
	for _, ext := range []string{".js", ".ts", ".jsx", ".tsx"} {
		if a := registry.AdapterFor("existing" + ext); a == nil || a.Language() != "typescript" {
			t.Fatalf("existing %s ownership changed: %v", ext, a)
		}
	}
	root := svelteProductionFixture(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, repo, repoID, err := openExistingApp(context.Background(), cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool_call","arguments":{"name":"supported_languages","arguments":{}}}}` + "\n")
	var out bytes.Buffer
	server := mcp.NewServer(repo.RootPath, repoID, app.Store, app.Indexer, app.Query, io.Discard)
	if err := server.SetToolMode(mcp.ToolModeGateway); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background(), bytes.NewReader(frame), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("MCP response = %s, error=%v", out.String(), err)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Languages    []parser.LanguageSupport    `json:"languages"`
			Capabilities []parser.LanguageCapability `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || !reflect.DeepEqual(payload.Data.Languages, registry.SupportedLanguages()) || !reflect.DeepEqual(payload.Data.Capabilities, registry.Capabilities()) {
		t.Fatalf("MCP disagrees with production registry: %+v", payload)
	}
	found := false
	for _, c := range payload.Data.Capabilities {
		if c.ID != "svelte" {
			continue
		}
		found = true
		if !c.Parser || c.Symbols == nil || !*c.Symbols || c.Calls == nil || *c.Calls || c.NoCGOParser == nil || *c.NoCGOParser || c.NoCGOCallGraph == nil || *c.NoCGOCallGraph || c.CallResolution != nil {
			t.Fatalf("Svelte capability = %+v", c)
		}
	}
	if found != supported {
		t.Fatalf("Svelte advertised=%v, available=%v", found, supported)
	}
	text, stderr, err := runCLI(t, "doctor", "--repo-root", root)
	if err != nil {
		t.Fatalf("doctor: %v: %s", err, stderr)
	}
	var report struct {
		Parser *doctor.ParserInfo `json:"parser"`
	}
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatal(err)
	}
	if report.Parser == nil || slices.Contains(report.Parser.Degraded, "svelte") != supported {
		t.Fatalf("CLI doctor disagrees with runtime support: %s", text)
	}
}
