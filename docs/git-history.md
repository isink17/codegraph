# File-level Git history

Every `codegraph index` and `codegraph update` (and the MCP `index_repo` and
`update_graph` tools) also records a small, bounded summary of the Git history of
each file. It is enrichment only: no history value creates, removes, ranks or
re-resolves a symbol, edge or reference, and the semantic graph is identical with
history on or off.

## What is stored

The **window** is the latest 250 first-parent commits reachable from `HEAD` when
the scan ran. That commit is the **watermark**. The window is anchored to it, not
to the wall clock, so the same commit graph always produces the same values.

Per file present in the watermark commit and touched in the window:

| Field | Meaning |
|---|---|
| `commit_count` | window commits that changed the file |
| `first_commit`, `last_commit` | oldest and newest of those commits (`sha`, `committer_time`), ordered by committer time, then SHA |
| `author_count` | distinct author emails, after `.mailmap` (`%aE`); nothing else is merged |
| `top_author`, `top_author_commits` | the author with the most commits; ties go to the smaller email |
| `lines_added`, `lines_deleted` | `git log --numstat` line churn; binary files count 0 |
| `revert_count` | commits that are explicit Git reverts (below) |
| `worktree_differs` | the working tree, which the graph was built from, differs from the watermark (modified, staged, deleted or untracked) |
| `indexed` | the path is a live file of the semantic graph |

Rules:

- **Merges:** first parent only (`--first-parent`). A merge counts once, with its
  diff against the first parent and under the merge's author; commits of the
  merged branch are not attributed.
- **Renames:** `-M` (Git's default 50% similarity), followed inside the window,
  so a renamed file keeps the commits made under its old name. No copy
  detection. Otherwise history is by path, as `git log -- <path>` reports it.
- **Reverts:** a commit is a revert when its subject starts with `Revert "` or a
  message line starts with `This reverts commit <hex>` — the two markers Git (and
  GitHub's revert button) write. The word "revert" in prose, a `revert:` prefix or
  an issue reference does not count.
- **Paths:** Git runs as `git -C <root> ... --relative`, so a codegraph root that
  is a subdirectory of the repository gets root-relative slash paths that join
  `files.path` directly. History from before a file moved into the root is outside
  the root and is not attributed. The window still counts repository commits.
- **Submodules:** their histories are not traversed, and indexed files inside a
  submodule report `worktree_differs` true (the superproject tracks only the
  submodule entry).
- **Configuration:** `log.showRoot`, `log.diffMerges`, `diff.renames`,
  `diff.algorithm` (Myers), `diff.renameLimit` (1000), signatures and color are
  overridden. Only the repository's own `.mailmap` is read; user-level
  `mailmap.file`/`mailmap.blob` settings are ignored, because they could change
  results without changing `HEAD`. Git configuration and object replacements
  passed through the environment (`GIT_CONFIG_*`, `GIT_REPLACE_REF_BASE`,
  `GIT_GRAFT_FILE`, ...) are ignored too, and `core.fsmonitor` is not run.
- **Untracked files:** every indexed file Git does not track, including files
  `.gitignore` excludes but codegraph indexes (generated code, for example),
  appears with `commit_count` 0 and `worktree_differs` true. codegraph's own
  `.codegraph/` directory is never a worktree change.
- **Freshness:** history is evaluated by `index` and `update`. A commit that
  changes no file (an empty commit, or a commit of already-indexed content) moves
  `HEAD` without triggering a watch update, so the answer keeps the previous
  `watermark` until the next scan; compare it with `git rev-parse HEAD` when that
  matters.

## Repository state

`history.status` is `ok`, `truncated` (a shallow clone: values are published but
the window may be cut short) or `absent`, with `absent_reason`:

| Reason | Meaning |
|---|---|
| `not_a_git_repository` | the root is not inside a Git repository |
| `git_unavailable` | no `git` executable on `PATH` |
| `no_commits` | the repository has no commits yet |
| `unsafe_directory` | Git refused the repository (`safe.directory`, dubious ownership) |
| `git_timeout` | a Git command exceeded 60 s |
| `git_failed` | any other Git failure inside a repository (corrupt objects, ...) |
| `disabled` | the scan ran with `--no-history` |
| `not_computed` | no scan has stored history yet |

None of these fails the scan. Git runs read-only (`GIT_OPTIONAL_LOCKS=0`, so it
never rewrites the index) and ignores `GIT_DIR`/`GIT_WORK_TREE` and similar
variables, so history always describes the repository that contains the root.

## Updates

The state stores the watermark, the window size, the algorithm version
(`file-v1`) and a fingerprint of the repository's `.mailmap`, which Git reads
from the working tree. When all of them are unchanged and the clone is not
shallow, an update only re-reads the worktree change set. A shallow clone is
always recomputed, because deepening it grows the window without moving `HEAD`. Otherwise the window is recomputed
from the new watermark — a bounded `git log -n 250`, never full history. That
covers rebases, resets and branch switches (the old watermark need not be an
ancestor) and keeps every stored value equal to a fresh index of the same state.

## Reading it

MCP: `file_history` is not in `tools/list` in either tool mode, so default
sessions are unchanged. Call it by name, or find it in gateway mode with
`tool_search("git history")`. Arguments: `files` (repository-relative paths) or
a listing with `path_filter`, `limit` and `offset`. A listing orders by
`commit_count` descending, then path.

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call",
 "params":{"name":"file_history","arguments":{"files":["internal/store/store.go"]}}}
```

CLI, same document:

```sh
codegraph file_history .                                   # files with the most commits first
codegraph file_history . --file internal/store/store.go    # one or more files
codegraph file_history . --path-filter internal/ --limit 50
codegraph index . --no-history                             # skip it (recorded as disabled)
```

Symbol-level history is not provided.
