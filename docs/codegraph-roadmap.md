# Codegraph CLI/Help Roadmap (Large Repo Stability)

This document captures the current CLI/help architecture of `codegraph` and proposes an incremental path to a cleaner command/help system without a big refactor up front.

## Current CLI Architecture (As Implemented)

- **Entrypoint:** `cmd/codegraph/main.go` calls `internal/cli.Run(ctx, os.Args[1:], stdout, stderr)`.
- **Root command parsing:** `internal/cli/app.go:Run` performs:
  - A startup version check (`versioncheck.NotifyIfOutdated`).
  - Global config load (`config.Load`) and logger initialization.
  - A `switch args[0]` dispatch on the first positional argument.
- **Subcommand dispatch:**
  - Root dispatch is a big `switch` in `Run` over `args[0]`.
  - Some commands have sub-dispatch:
    - `config` uses `switch args[0]` for `show|edit-path|validate|init`.
    - `graph` uses an additional layer (notably `graph export ...`).
  - Most commands implement their own `flag.FlagSet` parsing and bespoke error messages.
- **Usage/help printing:**
  - `internal/cli/app.go:printUsage` prints a static, hand-maintained list of commands and brief notes.
  - Root usage is printed only when `len(args) == 0`. There is no dedicated `help` subcommand.
  - Many commands return `errors.New("usage: ...")` for missing args, rather than rendering a structured per-command help text.
  - `flag.FlagSet` instances are generally created with `flag.ContinueOnError` and `fs.SetOutput(io.Discard)`, which suppresses the standard `-h/--help` output.

## Current Command List

Observed in `internal/cli/app.go:Run` dispatch and `printUsage`:

- `install`
- `index <repo-path>`
- `update <repo-path>`
- `stats <repo-path>`
- `find-symbol <repo-path> <query>`
- `search <repo-path> <query>`
- `callers <repo-path> --symbol <name>`
- `callees <repo-path> --symbol <name>`
- `impact <repo-path> [--symbol <name>]... [--file <path>]...`
- `doctor`
- `config <show|edit-path|validate|init>`
- `benchmark`
- `serve --repo-root <repo-path>`
- `watch <repo-path>`
- `graph export <repo-path> [--format json|dot] ...`
- `affected-tests [--repo-root PATH] [--stdin] [--json] [--limit N] <file>...`
- `visualize [--repo-root PATH] ...`
- `clean [repo-path] [--vacuum]`

Notes:

- `update` appears in dispatch and `printUsage`, but its behavior is implemented via `runIndex(..., update=true)` (shared with `index`).
- `graph` appears in dispatch as a top-level command, but usage text focuses on `graph export ...`.

## Current Problems (Help/Dispatch/Usability)

These issues matter more on large repositories because users end up iterating on the CLI more and need predictable discovery, consistent naming, and a low-friction help surface.

- **Help is not discoverable:**
  - Running `codegraph` with no args prints a usage block, but `codegraph help` is not a thing today.
  - `-h/--help` behavior is inconsistent or absent because most `FlagSet`s discard output and don’t provide a `Usage` function.
- **Usage text is duplicated and hand-maintained:**
  - Root usage is a static list in `printUsage`.
  - Many commands return a hard-coded `usage: codegraph ...` error string in multiple places.
  - Several usage strings hardcode `codegraph` rather than using `internal/appname.BinaryName`, making renames harder and risking drift.
- **Command naming is inconsistent with desired canonical naming:**
  - Query-related commands are currently a mix of hyphenated and short nouns (`find-symbol`, `callers`, `callees`, `impact`).
  - Desired canonical names are action-oriented and explicit, and use `snake_case` to match the MCP/tooling surface.
- **Dispatch architecture is monolithic:**
  - Root `switch args[0]` centralizes all commands in one function; adding commands expands the switch and the hand-maintained usage list.
  - Command-specific parsing/usage logic is scattered across the file with no single source of truth for command metadata.
- **Error UX is inconsistent:**
  - Some errors are plain `unknown command "..."`.
  - Some missing-arg errors return `usage: ...` without showing the full help for the subcommand.
  - Some commands treat repo-root as a positional, others as a flag (`--repo-root`), which is fine but should be described consistently in help output.

