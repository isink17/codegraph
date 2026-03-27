# Competitive Analysis: codegraph vs Similar Projects

Analysis date: 2026-03-27

## Projects Analyzed

| Project | Tech Stack | Stars | Focus |
|---------|-----------|-------|-------|
| **Our codegraph** | Go, SQLite | - | Local-first code context engine + MCP server |
| [Jakedismo/codegraph-rust](https://github.com/Jakedismo/codegraph-rust) | Rust, SurrealDB | - | Agentic graph + embeddings MCP server |
| [colbymchenry/codegraph](https://github.com/colbymchenry/codegraph) | TypeScript, SQLite | - | Semantic code intelligence for Claude Code |
| [dmnkhorvath/coderag-cli](https://github.com/dmnkhorvath/coderag-cli) | Python, SQLite/FAISS | - | Knowledge graph + RAG for AI assistants |
| [CodeGraphContext/CodeGraphContext](https://github.com/CodeGraphContext/CodeGraphContext) | Python, KuzuDB/Neo4j | - | Graph DB code indexer + MCP server |
| [vitali87/code-graph-rag](https://github.com/vitali87/code-graph-rag) | Python, Memgraph | - | Graph RAG with natural language querying |

---

## Feature Comparison Matrix

| Feature | Ours | codegraph-rust | colbymchenry | coderag-cli | CGC | code-graph-rag |
|---------|:----:|:--------------:|:------------:|:-----------:|:---:|:--------------:|
| **Parsing** |
| Tree-sitter parsing | - | Yes | Yes | Yes | Yes | Yes |
| AST-based extraction | Heuristic+Go AST | AST + FastML | tree-sitter | tree-sitter | tree-sitter | tree-sitter |
| LSP integration | - | Yes (6 langs) | - | - | - | - |
| **Languages** |
| Language count | 12 | 14 | 19 | 7 | 14 | 12 |
| Go (strong parser) | Yes | Yes | Yes | - | Yes | Yes (dev) |
| Framework detection | - | - | 12 frameworks | 11 frameworks | - | - |
| **Storage** |
| Database | SQLite | SurrealDB | SQLite | SQLite+FAISS | KuzuDB/Neo4j/FalkorDB | Memgraph |
| Zero-config / embedded | Yes | No (external DB) | Yes | Partial | Partial | No (Docker) |
| **Search & Query** |
| Full-text search (FTS5) | Yes | - | Yes | Yes | - | - |
| Semantic/vector search | Planned | Yes (HNSW) | Yes (local) | Yes (FAISS) | - | Yes |
| Natural language query | - | Yes (via LLM) | - | - | Yes (via AI) | Yes (Cypher gen) |
| Hybrid search (vector+FTS) | - | Yes (70/30) | - | Yes | - | - |
| Cross-encoder reranking | - | Yes | - | - | - | - |
| **Graph Analysis** |
| Call graph (callers/callees) | Yes | Yes | Yes | Yes | Yes | Yes |
| Impact radius/blast radius | Yes | Yes | Yes | Yes | Yes | - |
| Transitive dependencies | - | Yes | Yes | Yes | Yes | Yes |
| Cycle detection | - | Yes | - | - | - | - |
| Coupling metrics | - | Yes | - | - | - | - |
| Hub node analysis | - | Yes | - | - | - | - |
| PageRank/centrality | - | - | - | Yes | - | - |
| Community detection | - | - | - | Yes | - | - |
| Dead code detection | - | - | - | - | Yes | - |
| Complexity analysis | - | - | - | - | Yes | - |
| Architecture overview | - | Yes | - | Yes | - | - |
| Boundary violation rules | - | Yes | - | - | - | - |
| **AI/LLM Integration** |
| Agentic reasoning tools | - | Yes (4 tools) | - | - | - | - |
| Agent architectures (ReAct/LATS) | - | Yes (3 archs) | - | - | - | - |
| Context window adaptation | - | Yes | - | - | - | - |
| Context overflow protection | - | Yes | - | - | - | - |
| Configurable LLM provider | - | Yes | - | - | - | Yes |
| Local embeddings | - | Yes (Ollama/ONNX) | Yes (transformers.js) | Yes (FAISS) | - | - |
| Cloud embeddings | - | Yes (OpenAI/Jina) | - | - | - | - |
| **MCP Tools** |
| Tool count | 12 | 4 (agentic) | 8 | 16 | Many | 13 |
| Index/reindex | Yes | Yes | Yes (sync) | Yes | Yes | Yes |
| Symbol search | Yes | Via agentic | Yes | Yes | Yes | Yes |
| Callers/callees | Yes | Via agentic | Yes | Yes | Yes | - |
| Impact analysis | Yes | Via agentic | Yes | Yes | Yes | - |
| Context building | - | Yes | Yes | Yes | - | - |
| Graph stats | Yes | - | Yes | - | Yes | - |
| File listing | - | - | Yes | - | Yes | Yes |
| Session memory | - | - | - | Yes (8 tools) | - | - |
| **Developer Experience** |
| One-command install | go install | Build from source | npx installer | pip install | pip install | uv sync |
| Auto-sync / file watching | Yes (watcher) | Yes (daemon) | Yes (hooks) | Yes | Yes | Yes |
| Interactive setup wizard | - | - | Yes | - | Yes | - |
| Claude Code hooks integration | - | - | Yes | - | - | - |
| Docker support | - | - | - | Yes | - | Yes |
| **Visualization** |
| DOT/Graphviz export | Yes | - | - | - | - | - |
| Interactive web visualization | - | Yes (HTML) | - | Yes (D3.js) | Yes (premium) | Yes (gitcgr.com) |
| **Other** |
| Cross-language linking | - | - | - | Yes | - | - |
| Import resolution | - | Yes (LSP) | Yes (framework-aware) | Yes (4-strategy) | - | - |
| Related test finding | Yes | - | Yes (affected) | - | - | - |
| Git history enrichment | - | - | - | Yes | - | - |
| Pre-indexed bundles (.cgc) | - | - | - | - | Yes | - |
| Code editing/replacement | - | - | - | - | - | Yes |
| Code optimization suggestions | - | - | - | - | - | Yes |
| Multi-repo support | - | - | - | - | Yes | Yes |
| Token cost benchmarking | - | - | Yes | Yes | - | - |

---

## What We're Missing (Priority-Ordered)

### P0 - High Impact, Core Differentiators

#### 1. Tree-sitter Based Parsing
**Who has it:** Everyone except us.
**What it gives:** Robust, language-agnostic AST parsing that correctly handles all syntax edge cases. Our heuristic parsers (regex-based for most languages, Go AST only for Go) miss symbols, produce false edges, and can't handle complex nesting. Tree-sitter is the industry standard for multi-language code intelligence.

#### 2. Semantic / Vector Search
**Who has it:** codegraph-rust, colbymchenry, coderag-cli, code-graph-rag.
**What it gives:** Find code by meaning, not just text. Search for "authentication logic" and find `validateToken`, `loginHandler`, `AuthMiddleware` even if the word "auth" doesn't appear. This is table-stakes for AI-powered code understanding. Options:
- Local embeddings (transformers.js, ONNX, Ollama) - zero cloud dependency
- Cloud embeddings (OpenAI, Jina) - higher quality
- Hybrid search (70% vector + 30% FTS) as codegraph-rust does

#### 3. Context Building Tool
**Who has it:** codegraph-rust (`agentic_context`), colbymchenry (`codegraph_context`), coderag-cli (`coderag_file_context`).
**What it gives:** A single MCP tool call that returns everything an AI agent needs for a task - entry points, related symbols, code snippets, and relationships. This is the highest-value MCP tool because it replaces multiple grep/read cycles with one call. Our tools require the AI to manually compose multiple queries.

#### 4. Framework-Aware Resolution
**Who has it:** colbymchenry (12 frameworks), coderag-cli (11 frameworks).
**What it gives:** Understanding framework-specific patterns: Express route handlers, React component hierarchies, Laravel controllers, Django views, etc. Without this, the graph misses critical relationships that exist through framework conventions rather than direct function calls. Frameworks supported by competitors:
- Express, React, Svelte, Vue, Angular
- Laravel, Symfony, Django, Flask, FastAPI
- Go (frameworks), Java (Spring?), Rust, Ruby, Swift, C#

### P1 - Important Capabilities

#### 5. Transitive Dependency Analysis
**Who has it:** codegraph-rust, colbymchenry, coderag-cli, CGC.
**What it gives:** Our `get_impact_radius` does BFS to a depth, but we don't expose full transitive dependency chains. codegraph-rust has dedicated graph functions for transitive deps, call chain tracing, and reverse deps.

#### 6. Import/Cross-File Resolution
**Who has it:** codegraph-rust (LSP), colbymchenry (4-strategy resolver), coderag-cli.
**What it gives:** Connecting function calls to their definitions across files. colbymchenry uses exact match, suffix match, short name match, and framework-specific patterns. codegraph-rust uses actual LSP servers for type-aware linking.

#### 7. Interactive Visualization
**Who has it:** coderag-cli (D3.js), CGC (premium web viz), code-graph-rag (gitcgr.com).
**What it gives:** Interactive HTML/web-based code graph exploration. We only have DOT/Graphviz export which requires external tools. An interactive web view would be much more accessible.

#### 8. File Listing / Project Structure Tool
**Who has it:** colbymchenry (`codegraph_files`), CGC, code-graph-rag.
**What it gives:** An MCP tool that returns the project file structure from the index. Faster than filesystem scanning and useful for AI agents to understand project layout.

#### 9. Architecture Overview Tool
**Who has it:** codegraph-rust (`agentic_architecture`), coderag-cli (`coderag_architecture`).
**What it gives:** High-level view of the system: module structure, API surfaces, architectural patterns, key metrics. Helps AI agents understand the big picture before diving into specifics.

### P2 - Nice to Have

#### 10. Session Memory
**Who has it:** coderag-cli (8 MCP tools for session tracking).
**What it gives:** Cross-session context persistence - tracking file reads, edits, decisions, and discovered facts. Helps the AI remember what was done in previous sessions.

#### 11. Graph Analytics (PageRank, Coupling, Complexity)
**Who has it:** codegraph-rust (coupling, cycles, hub nodes), coderag-cli (PageRank, community detection), CGC (dead code, complexity).
**What it gives:** Advanced code quality metrics: which symbols are most central, which modules are tightly coupled, where the complexity hotspots are.

#### 12. Dead Code Detection
**Who has it:** CGC.
**What it gives:** Identifies functions/classes with no callers or references. Simple to implement with our existing graph data.

#### 13. Affected Tests Discovery (Enhancement)
**Who has it:** colbymchenry (`codegraph affected`).
**What it gives:** We have `find_related_tests` but colbymchenry has a much richer CLI command that accepts git diff output, supports glob filtering, custom depth, and CI integration. Their version is more practical for real workflows.

#### 14. Setup Wizard / Interactive Installer
**Who has it:** colbymchenry (npx installer), CGC (`cgc mcp setup`).
**What it gives:** Zero-friction onboarding. colbymchenry's installer auto-configures Claude Code MCP, sets permissions, adds CLAUDE.md instructions, and installs hooks. CGC's wizard detects and configures 10+ AI IDEs.

#### 15. Agentic Reasoning Layer
**Who has it:** codegraph-rust (Rig, ReAct, LATS architectures).
**What it gives:** Instead of returning raw data, the MCP tools run an internal reasoning agent that plans, searches, analyzes, and synthesizes an answer. This is codegraph-rust's biggest differentiator - but it requires an LLM provider and adds complexity/cost.

#### 16. Multi-Database Backend Support
**Who has it:** CGC (KuzuDB, FalkorDB, Neo4j).
**What it gives:** Flexibility. Our SQLite-only approach is actually a strength (zero-config, embedded), but having optional backends for large codebases could be useful.

#### 17. Pre-indexed Bundles
**Who has it:** CGC (.cgc files).
**What it gives:** Instantly load famous repositories without indexing. A distribution format for sharing indexed graphs.

#### 18. Cross-Language Linking
**Who has it:** coderag-cli.
**What it gives:** Connecting PHP routes to JS fetch calls, Python APIs to TS clients. Understanding the full stack across language boundaries.

---

## Competitor Strengths Summary

### codegraph-rust - "The Feature King"
The most ambitious project. Key innovations:
- **Agentic tools with internal reasoning** (ReAct/LATS/Rig architectures)
- **SurrealDB with HNSW vector index** for hybrid search
- **LSP integration** for type-aware linking (6 languages)
- **Tiered indexing** (fast/balanced/full) for speed/quality tradeoff
- **Context window adaptation** - adjusts behavior to model's context size
- **Context overflow protection** - prevents expensive failures
- **Architecture boundary rules** - custom package dependency constraints
- Weakness: Requires SurrealDB, complex setup, Rust compilation

### colbymchenry/codegraph - "Best DX for Claude Code"
Closest competitor to us in philosophy. Key innovations:
- **npx one-line installer** with interactive Claude Code setup
- **Claude Code hooks** for automatic index sync
- **Local embeddings** via transformers.js (no API keys)
- **19 languages** via tree-sitter (including Svelte, Liquid, Pascal/Delphi)
- **Framework-aware import resolution** (12 frameworks)
- **Affected tests** with git diff piping for CI
- **Benchmark data** showing 30% fewer tokens, 25% fewer tool calls
- Weakness: Node.js dependency, no agentic reasoning

### coderag-cli - "The Full-Stack Analyzer"
Most comprehensive analysis toolkit. Key innovations:
- **16 MCP tools + 8 session memory tools** (24 total)
- **PageRank and community detection** for code importance
- **Cross-language linking** (PHP routes → JS fetch calls)
- **11 framework detectors** (Laravel, Express, Django, etc.)
- **Session memory** for cross-session context
- **Interactive D3.js visualization**
- **86% token savings** claimed with benchmarks
- **Docker deployment** option
- Weakness: Python, 7 languages only, complex setup

### CodeGraphContext - "The Multi-DB Graph Pioneer"
Focus on graph database flexibility. Key innovations:
- **3 database backends** (KuzuDB, FalkorDB, Neo4j)
- **Pre-indexed bundles** (.cgc) for famous repos
- **Dead code detection** and **complexity analysis**
- **Premium interactive visualization** (glassmorphism, search, layouts)
- **10+ IDE auto-configuration** (VS Code, Cursor, Windsurf, Claude, Gemini CLI, Kiro, etc.)
- **14 languages** with tree-sitter
- Weakness: No embeddings/semantic search, setup complexity with graph DBs

### code-graph-rag - "The RAG System"
Full RAG pipeline with code editing. Key innovations:
- **Memgraph** graph database with Cypher queries
- **Natural language → Cypher translation** (multi-provider: Gemini, OpenAI, Ollama)
- **Code editing tools** (surgical replace, write file) in MCP
- **Code optimization suggestions** with reference-guided approach
- **gitcgr.com** - visualize any GitHub repo by changing URL
- **Multi-provider LLM support** (mix orchestrator + cypher model)
- Weakness: Requires Docker for Memgraph, complex setup

---

## Our Strengths (What to Keep)

1. **Zero-config embedded SQLite** - No external databases, no Docker, no API keys needed
2. **Single Go binary** - Cross-platform, no runtime dependencies
3. **Fast, lightweight** - Minimal resource footprint
4. **Local-first philosophy** - No data leaves the machine
5. **Clean MCP protocol** - Well-structured tool definitions
6. **Incremental updates** - File watcher with debounce
7. **DOT export** - Graph visualization (though basic)

---

## Recommended Roadmap

### Phase 1: Foundation (tree-sitter + embeddings) --- DONE
- [x] Migrate to tree-sitter parsing for all languages (`internal/parser/treesitter/` — 13 adapters: Go, Python, TS/JS, Java, Rust, C#, Ruby, Swift, PHP, C/C++, Kotlin)
- [x] Add local embedding support via Ollama HTTP client (`internal/embedding/`)
- [x] Implement hybrid search — vector + FTS5 with Reciprocal Rank Fusion (`store.HybridSearch`)
- [x] Schema migration for `symbol_embeddings` table (006)
- [x] Embedder wired into indexer (opt-in via `.codegraph/config.json` `"embedding": {"enabled": true}`)
- [x] `search_semantic` MCP tool auto-upgrades to hybrid search when embeddings are present
- [x] Add `context_for_task` MCP tool (semantic search → caller/callee expansion → test discovery → ranked context bundle)

### Phase 2: Graph Intelligence --- DONE
- [x] Add `list_files` MCP tool (path prefix filter, language, size)
- [x] Add `architecture_overview` MCP tool (languages, directories, symbol/edge kinds, entry points, hub symbols)
- [x] Add `find_dead_code` MCP tool (symbols with zero callers/references, excludes main/init/tests)
- [x] Improve import/cross-file resolution (4-strategy edge resolver: exact, name, suffix, method receiver — integrated into indexer)
- [x] Add `trace_dependencies` MCP tool (BFS transitive upstream/downstream chains with configurable depth)
- [x] Enhance `find_related_tests` with multi-file support + `affected-tests` CLI command (stdin piping, JSON output)

### Phase 3: Developer Experience --- DONE
- [x] Interactive installer with auto-configuration for Claude Code, Cursor, Windsurf, Gemini CLI (`install` command auto-detects and configures)
- [x] Interactive web-based graph visualization (`visualize` CLI command — D3.js force graph with search, zoom, dark theme)
- [x] Framework detection — 20+ frameworks via `detect_frameworks` MCP tool (gin, Express, Django, React, Spring, Laravel, etc.)
- [x] Token cost benchmarking via `benchmark_tokens` MCP tool (estimates savings vs raw file reading)

### Phase 4: Advanced --- DONE
- [x] Cross-language linking via `cross_language_links` MCP tool (shared name matching + import-path linking)
- [x] Graph analytics via `graph_analytics` MCP tool (PageRank, coupling metrics, cycle detection)
- [x] Session memory — 4 MCP tools: `session_log`, `session_history`, `session_hot_files`, `session_context`
- [x] Agentic reasoning via `agentic_query` MCP tool (ReAct loop over Ollama LLM, chains internal tools, synthesizes answers)
