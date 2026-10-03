# AGENTS.md

## Project Scope

`codegraph` is a local-first code context engine and MCP server. Keep the core engine client-agnostic. Put Codex, Gemini, and other client guidance in docs or examples rather than hard-coding client behavior into the binary.

## Architecture Map

- `cmd/codegraph`: main entrypoint and command bootstrap
- `internal/appname`: centralized product naming for easy rename
- `internal/config`: config loading, path resolution, install defaults
- `internal/cli`: command handlers and JSON/text output
- `internal/store`: SQLite connection, migrations, persistence
- `internal/indexer`: repository scan, hashing, incremental updates
- `internal/parser`: parser interfaces and language adapters
- `internal/classify`: evidence-based classification of unresolved edge targets
  (builtin/stdlib/external/unknown); owns all per-language rules
- `internal/audit`: developer-only resolver-correctness harness and its synthetic
  fixture (`go run ./internal/audit/cmd/resolveraudit`); not a CLI or MCP command
- `internal/graphaudit`: production audit of a user's already-indexed graph
  (`codegraph audit`, MCP `audit`); read-only, observational, never re-resolves
- `internal/constraints`: architectural constraints over an indexed graph
  (`codegraph check_constraints`, MCP `check_constraints`); read-only
- `internal/query`: symbol, caller, callee, impact, and stats queries
- `internal/search`: package doc only, no code; semantic search is implemented
  in `internal/store` and `internal/query` (token weights in `internal/texttoken`)
- `internal/texttoken`: token weighting for full-text and semantic ranking
- `internal/embedding`: optional vector embeddings (Ollama client, noop fallback)
- `internal/agent`: optional `agentic_query` ReAct loop over a local Ollama model
- `internal/framework`: framework and library detection (`detect_frameworks`)
- `internal/graph`: shared graph types (symbols, references, edges, parsed files)
- `internal/mcp`: stdio MCP server and tool routing
- `internal/usage`: in-process MCP context-usage meter (`serve --usage-summary`)
- `internal/limits`: shared public size policy (page, depth, and batch bounds)
- `internal/tokenest`: deterministic token estimator (ceil(bytes/4))
- `internal/detail`: progressive symbol projections and bounded source detail
- `internal/compactfmt`: `codegraph.compact/v1` tabular encoding for bulk MCP
  results (opt-in `format=compact`; JSON stays the default)
- `internal/platform`: cross-platform path and OS helpers
- `internal/version`: build and runtime version reporting
- `internal/versioncheck`: opt-out release check against GitHub releases
- `internal/doctor`: `codegraph doctor` diagnostics
- `internal/gotool`: Go binary path hints used by install and doctor output
- `internal/logging`: `slog` logger construction
- `internal/export`: JSON and DOT export
- `internal/viz`: single-file D3.js HTML graph (`codegraph visualize`; loads D3 from d3js.org)
- `internal/watcher`: file watch and debounced updates
- `internal/querybench`: read-only query latency benchmark (`codegraph bench-queries`)
- `internal/latency`: percentile arithmetic shared by query benchmarks

## Working Rules

- Preserve the local-first architecture
- Prefer one-binary workflows and minimal runtime dependencies
- Keep public interfaces narrow and explicit
- Use SQLite migrations for schema changes
- Keep JSON responses concise and predictable
- Avoid adding cloud services, heavy UI, or client-specific behavior in core packages
- Use `codegraph` MCP for codebase exploration and context queries when available (`codegraph serve --repo-root <repo>`).

## Verification

- Run `go test ./...` when tests exist
- Run `go build ./cmd/codegraph`
- For indexing changes, verify `codegraph index`, `codegraph update`, and `codegraph stats`
- For MCP changes, verify `codegraph serve` still answers `tools/list` and `tools/call`
- Prefer MCP-backed checks (`tools/list`, `tools/call`) before adding ad-hoc local inspection scripts.

## Merging

- Pull requests target `v2.0` only; `master` is read-only.
- The `v2.0 merge gate` ruleset requires a pull request, squash merges, and a green aggregate `ci` check on a branch that is up to date with `v2.0`. GitHub cannot check the independent review, so merge only through `.github/scripts/merge-gate.sh PR HEAD_SHA REVIEW_FILE --merge`. It blocks unless an independent review of that exact head SHA has completed (`PR: #N`, `Reviewed-Head: <sha>`, `Verdict: APPROVE`, `Blocking: 0`), every check on that head is green including the aggregate `ci` check, the head contains the current `v2.0` tip, and GitHub reports no conflict. It merges with `--match-head-commit`.
- A review that has started is not a review that passed. Green CI, a session limit or a queue of PRs never replace it; if the review is late, the PR stays open.
- Any new head (fix, rebase, branch update) needs green checks and a review that names the new SHA before merging.

## Release Hygiene

- Keep `README.md` and `CHANGELOG.md` aligned with shipped behavior for each tag.
- Add a new changelog section for each release (for example `v1.0.2 -> v1.0.3`) before tagging.
- Tag releases with semantic version tags (`vX.Y.Z`) after tests/build pass.
