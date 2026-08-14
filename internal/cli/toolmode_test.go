package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/mcp"
)

// TestServeHelpDocumentsToolMode pins the documented surface of the flag that
// selects the tool mode.
func TestServeHelpDocumentsToolMode(t *testing.T) {
	quietStartup(t)
	out, _, err := runCLI(t, "serve", "--help")
	if err != nil {
		t.Fatalf("Run(serve --help) error = %v", err)
	}
	for _, want := range []string{"--tool-mode", "full", "gateway", "--repo-root"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help is missing %q:\n%s", want, out)
		}
	}
}

// TestServeRejectsAnInvalidToolMode is the no-silent-fallback contract. The mode is
// resolved before the database is opened, so an unusable value stops startup
// rather than quietly serving the default surface.
func TestServeRejectsAnInvalidToolMode(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	for _, mode := range []string{"minimal", "lite", "auto", "FULL"} {
		_, _, err := runCLI(t, "serve", "--tool-mode", mode)
		if err == nil {
			t.Errorf("serve --tool-mode %q succeeded, want an error", mode)
			continue
		}
		if !strings.Contains(err.Error(), "invalid tool mode") {
			t.Errorf("serve --tool-mode %q error = %v, want it to name the invalid mode", mode, err)
		}
	}
}

// TestInstallRejectsAnInvalidToolMode checks the mode is validated before install
// touches any directory or client config.
func TestInstallRejectsAnInvalidToolMode(t *testing.T) {
	quietStartup(t)
	if err := runInstall(io.Discard, []string{"--tool-mode", "smart"}); err == nil {
		t.Fatal("install --tool-mode smart succeeded, want an error")
	} else if !strings.Contains(err.Error(), "invalid tool mode") {
		t.Fatalf("install --tool-mode smart error = %v, want it to name the invalid mode", err)
	}
}

// TestServeArgsDefaultIsUnchanged is the install backward-compatibility guarantee:
// the default and the explicit default both write the argv every existing client
// config already holds.
func TestServeArgsDefaultIsUnchanged(t *testing.T) {
	for _, mode := range []mcp.ToolMode{"", mcp.ToolModeFull} {
		if got := serveArgs(mode); !reflect.DeepEqual(got, []string{"serve"}) {
			t.Errorf("serveArgs(%q) = %v, want [serve]", mode, got)
		}
	}
	want := []string{"serve", "--tool-mode", "gateway"}
	if got := serveArgs(mcp.ToolModeGateway); !reflect.DeepEqual(got, want) {
		t.Errorf("serveArgs(gateway) = %v, want %v", got, want)
	}
}

// TestMergeMCPEntryWritesTheRequestedToolMode drives the real config-merge path
// against a temp file rather than a machine-global client config.
func TestMergeMCPEntryWritesTheRequestedToolMode(t *testing.T) {
	for _, tc := range []struct {
		mode mcp.ToolMode
		want []any
	}{
		{mode: "", want: []any{"serve"}},
		{mode: mcp.ToolModeFull, want: []any{"serve"}},
		{mode: mcp.ToolModeGateway, want: []any{"serve", "--tool-mode", "gateway"}},
	} {
		configPath := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := mergeMCPEntry(configPath, "/usr/local/bin/codegraph", tc.mode); err != nil {
			t.Fatalf("mergeMCPEntry(%q) error = %v", tc.mode, err)
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		var root struct {
			MCPServers map[string]struct {
				Command string `json:"command"`
				Args    []any  `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatalf("json.Unmarshal() error = %v: %s", err, data)
		}
		entry, ok := root.MCPServers["codegraph"]
		if !ok {
			t.Fatalf("no codegraph entry was written: %s", data)
		}
		if entry.Command != "/usr/local/bin/codegraph" {
			t.Errorf("command = %q, want the binary path", entry.Command)
		}
		if !reflect.DeepEqual(entry.Args, tc.want) {
			t.Errorf("mode %q args = %v, want %v", tc.mode, entry.Args, tc.want)
		}
	}
}

// TestGatewayInstallHintNamesTheArgsToChange checks that an existing entry still
// tells the user how to get the mode they asked for, instead of only "skipped".
func TestGatewayInstallHintNamesTheArgsToChange(t *testing.T) {
	if got := mustJSONArgs(serveArgs(mcp.ToolModeGateway)); got != `["serve","--tool-mode","gateway"]` {
		t.Errorf("mustJSONArgs(serveArgs(gateway)) = %s, want the gateway argv", got)
	}
	if got := mustJSONArgs(serveArgs(mcp.ToolModeFull)); got != `["serve"]` {
		t.Errorf("mustJSONArgs(serveArgs(full)) = %s, want [\"serve\"]", got)
	}
}

// TestMergeMCPEntryStillPreservesExistingEntries checks the mode change did not
// alter the merge behaviour around a config the user already customised.
func TestMergeMCPEntryStillPreservesExistingEntries(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "mcp.json")
	original := `{"mcpServers":{"other":{"command":"x","args":["y"]}}}` + "\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := mergeMCPEntry(configPath, "codegraph", mcp.ToolModeGateway); err != nil {
		t.Fatalf("mergeMCPEntry() error = %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), `"other"`) {
		t.Errorf("the existing entry was dropped: %s", data)
	}
	// A second merge must not overwrite what is already there, and it must say so
	// with the sentinel the installer uses to print the switch instruction.
	err = mergeMCPEntry(configPath, "codegraph", mcp.ToolModeFull)
	if err == nil {
		t.Error("mergeMCPEntry() overwrote an existing codegraph entry")
	} else if !errors.Is(err, errMCPEntryExists) {
		t.Errorf("mergeMCPEntry() on an existing entry = %v, want errMCPEntryExists", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(after), "--tool-mode") {
		t.Errorf("the gateway entry was replaced: %s", after)
	}
}
