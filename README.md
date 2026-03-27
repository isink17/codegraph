# codegraph

**Your codebase, understood. Locally.**

`codegraph` is a local-first code context engine and MCP server that builds a persistent knowledge graph of your source repositories in SQLite. It gives AI coding assistants deep structural awareness — symbols, call graphs, dependencies, and semantic search — without sending a single byte to the cloud.

**Single binary. Zero config. No external databases. No API keys.**

---

## Why codegraph?

AI coding assistants are powerful, but they spend most of their token budget *discovering* what to change — grepping files, reading code, reconstructing call graphs from partial evidence.

**codegraph shifts that cost.** One tool call to `context_for_task` returns the exact files, symbols, and relationships an agent needs. One call to `agentic_query` gets a synthesized answer backed by graph traversal and semantic search.

### How It Works

```
Your Code ──> tree-sitter AST ──> SQLite Graph ──> MCP Tools ──> AI Assistant
                 │                    │                │
            12 languages        symbols, edges     29 tools
            framework detect    embeddings         agentic reasoning
            import resolution   session memory     hybrid search
```

**Without codegraph:** AI reads files one by one, grep for patterns, burns tokens on context-gathering.

**With codegraph:** AI calls `context_for_task("add retry logic to HTTP client")` and instantly gets the relevant files, functions, callers, callees, and tests.

---

## Features

### Parsing & Indexing
- **Tree-sitter parsing** for all 12 languages — robust AST extraction, not regex
- **4-strategy import resolution** — exact, name, suffix, and method receiver matching
- **Cross-language linking** — connects symbols across language boundaries
- **Incremental updates** — only re-indexes changed files
- **Framework detection** — recognizes 20+ frameworks (Express, Django, gin, React, Spring, Laravel, etc.)

### Search & Query
- **Hybrid search** — vector similarity (via Ollama embeddings) + FTS5, fused with Reciprocal Rank Fusion
- **Semantic search** — find code by meaning, not just text
- **Call graph traversal** — callers, callees, transitive dependency chains
- **Impact analysis** — know what breaks before you change it
- **Dead code detection** — find symbols with zero references
- **Architecture overview** — language breakdown, entry points, hub symbols, coupling metrics

### AI Integration
- **29 MCP tools** — comprehensive API for AI assistants
- **Agentic reasoning** — ReAct loop over local Ollama LLM chains tools and synthesizes answers
- **Context building** — one tool call returns everything an agent needs for a task
- **Session memory** — persist reads, edits, decisions, and facts across sessions
- **Token benchmarking** — measure savings vs. naive file reading

### Graph Analytics
- **PageRank** — find the most important symbols in your codebase
- **Coupling metrics** — identify tightly coupled file pairs
- **Cycle detection** — find circular dependencies at the file level
- **Interactive visualization** — D3.js force-directed graph with search and zoom

### Developer Experience
- **Single Go binary** — no runtime dependencies, cross-platform
- **Zero-config SQLite** — no Docker, no external databases
- **Auto-configure** — `codegraph install` detects and configures Claude Code, Cursor, Windsurf, Gemini CLI
- **File watching** — automatic re-indexing on changes
- **100% local** — no data leaves your machine

---

## Supported Languages

All languages use tree-sitter for AST parsing:

| Language | Extensions |
|----------|-----------|
| Go | `.go` |
| Python | `.py` |
| TypeScript | `.ts`, `.tsx` |
| JavaScript | `.js`, `.jsx`, `.mjs` |
| Java | `.java` |
| Kotlin | `.kt`, `.kts` |
| Rust | `.rs` |
| C# | `.cs` |
| Ruby | `.rb` |
| Swift | `.swift` |
| PHP | `.php` |
| C / C++ | `.c`, `.h`, `.cpp`, `.hpp`, `.cc` |

---

## Quick Start

### 1. Install

```bash
go install github.com/isink17/codegraph/cmd/codegraph@latest
```

### 2. Auto-configure your AI tool

```bash
codegraph install
```

This detects Claude Code, Cursor, Windsurf, and Gemini CLI and writes the MCP config automatically.

### 3. Index your project

```bash
cd your-project
codegraph index .
```

### 4. Start the MCP server

```bash
codegraph serve --repo-root .
```

That's it. Your AI assistant now has deep code understanding.

---

## MCP Tools (29)

### Code Intelligence

| Tool | Description |
|------|-------------|
| `find_symbol` | Find symbols by exact or fuzzy query |
| `search_symbols` | Search symbol names, signatures, and docs (FTS5) |
| `search_semantic` | Hybrid semantic search (vector + FTS when embeddings available) |
| `find_callers` | Find what calls a function |
| `find_callees` | Find what a function calls |
| `get_impact_radius` | Estimate affected symbols and files around a change |
| `trace_dependencies` | Trace transitive dependency chains (upstream/downstream) |
| `find_related_tests` | Find tests for a symbol, file, or set of changed files |
| `find_dead_code` | Find symbols with no callers or references |
| `context_for_task` | Build focused context bundle for a natural-language task |

### Architecture & Analysis

| Tool | Description |
|------|-------------|
| `architecture_overview` | Language breakdown, directories, entry points, hub symbols |
| `graph_analytics` | PageRank, coupling metrics, or cycle detection |
| `detect_frameworks` | Detect frameworks and libraries used in the repo |
| `cross_language_links` | Find and create cross-language symbol references |
| `benchmark_tokens` | Estimate token savings vs. reading raw files |

### Repository Management

| Tool | Description |
|------|-------------|
| `index_repo` | Index a repository into the local code graph |
| `update_graph` | Update only changed files |
| `list_files` | List indexed files with optional path filter |
| `graph_stats` | Repository graph statistics |
| `supported_languages` | List supported languages and extensions |
| `list_repos` | List known repositories |
| `list_scans` | List recent scans |
| `latest_scan_errors` | List scan errors |