## Recommended Canonical Command Naming

Define canonical, stable names for the CLI surface. These become the names shown in `codegraph help` and documentation.

### Source of Truth

The repo should have a single, vendor-neutral source of truth for tool/assistant-facing naming and usage. See `docs/ai-assistant-guidance.md` and the root `README.md` (MCP tools and setup) for the intended naming style and surface area.

### Canonical Names (Documented In Repo Docs)

These names are grounded in existing repo docs, and represent the intended "clean" surface to converge on:

- `graph_stats`
- `update_graph`
- `find_symbol`
- `find_callers`
- `find_callees`
- `get_impact_radius`
- `find_related_tests`
- `search_symbols`
- `search_semantic`

### Backward-Compatible Aliases To Keep

Preserve the existing command surface as aliases (at least through one major release), so users and scripts don’t break:

- `find-symbol` -> `find_symbol`
- `callers` -> `find_callers`
- `callees` -> `find_callees`
- `impact` -> `get_impact_radius`
- `stats` -> `graph_stats`
- `index` -> `update_graph`
- `update` -> `update_graph`
- `affected-tests` -> `find_related_tests`
- `search` -> `search_symbols`

Implementation note: the alias should be implemented at the dispatch layer (mapping old name to canonical handler), not by duplicating parsing/logic in two places.

## Recommended Implementation Order (Incremental)

This order keeps diffs small and aims for immediate UX improvements without rewriting the CLI.

1. **Add a real help surface at the root**
   - Support `codegraph help` and `codegraph help <command>`.
   - Support `codegraph -h`, `codegraph --help`, and `codegraph <command> --help`.
2. **Centralize command metadata (names, aliases, one-line summary)**
   - Create a lightweight registry (struct slice/map) that enumerates commands and provides:
     - canonical name
     - aliases
     - short description
     - usage synopsis
     - handler function
   - Use this registry to render root help and to resolve aliases.
3. **Normalize how `FlagSet` help is rendered**
   - Stop discarding `FlagSet` output.
   - Provide a consistent `Usage` function per command that prints synopsis + flags.
   - Treat `flag.ErrHelp` as a successful, user-help result (exit code 0) rather than an error.
4. **Introduce canonical query command names + alias mapping**
   - Add the canonical names above (from repo docs), keep the existing aliases.
   - Keep JSON output, flags, and behavior identical (only names/help changes).
5. **Tighten naming consistency in output strings**
   - Replace hard-coded `codegraph` in usage strings with `appname.BinaryName` where practical.
   - Keep this change narrow and localized (no broad string rewrites).

## Implementation Plan for Help and Command Dispatch

The goal is to improve help and command discovery first, without re-architecting everything.

1. Add a `help` command handler in `internal/cli/app.go` that:
   - With no args: prints root help (command list with one-line summaries).
   - With `help <cmd>`: prints that command’s usage + flags (including aliases).
2. Create a minimal command table (in `internal/cli/app.go` initially) and route root dispatch through it:
   - Keep existing handler functions (`runIndex`, `runStats`, `runQueryCommand`, etc.).
   - Add a small alias resolver: map `find-symbol` to canonical command handler name.
3. For each command handler that uses `flag.FlagSet`:
   - Set output to `stdout` for help and usage.
   - Provide a `fs.Usage` closure that prints a stable synopsis, then `fs.PrintDefaults()`.
   - Treat `flag.ErrHelp` as a non-error result (exit code 0).
4. Keep `printUsage` as a compatibility fallback initially, then migrate it to render from the command table.

### Help-System Expectations

- Every command has a consistent "help path":
  - `codegraph help`
  - `codegraph help <command>`
  - `codegraph <command> --help` (and `-h`)
- Help is not an error:
  - If a `FlagSet` returns `flag.ErrHelp`, render help/usage and return success (exit code 0).
  - Usage errors (missing required args, unknown subcommand) should return an error and include a short usage line (and optionally a hint to run `codegraph help <command>`).
