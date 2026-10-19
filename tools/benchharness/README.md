# benchharness

Developer tool that produces one machine-readable record
(`codegraph.benchmark/v1`) per run of a pinned fixture, validates it, and
renders a Markdown summary from the record alone. Not part of the codegraph
binary.

## Run the bundled fixture

    SHA=$(git rev-parse HEAD)            # the binary must be built from this SHA
    W=$(mktemp -d)
    go build -o "$W/codegraph" ./cmd/codegraph
    go run ./tools/benchharness run --fixture tools/benchharness/testdata/go-calls \
      --codegraph "$W/codegraph" --sha "$SHA" --date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --workdir "$W/work" --out record.json
    go run ./tools/benchharness validate --expect-sha "$SHA" --fixture tools/benchharness/testdata/go-calls record.json
    go run ./tools/benchharness summarize record.json > summary.md
    go run ./tools/benchharness compare old.json record.json   # refuses incompatible records

## Methodology (`fixture-callgraph-context/v1`)

- Fixture: `src/` (indexed from a fresh copy, fresh database) plus
  `truth.json`, a hand-labeled list of every eligible (caller, callee) pair and
  a fixed task set with expected facts. Fixture identity is the SHA-256 over
  sorted paths and contents of the fixture directory.
- Correctness: `codegraph find_callees` per truth caller; pairs are counted as
  resolved to the truth target or not resolved; resolved edges outside the
  truth universe are counted separately: edges to a labeled `targets` name that
  are not a labeled pair are `resolved_outside_truth` (in the name-matched
  precision denominator); edges to unlabeled callees are
  `resolved_outside_universe` and excluded. `coverage` is emitted only when the
  truth enumerates the whole universe. No accuracy, recall or F1 is emitted.
- Context: for each task, the exact bytes of (a) a fixed read-all-sources
  baseline and (b) the codegraph CLI query output. Estimated tokens are
  `ceil(bytes/4)` (`internal/tokenest`), not provider tokens. Evidence
  completeness is presence of each expected fact delimited by
  non-identifier characters (facts should be plain identifiers). Answer quality
  is not measured (no model is run) and is serialized as `null`.
- Fewer bytes is never reported as better on its own.

`validate` rejects records missing source SHA, fixture identity, methodology,
ground truth, limitations, observations, or ratio numerators/denominators, and
units outside count/bytes/estimated_tokens/ratio, any accuracy/F1 claim, and
recall/coverage/unresolved_share values without complete ground truth. `--expect-sha`
and `--fixture` reject stale records.

## Agent transcripts (`agent-transcript-ab/v2`)

`ingest` reads Claude Code JSONL transcripts that an operator already recorded
for an A/B campaign (one arm without CodeGraph, one or more arms with it) and
writes one deterministic `codegraph.agent_transcript_ingest/v2` record. It runs
no model and makes no network calls. No paid runs were made for this tool; the
bundled transcripts under `testdata/transcripts/` are hand-written synthetic
fixtures, and nothing produced by `ingest` is a published claim.

    go run ./tools/benchharness ingest --manifest runs/manifest.json \
      --date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --out ingest.json
    go run ./tools/benchharness validate ingest.json
    go run ./tools/benchharness summarize ingest.json > summary.md
    go run ./tools/benchharness summarize --include-contaminated ingest.json

The manifest (`codegraph.agent_run_manifest/v1`, see
`testdata/transcripts/manifest.json`) records the client name and version,
the full model ID, effort, the CodeGraph commit and binary SHA-256, the corpus
repository and commit, the task-set SHA-256 and optionally the task set's
relative path (`task_set`), per-arm tool mode, MCP config and
settings SHA-256, isolation flags, free-text uncertainties, and per session the
arm, task, repetition, relative transcript path and transcript SHA-256.
`ingest` refuses a transcript whose SHA-256, main-session model or client
version differs from the manifest, a task set whose SHA-256 differs from
`task_set_sha256`, and a session whose task is not in the task set. Config hashes and isolation flags are
recorded, not verified; every isolation flag that is false becomes a listed
limitation.

### Metric definitions

