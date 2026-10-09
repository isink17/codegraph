# Architectural constraints (`check_constraints`)

`check_constraints` checks dependency rules between path groups that you declare,
against a graph that `codegraph index` has already built. It does not index or
re-resolve anything, and it never writes to the database.

```bash
codegraph index . && codegraph check_constraints .
```

## Names

| Surface | Name | Notes |
|---|---|---|
| CLI | `check_constraints` | canonical name |
| CLI | `check-constraints` | alias; same command, same output, same exit code |
| MCP | `check_constraints` | listed in `full` mode; in `gateway` mode, find it with `tool_search` and run it with `tool_call` |

CLI flags: `[PATH]` or `--repo-root PATH`, `--config FILE`, `--limit N`, `--offset N`,
`--strict-freshness`.
MCP arguments: `limit`, `offset`, `strict_freshness`. The MCP tool takes no config path, so a tool call
cannot make the server read an arbitrary file. It reads only the repo-root file.

## Config file

The default location is `.codegraph-constraints.json` at the repository root, so
it is committed with the code. (`.codegraph/` holds the database and is usually
git-ignored.) The CLI's `--config FILE` can point at another file.

```json
{
  "schema_version": 1,
  "groups": {
    "domain": {"include": ["internal/domain/**"], "exclude": ["internal/domain/**/*_test.go"]},
    "infra":  {"include": ["internal/infra/**"]},
    "api":    {"include": ["cmd/**", "internal/api/**"]}
  },
  "rules": [
    {"id": "domain-no-infra", "kind": "forbidden_dependency", "from": ["domain"], "to": ["infra"]},
    {"id": "api-layer",       "kind": "allowed_dependencies", "from": ["api"],    "to": ["domain"]},
    {"id": "domain-owned",    "kind": "allowed_dependents",   "of":   ["domain"], "from": ["api"]},
    {"id": "no-group-cycles", "kind": "forbidden_cycles",     "groups": ["api", "domain", "infra"]}
  ]
}
```

Parsing is strict. Any unknown field is an error, and so is any value of the wrong type.

### Groups and patterns

A group needs a non-empty `include` list. It can also have an `exclude` list. A file
belongs to a group when it matches an `include` pattern and no `exclude` pattern. A file
can belong to at most one group: a live file that matches two groups is a
`group_overlap` error, so use `exclude` to carve one group out of another. A file
that matches no group is *unowned*.

Patterns are anchored at the repository root and matched one path segment at a time:

- `*`, `?` and `[...]` match inside a single segment.
- `**` as a whole segment matches zero or more segments.
- There is no basename fallback. `*.go` matches only top-level files, and
  `**/*.go` matches them at any depth.
- Matching is byte-exact on every OS. Nothing is case-folded, trimmed or
  separator-translated. `Internal/**` does not match `internal/x.go`.
