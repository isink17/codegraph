package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/isink17/codegraph/internal/mcp"
)

// errMCPEntryExists reports that a config already has a codegraph entry.
//
// It is a sentinel rather than a formatted string so the caller can tell "you
// already have one, and it may name a different mode" apart from a genuine failure
// to write the file.
var errMCPEntryExists = errors.New("codegraph entry already exists")

// mcpTool describes an AI tool whose config we can auto-configure.
type mcpTool struct {
	Name       string
	ConfigPath string // absolute path to the tool's config file
}

// discoverTools returns the list of AI tools whose config files exist on disk.
func discoverTools(home string) []mcpTool {
	candidates := []struct {
		name  string
		paths []string // relative to home
	}{
		{"Claude Code", []string{".claude.json", filepath.Join(".claude", "claude.json")}},
		{"Cursor", []string{filepath.Join(".cursor", "mcp.json")}},
		{"Windsurf", []string{filepath.Join(".windsurf", "mcp.json"), filepath.Join(".codeium", "windsurf", "mcp.json")}},
		{"Gemini CLI", []string{filepath.Join(".gemini", "settings.json")}},
	}

	var found []mcpTool
	for _, c := range candidates {
		for _, rel := range c.paths {
			abs := filepath.Join(home, rel)
			if _, err := os.Stat(abs); err == nil {
				found = append(found, mcpTool{Name: c.name, ConfigPath: abs})
				break // first match per tool wins
			}
		}
	}
	return found
}

// serveArgs is the argv an MCP client launches the server with.
//
// It is the one place the launch command is built, so the auto-configured JSON,
// the printed snippets, and the mode flag cannot disagree. The default mode adds
// no flag at all: an install without --tool-mode writes exactly the `["serve"]`
// it wrote before gateway mode existed.
func serveArgs(mode mcp.ToolMode) []string {
	if mode == mcp.ToolModeGateway {
		return []string{"serve", "--tool-mode", string(mcp.ToolModeGateway)}
	}
	return []string{"serve"}
}

// mcpServerEntry returns the codegraph MCP server JSON object.
// For Gemini CLI the format is identical to other tools.
func mcpServerEntry(binaryPath string, mode mcp.ToolMode) map[string]any {
	return map[string]any{
		"command": binaryPath,
		"args":    serveArgs(mode),
	}
}

// autoConfigureMCP detects installed AI tools and injects the codegraph MCP
// server entry into each tool's config file. It returns the number of tools
// that were successfully configured.
func autoConfigureMCP(stdout io.Writer, mode mcp.ToolMode) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stdout, "  could not determine home directory: %v\n", err)
		return 0
	}

	binaryPath, err := os.Executable()
	if err != nil {
		binaryPath = "codegraph"
	} else {
		// Normalise to forward slashes on Windows for JSON portability.
		if runtime.GOOS == "windows" {
			binaryPath = filepath.ToSlash(binaryPath)
		}
	}

	tools := discoverTools(home)
	if len(tools) == 0 {
		return 0
	}

	configured := 0
	for _, tool := range tools {
		if err := mergeMCPEntry(tool.ConfigPath, binaryPath, mode); err != nil {
			fmt.Fprintf(stdout, "  %s (%s): skipped – %v\n", tool.Name, tool.ConfigPath, err)
			// A user who asked for a specific mode and already has an entry would
			// otherwise read "skipped", restart the client, and get the old surface with
			// nothing saying why. Existing entries are never overwritten, so say what to
			// change instead of implying the flag took effect.
			if errors.Is(err, errMCPEntryExists) {
				fmt.Fprintf(stdout, "    to use --tool-mode %s here, set its \"args\" to %s\n",
					mode, mustJSONArgs(serveArgs(mode)))
			}
			continue
		}
		fmt.Fprintf(stdout, "  %s: configured (%s)\n", tool.Name, tool.ConfigPath)
		configured++
	}
	return configured
}

// mustJSONArgs renders an argv for a printed instruction. A marshal of a []string
// cannot fail, and a printed hint is not worth an error path.
func mustJSONArgs(args []string) string {
	payload, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(payload)
}

// mergeMCPEntry reads a JSON config file, merges the codegraph entry under
// mcpServers, and writes the result back. Existing entries are preserved.
func mergeMCPEntry(configPath, binaryPath string, mode mcp.ToolMode) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	var root map[string]any

	// Handle empty files or files with only whitespace.
	trimmed := trimBytes(data)
	if len(trimmed) == 0 {
		root = make(map[string]any)
	} else {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}

	// Ensure mcpServers map exists.
	servers, ok := root["mcpServers"]
	if !ok {
		root["mcpServers"] = map[string]any{}
		servers = root["mcpServers"]
	}
	serversMap, ok := servers.(map[string]any)
	if !ok {
		return fmt.Errorf("mcpServers is not an object")
	}

	// Only add if not already present (don't overwrite user customisations).
	if _, exists := serversMap["codegraph"]; exists {
		return errMCPEntryExists
	}

	serversMap["codegraph"] = mcpServerEntry(binaryPath, mode)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	return os.WriteFile(configPath, out, 0o644)
}

// trimBytes returns data with leading/trailing whitespace removed.
func trimBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}