- Requests are deduplicated by `message.id`: Claude Code writes one assistant
  event per content block, each repeating the request's usage. Input and cache
  fields that differ for one id are an error. `output_tokens` is a streamed
  snapshot, so the largest value per id is kept; it may still undercount and is
  reported separately from processed tokens. `<synthetic>` messages are local
  and not requests.
- `processed_tokens`: sum over unique main-session requests of
  `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. This
  is throughput (what the model read across the session), not context size.
- `final_context_tokens` (residual context occupancy): the same sum for the
  last request only, the context the session ended with. `peak_context_tokens`
  is the largest per-request context. These are never added to or compared
  with `processed_tokens`.
- `compactions`: main-session `compact_boundary` system events.
- `wall_clock_ms`: last minus first main-session event timestamp (null with
  fewer than two). `tool_calls`: main-session `tool_use` blocks by tool name.
- Sidechain (subagent) events inside the transcript are reported under
  `sidechain` and excluded from every main-session total. Subagent transcripts
  stored in separate files are not ingested.
- Per arm, `summarize` shows the nearest-rank median (an observed session
  value), min and max of each metric over included sessions. Provider token
  counts are labelled `provider_tokens`, not estimates.

### Contamination

For an arm with `codegraph_enabled: false`, every `tool_use` (main session and
in-file sidechain) is checked for:

- an MCP tool whose server name contains `codegraph`;
- a `Skill` call naming a codegraph skill;
- a Bash command that runs the `codegraph` binary as a command. The command
  line is tokenized like a shell (quotes, escapes, `;`, `&&`, `|`, `(`, `{`,
  `!`, newlines, `$(...)` and backticks, including inside double quotes,
  and `bash -c`/`sh -c` scripts), then each simple command's command word is
  taken after `VAR=value` prefixes, `if`/`then`/`do`/`while`/... keywords and
  wrappers with their options (`env`, `sudo`, `timeout`, `nice`, `time`,
  `xargs`, `stdbuf`, `nohup`, `exec`, `command`). It matches `codegraph` bare,
  as the last path segment or with `.exe`, `npx`/`bunx` of a `codegraph`
  package, and `go run .../cmd/codegraph[@version]`. Arguments
  (`cd /src/codegraph`, `git -C /src/codegraph`, `ls .codegraph`,
  `grep codegraph`, `command -v codegraph`, and any `command` option cluster
  containing `v` or `V` such as `-pv`) do not match. Not detected:
  aliases, shell functions, `eval`, scripts, variables used as the command,
  `find -exec codegraph`, `watch codegraph`, a redirection glued to the name
  (`codegraph>out`, `>out codegraph`), `env -S 'codegraph ...'` and
  `go run ./cmd/codegraph/`. Heredoc bodies and trailing `# comments` are read
  as commands, so they can give false positives;
- a `Task`/`Agent` call: the subagent's own transcript is a separate file that
  is not ingested, so its CodeGraph use cannot be ruled out.

Each attempt gets an outcome from its `tool_result`: `blocked` only for exact
denial shapes (a result starting with `PreToolUse:<tool> hook error` or
`Hook PreToolUse:<tool> denied`, `Permission to use `, `The user doesn't want
to proceed`, or a result whose every non-empty line is either `Exit code N` or
a shell reporting the codegraph binary not found, such as
`bash: line 1: codegraph: command not found` or `sh: 1: codegraph: not found`;
the name must be `codegraph` itself, bare or as the last path segment, so
`./notcodegraph: command not found` is not a block);
otherwise
`succeeded`, `error` (codegraph may have run and printed output), `no_result`,
or `unverified` for subagent calls. A session with any attempt that is not
`blocked` is contaminated and excluded from per-arm statistics unless
`--include-contaminated` is given; `summarize` prints contamination before any
statistic. A baseline arm that needs subagents therefore needs its subagents
disabled or its results read with `--include-contaminated` and that caveat.

Output alone cannot tell a blocked call from a codegraph run that printed
nothing: an empty successful result is `succeeded` and an empty error result is
`error`, so both contaminate. Multi-block tool results are joined with line
breaks before these checks.

`validate --expect-sha` compares the manifest's CodeGraph commit; `--fixture`
and `compare` do not apply to ingest records and are refused.

Detection is a check, not the control. Before recording, the operator should,
for the baseline arm: use a strict per-arm MCP config with no CodeGraph server;
remove the `codegraph` binary from `PATH`; install a PreToolUse hook that
denies Bash commands running `codegraph` (and verify it once); isolate
user-level instructions, skills and plugins that mention CodeGraph (user
`CLAUDE.md`, skills directories), or record in the manifest that they were not
isolated; keep model, effort, prompts and client version identical across
arms; randomize session order from a published seed and run several
repetitions per task and arm.

### Residual context attribution (`context-delta-char-share/v1`)

Each session's `residual` splits `final_context_tokens` into `codegraph`
(CodeGraph MCP tools and Bash commands that run codegraph, in any arm),
`file_read` (Read), `search` (Grep, Glob, LS), `shell` (other Bash),
`other_tool`, `assistant` (text, thinking and tool-call blocks), `user_text`,
`initial_request` and `unattributed`. Every category carries a `basis`:

- `measured`: `initial_request` is the first main-session request's
  `input + cache_read + cache_creation`, the exact provider count of the system
  prompt, tool schemas and first prompt. The final context and all totals are
  measured too.
- `estimated`: for each later main-session request, the growth over the
  previous request is split over the content written between the two (the
  previous answer's blocks, tool results, user text) in proportion to its
  character count, with the largest-remainder method so shares sum to the
  measured growth exactly. These are apportioned provider tokens, not token
  counts of the content.
- `unavailable`: `unattributed` holds growth over a gap with no visible
  content, and, after a `compact_boundary` or any drop in context, the whole
  context at that request; attribution restarts from there (`resets`).

Categories always sum to the measured final context. Sidechain requests and
their tool results are excluded; subagent context is not attributed.
Error sources: content the transcript does not show (redacted thinking,
attachments, system reminders) has its tokens spread over the visible content
of the same gap; tokenizer density differs between source code, JSON and prose,
so one character does not cost the same everywhere; cache accounting noise
moves into the next gap. `summarize` prints a per-session table and per-arm
median/min/max with the basis in each unit (`provider_tokens`,
`estimated_provider_tokens`, `unattributed_provider_tokens`).

Compared with the external residual-occupancy method this adapts (measured
context deltas priced by characters), this one does not calibrate a
chars-per-token ratio on tool-dominated gaps or print a per-result error bar,
and it does not model partial eviction: any context drop resets attribution
instead of evicting the oldest results. It therefore reports more as
`unattributed` and never invents a resident set it cannot see.

### Answer correctness (`codegraph.agent_task_set/v1`)

When the manifest names a task set (see `testdata/transcripts/tasks.json`),
each session's final main-session answer is scored against its task's
`required` and `forbidden` facts. A fact is `substring`, `regex` (RE2) or
`number` (a regex with one capture group whose text, commas removed, equals
`value`); `ignore_case` is optional. The answer is the text blocks of the last
main-session request; a session whose last request called a tool, that was
followed by a user message, or that has no text is `unscored` with that reason.
Records store per-fact results, counts and the answer's SHA-256, not its text.
Per arm, `correctness` sums scored and unscored sessions, required facts
matched over total, forbidden hits, and sessions with every required fact and
no forbidden one, over included (uncontaminated) sessions. This is rubric
matching, not a semantic judgment: an answer can match every fact and still be
wrong, or be right in words the rubric does not anticipate. No model judges
answers. Subagent answers are not scored. Task-set tasks with no session are
listed as limitations.

### Schema compatibility

`codegraph.agent_transcript_ingest/v2` (methodology `agent-transcript-ab/v2`)
adds `task_set`, per-session `residual` and `evaluation`, per-arm
`correctness`, and `residual_*` per-arm statistics, and changes contamination
outcomes for the not-found and `command -pv` fixes; the version is bumped so a
v1 reader never sees fields or outcomes it does not know. `validate` and
`summarize` still accept `/v1` records under v1 rules (no residual,
evaluation or task set). `codegraph.agent_run_manifest/v1` gains only the
optional `task_set` field; existing manifests are unchanged.
`codegraph.benchmark/v1` is unchanged.