- A pattern must be a valid repository-relative path: no leading `/`, no
  empty, `.` or `..` segment, no drive letter, no backslash. A backslash is
  always an error, so a Windows spelling such as `internal\store\**` fails
  loudly instead of matching nothing. On POSIX, a file whose name contains `\`
  can only be matched through a wildcard (`dir/a?b` matches `dir/a\b`).

### Rules

A dependency from a file in group S to a file in group T is a *crossing* when
S ≠ T. Dependencies inside one group are always allowed.

| kind | fields | violation |
|---|---|---|
| `forbidden_dependency` | `from` (≥1), `to` (≥1), disjoint | S ∈ `from` and T ∈ `to` |
| `allowed_dependencies` | `from` (≥1), `to` (≥0) | S ∈ `from` and T ∉ `to`; an unowned T is a violation |
| `allowed_dependents` | `of` (≥1), `from` (≥0) | T ∈ `of` and S ∉ `from`; an unowned S is a violation |
| `forbidden_cycles` | `groups` (≥2 distinct) | the group graph induced on `groups` has a cycle |

Because unowned endpoints violate the allow-list rules, adopting the tool on part
of a repository needs a catch-all group, such as
`{"other": {"include": ["**"], "exclude": ["internal/**"]}}`.

### What counts as a dependency

The rules evaluate every resolved edge of the repository (calls and constructions)
when all of these hold:

- its resolution confidence is `high` or `medium`;
- its source symbol's file, target symbol's file and evidence file are all live.

Groups are matched against the files that hold the source and target symbols. The
evidence file is not used for matching.

These never become violations:

- unresolved edges;
- import strings;
- low-confidence edges, which includes every `cross_language_ref`;
- rows of deleted files;
- rows of other repositories in the same database.

Unresolved and excluded edges are counted in `coverage` instead, because zero
findings is not proof of zero dependencies.

## Result

```json
{
  "schema": "codegraph.constraints/v1",
  "status": "violations",
  "errors": [],
  "config": {"path": ".codegraph-constraints.json", "source": "repo", "schema_version": 1, "sha256": "…"},
  "index": {"last_indexed_at": "…", "last_scan_id": 7, "dirty_files": 0},
  "summary": {"rules": 4, "findings": 1, "occurrences": 1, "cycles": 0, "by_rule": {"domain-no-infra": 1}},
  "coverage": {
    "trusted_dependencies_evaluated": 120,
    "excluded_by_trust_filter": {"domain→infra": 1},
    "unresolved_dependency_rows_by_source_group": {"domain": 14, "(unowned)": 3},
    "files_without_call_edges_by_group_and_language": {},
    "unowned_files": 2,
    "unmatched_patterns": []
  },
  "findings": [{
    "rule_id": "domain-no-infra", "kind": "forbidden_dependency",
    "source_group": "domain", "target_group": "infra",
    "edge_kind": "calls",
    "source": {"path": "internal/domain/a.go", "line": 5, "symbol": "domain.A", "symbol_start_line": 5, "stable_key": "…"},
    "target": {"path": "internal/infra/b.go", "symbol": "infra.B", "start_line": 3, "stable_key": "…"},
    "resolution_strategy": "…", "resolution_confidence": "high", "evidence": "…",
    "occurrences": 1
  }],
  "findings_truncated": false,
  "cycles": [],
  "cycles_truncated": false
}
```

- **Findings.** A finding is identified by (rule_id, source path, line, edge_kind,
  source symbol start line, source stable_key, target path, target start line,
  target stable_key). Rows with the same identity collapse into one finding, and
  `occurrences` counts them. Findings are sorted by that identity.
  `source_group`/`target_group` is `null` for an unowned endpoint.
- **Cycles.** There is one entry per strongly connected component of a
  `forbidden_cycles` rule's group graph:
  - `members` is the sorted list of groups in the component;
  - `witness` is the shortest return path from the smallest member, and the
    lexicographically smallest one when several paths are equally short;
  - `hops` gives one witnessing dependency per step.

  Cycles are sorted by (rule_id, members).
- **Summary.**
  - `summary.findings`, `summary.occurrences` and `summary.cycles` are exact, whatever page you ask for.
  - `by_rule` counts findings for dependency rules and components for `forbidden_cycles` rules.
- **Paging.**
  - `limit` defaults to 20 when it is 0 or omitted. Values from 1 to 500 are accepted; anything above 500 or below 0 is an error.
  - `offset` pages through `findings`. It must be 0 or more.
  - `limit` also caps `cycles`.
  - Truncation never changes the status or the exit code.
- **Coverage.** Coverage map keys use group names. `(unowned)` stands for files outside every group.
  - `files_without_call_edges_by_group_and_language` counts files whose parser emitted no call edges, for example Ruby indexed by a non-CGO build.
  - The constraints file itself is left out of every count.
- **No host paths.** The result never contains the database path or the repository
  root. A `--config` file outside the repository is reported as
  `{"path": null, "source": "external"}`.
- **Same bytes for the same tree.** A fresh index and an incremental history of the
  same tree give byte-identical `findings`, `cycles`, `summary` and `coverage`.
  `index` is history metadata and is not part of that guarantee.
- **CLI and MCP agree.** The MCP tool's `data` is the CLI result. The CLI prints it
  indented and MCP compact; after removing insignificant whitespace (`json.Compact`) the
  bytes are identical, including the escaping of `<`, `>` and `&`.

## Statuses and exit codes

The status is the first of these that applies:

| status | meaning | CLI exit |
|---|---|---|
| `config_error` | the document is invalid (`errors` lists each problem) | 2 |
| `not_configured` | no constraints file was found | 2 |
| `not_indexed` | the repository has no graph yet | 2 |
| `stale` | the watcher has queued changes that are not yet indexed (`index.dirty_files > 0`); findings are still computed | 2 |
| `violations` | at least one finding or cycle | 1 |
| `ok` | none | 0 |

The CLI writes the JSON to stdout before deciding the exit code. Invalid
arguments also exit 2. Over MCP every status is returned as `data.status`, never
as a tool error.

Checks happen in this order: the config is read and validated first, without the
index; then the index is opened read-only; then group overlap is checked; then
staleness. Without a watcher, `stale` never fires. In CI, freshness is the
caller's job: run `codegraph index` (or `update`) first.

### Strict freshness (`--strict-freshness`, MCP `strict_freshness: true`)

Opt-in. Without it the result and the exit code are exactly as above. With it, a
result whose index was opened (`stale`, `violations`, `ok`) gains a `freshness`
object, and the exit code also requires proof that the whole repository was
walked at the current HEAD. `config_error`, `not_configured` and `not_indexed`
are unchanged (exit 2, no `freshness`).

| `freshness.verdict` | meaning | CLI exit |
|---|---|---|
| `full_coverage_at_head` | no known staleness, and the newest completed full scan started and finished at the current HEAD, overlapped no other scan, and no scan has failed since | 0 (`ok`) or 1 (`violations`) |
| `known_stale` | `status` is `stale`, or an independent stale fact: a failed latest scan (its committed batches belong to no completed scan), a non-empty dirty queue, or HEAD moved past the history watermark. A running scan newer than the last completed one is not such a fact here, although graph_stats reports `known_stale` for it: with no other stale fact it gives `unknown` (4); with one, 3 | 3 |
| `unknown` | the deciding fact was not recorded or not readable: no completed scan, a database from before coverage recording (read-only, unmigrated), a full scan without recorded HEADs, HEAD unreadable now, a full scan that overlapped another scan, any scan row still `running` (a live writer or one a crashed process abandoned; it yields 4 until the row is closed: `codegraph recover-scans` records it `failed` once no live scan holds the scan lock and refuses otherwise, after which a completed full scan is still required; `codegraph index --rebuild` also clears it by deleting the repository database), or scan activity between the freshness read that precedes every graph read and the one after the evaluation | 4 |
| `insufficient_coverage` | no known staleness, but no recorded full scan, or a scan failed after it, or HEAD moved during it or since it (a path-scoped `update` or watch flush never counts as full, even when it advanced the history watermark) | 5 |

A proven gap (`insufficient_coverage`) outranks `unknown`. `freshness` also carries
`reasons`, the graph_stats `state` and `state_reasons` (never "fresh";
`no_known_staleness` at best), `head_now`, `last_full_scan` and `exit_code`.
A full scan is `index`, or `update` without paths, under the repository's own
configuration; language, include or exclude overrides passed by an internal caller
that differ from it make a `filtered` scan, which does not count (no CLI or MCP
surface sets them). A failed scan counts against coverage when it started after the
full scan or closed no earlier than it started.

`full_coverage_at_head` is not "fresh": uncommitted edits, watcher events that
were never queued, a changed repository configuration and a HEAD that moved away
and back during the scan are not detected. A database read that fails with an error (unlike an
unrecorded fact or an unreadable HEAD, which give `unknown`) is not a verdict: the CLI exits 2 and MCP returns a
tool error, as in default mode. Like `index`, `freshness` is history
metadata outside the same-bytes guarantee; CLI and MCP still agree byte for byte.

### Config error codes

Each error is `{location, code, message}`, and errors are sorted by location and
then code.

| code | meaning |
|---|---|
| `json` | malformed JSON, a duplicate key, or a value of the wrong type |
| `unknown_field` | a field the schema does not define, at any level |
| `schema_version` | missing, or not 1 |
| `group_name` | empty, duplicated, or not `[a-z][a-z0-9_-]*` |
| `group_include_empty` | a group's `include` is missing or empty |
| `pattern` | an invalid pattern (see above) |
| `rule_id` | missing, empty or duplicated |
| `rule_kind` | missing or unknown kind |
| `rule_field` | a field not allowed for the kind, or a required field missing or empty |
| `rule_group` | a rule names an undeclared group |
| `rule_contradiction` | `forbidden_dependency` with overlapping `from` and `to`, or `forbidden_cycles` with fewer than 2 distinct groups |
| `group_overlap` | a live file belongs to more than one group; at most 20 are reported, sorted by path, with location set to the file |