### Session Memory

| Tool | Description |
|------|-------------|
| `session_log` | Log a session event (read, edit, decision, task, fact) |
| `session_history` | Get session event history |
| `session_hot_files` | Get most frequently accessed files |
| `session_context` | Get aggregated session context for pre-loading |

### Agentic

| Tool | Description |
|------|-------------|
| `agentic_query` | Ask a question answered by an AI agent that reasons over the code graph (requires local Ollama) |

---

## CLI Commands

```bash
codegraph install                  # Auto-configure AI tools
codegraph index <path>             # Full index
codegraph update <path>            # Incremental update
codegraph stats <path>             # Graph statistics
codegraph serve --repo-root <path> # Start MCP server
codegraph watch <path>             # Watch and auto-reindex

codegraph find-symbol <path> <query>        # Find symbols
codegraph search <path> <query>             # Search symbols
codegraph callers <path> --symbol <name>    # Find callers
codegraph callees <path> --symbol <name>    # Find callees
codegraph impact <path> --symbol <name>     # Impact analysis

codegraph affected-tests [--stdin] <files>  # Find tests affected by changes
codegraph visualize --repo-root <path>      # Interactive graph visualization

codegraph graph export <path> --format dot  # Export as Graphviz DOT
codegraph graph export <path> --format json # Export as JSON

codegraph doctor                   # Check installation
codegraph config show              # Show config
codegraph clean <path>             # Clean database
codegraph benchmark                # Run benchmark
```

### Affected Tests with Git

```bash
# Find tests affected by uncommitted changes
git diff --name-only | codegraph affected-tests --stdin

# CI integration
TESTS=$(git diff --name-only HEAD~1 | codegraph affected-tests --stdin --repo-root .)
go test $TESTS
```

---

## MCP Setup

### Auto-configure (recommended)

```bash
codegraph install
```

Automatically detects and configures Claude Code, Cursor, Windsurf, and Gemini CLI.

### Manual Setup

**Claude Code** — add to `.mcp.json`:
```json
{
  "mcpServers": {
    "codegraph": {
      "command": "codegraph",
      "args": ["serve", "--repo-root", "."]
    }
  }
}
```

**Codex** — add to `config.toml`:
```toml
[mcp_servers.codegraph]
command = "codegraph"
args = ["serve", "--repo-root", "/path/to/repo"]
startup_timeout_sec = 60
```

**Cursor / Windsurf** — add to `mcp.json`:
```json
{
  "mcpServers": {
    "codegraph": {
      "command": "codegraph",
      "args": ["serve", "--repo-root", "."]
    }
  }
}
```

See `examples/` for more configuration samples.

---

## Optional: Embeddings & Agentic Mode

codegraph works fully without any external services. For enhanced capabilities, you can optionally enable:

### Vector Embeddings (via Ollama)

Enables hybrid semantic search (vector + FTS):

```bash
# Install and start Ollama
ollama pull nomic-embed-text

# Enable in repo config
codegraph config init --repo .
```

Edit `.codegraph/config.json`:
```json
{
  "embedding": {
    "enabled": true,
    "model": "nomic-embed-text"
  }
}
```

Re-index to generate embeddings:
```bash
codegraph index . --force
```

### Agentic Reasoning (via Ollama)

The `agentic_query` tool uses a local LLM to reason over the graph:

```bash
ollama pull llama3.2
```

Edit `.codegraph/config.json`:
```json
{
  "agent": {
    "enabled": true,
    "model": "llama3.2"
  }
}
```

---

## Configuration

### Global config

Located at the standard OS config directory:
- **Windows:** `%AppData%\codegraph\config.json`
- **macOS:** `~/Library/Application Support/codegraph/config.json`
- **Linux:** `${XDG_CONFIG_HOME:-~/.config}/codegraph/config.json`

### Repo config

Created with `codegraph config init --repo .` at `.codegraph/config.json`:

```json
{
  "include": [],
  "exclude": ["vendor/**", "node_modules/**"],
  "languages": [],
  "embedding": {
    "enabled": false,
    "model": "nomic-embed-text"
  },
  "agent": {
    "enabled": false,
    "model": "llama3.2"
  }
}
```

### Ignore file

Create `.codegraphignore` in the repo root (same syntax as `.gitignore`):
```
build/
dist/
*.generated.go
```

---

## Architecture

```
cmd/codegraph           CLI entrypoint
internal/
  agent/                Agentic reasoning (ReAct loop over Ollama)
  cli/                  Command handlers and MCP auto-configuration
  config/               Config loading and path resolution
  embedding/            Vector embedding (Ollama HTTP client)
  export/               JSON and DOT graph export
  framework/            Framework detection (20+ frameworks)
  graph/                Core types (Symbol, Edge, Reference, etc.)
  indexer/              Repository scan, incremental updates, embedding
  mcp/                  MCP stdio server (29 tools)
  parser/               Parser interface and adapters
    treesitter/         Tree-sitter adapters (12 languages)
    golang/             Go AST parser (legacy)
    python/             Python heuristic parser (legacy)
    heuristic/          Regex-based parsers (legacy fallback)
  query/                Query orchestration and hybrid search
  store/                SQLite storage, migrations, graph analytics
  viz/                  Interactive D3.js graph visualization
  watcher/              File watch and debounced updates
```

---

## Build

```bash
go build ./cmd/codegraph
go test ./...
```

Requires Go 1.26+ and a C compiler (for tree-sitter CGo bindings).

---

## License

This project is licensed under the Functional Source License, Version 1.1, MIT Future License (`FSL-1.1-MIT`).

On the second anniversary of each version's release, that version converts to the MIT License. See `LICENSE` for details.
