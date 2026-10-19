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

## Agent transcripts (`agent-transcript-ab/v3`)

`ingest` reads Claude Code JSONL transcripts that an operator already recorded
for an A/B campaign (one arm without CodeGraph, one or more arms with it) and
writes one deterministic `codegraph.agent_transcript_ingest/v3` record. It runs
no model and makes no network calls. No paid runs were made for this tool; the
bundled transcripts under `testdata/transcripts/`, `testdata/subagents/` and
`testdata/calibration/` (and the calibration artifact made from them) are
hand-written synthetic fixtures, and nothing produced by `ingest` is a published claim.

    go run ./tools/benchharness calibrate --out calibration.json runs/*.jsonl runs/*/subagents/agent-*.jsonl
    go run ./tools/benchharness ingest --manifest runs/manifest.json --calibration calibration.json \
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
arm, task, repetition, relative transcript path and transcript SHA-256, and
optionally `subagents`, a list of the session's subagent transcripts (relative
path and SHA-256; see `testdata/subagents/manifest.json`).
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
  `sidechain` and excluded from every main-session total.
- Subagent transcripts: Claude Code writes each subagent to
  `<session>/subagents/agent-<id>.jsonl` next to `<session>.jsonl`. `ingest`
  always reads every `agent-*.jsonl` file in that directory and records each
  with its SHA-256 under the session's `subagents`. When the manifest lists the
  session's `subagents` (an empty list included), a discovered file that is not
  listed, a listed file that is missing, or a hash mismatch is an error. Other
  files there (`.meta.json`, fork markers) are not read. A subagent file's
  events must carry its own `agentId` (or none) and the manifest's client
  version; its model may differ from the main session's.
- Linking: the `tool_result` of a `Task`/`Agent` call carries
  `toolUseResult.agentId`; that call is the subagent's parent (`main`, or the
  spawning subagent for nested agents, with `depth` counted from the main
  session). A subagent file no result names is listed with no parent (depth 0,
  for example teammates); a `Task`/`Agent` call whose result names no file is
  counted in `workload.unlinked_agent_calls`. A file named by two calls is an
  error.
- Cross-file deduplication: each `message.id` counts once across the session's
  files, owned by the main thread first, then subagents in spawn order, then
  unlinked subagents by id, then sidechain events inside the main file. A fork
  that copies its parent's history therefore does not count it again
  (`duplicate_requests`), and a sidechain request recorded both inside the main
  file and in its subagent file counts once, under the subagent. The same id
  with different input or cache usage in two files is an error. Copies with
  the same usage may still differ in content, so this applies to token
  accounting only: linking and contamination read every copy of every
  `tool_use` (see Contamination).
- Per subagent: the same metrics as a main session (requests, processed,
  final and peak context, tool calls, wall clock), from its own file only.
  `workload` reports main, subagent-file and in-file sidechain processed
  tokens and their total. Context sizes are never summed across agents; the
  main session's final and peak context stay main-only.
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
- a `Task`/`Agent` call that is not linked to a subagent file (see Linking):
  its CodeGraph use cannot be ruled out. A linked call is not an attempt; the
  subagent file's own tool calls are checked by the same rules instead, and
  attempts carry the `agent` that made them (`main`, `sidechain` or a
  subagent id). Nested calls are linked the same way, so a chain is checked
  only as far as its files are linked. Every copy of a `tool_use` is checked,
  including copies on requests another file owns; attempts are listed once
  per `tool_use` id (main, linked subagents in spawn order, unlinked
  subagents, then in-file sidechains), and an outcome that is not `blocked` in
  any copy wins. `agent-*.jsonl` files whose id has characters outside
  `[A-Za-z0-9_-]` are an error.

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
or `unverified` for unlinked subagent calls. A session with any attempt that is not
`blocked` is contaminated and excluded from per-arm statistics unless
`--include-contaminated` is given; `summarize` prints contamination before any
statistic. A baseline arm that needs subagents therefore needs its subagents
disabled, its subagent files recorded next to the transcript, or its results
read with `--include-contaminated` and that caveat.

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
their tool results are excluded; subagent context is not attributed. In `/v3`
records this method carries `uncalibrated: true`.
Error sources: content the transcript does not show (redacted thinking,
attachments, system reminders) has its tokens spread over the visible content
of the same gap; tokenizer density differs between source code, JSON and prose,
so one character does not cost the same everywhere; cache accounting noise
moves into the next gap. `summarize` prints a per-session table and per-arm
median/min/max with the basis in each unit (`provider_tokens`,
`estimated_provider_tokens`, `unattributed_provider_tokens`).

Compared with the external residual-occupancy method this adapts (measured
context deltas priced by characters; colbymchenry/codegraph @
0d33925aa3ac4d34ba9f425b24029332597f1941, a different project whose numbers
are not ours), this method does not calibrate; the calibrated method below
fits per-category ratios on dominated gaps and reports interquartile bounds
per category rather than a per-result error bar. Neither models partial
eviction: any context drop resets attribution
instead of evicting the oldest results. It therefore reports more as
`unattributed` and never invents a resident set it cannot see.

### Residual calibration (`context-delta-calibrated/v1`)

`calibrate --out calibration.json <transcript>...` fits characters per 1000
provider tokens (`codegraph.residual_calibration/v1`) from recorded usage. For
each main-thread request after the first (a file with no main-session request
is read as a subagent file), the measured growth over the previous request is
compared with the visible characters written in between. A gap is a sample
when growth is positive, no compaction or context drop happened, the gap has
at least 100 characters, and its `message.id` was not sampled from another
input (forks copy history). Every sample enters the global fit; it also enters
a category's fit when that category holds at least 90% of the gap's
characters. Fits are nearest-rank median, quartiles, minimum and maximum
(integers). A category with fewer than 5 samples is priced by the global fit;
with fewer than 5 global samples the artifact is `sufficient: false`. Inputs
are recorded by SHA-256 only, in hash order, so the artifact does not depend
on paths or argument order.

`ingest --calibration` embeds the artifact and its SHA-256 (the file must be
in the canonical encoding `calibrate` writes, so `validate` can tie the copy to
the hash). With a sufficient calibration, each gap's segments are priced at
their category's median ratio: priced tokens up to the measured growth go to
the categories and the rest of the growth to `unattributed`; a gap priced
above its growth is scaled down to it (largest remainder). Compactions and
drops reset as before; no eviction is inferred. Each estimated category also
reports `calibrated`: the median-ratio price of its characters since the last
reset (`estimate_tokens`) and the price at the upper and lower quartile ratio
(`low_tokens`, `high_tokens`), uncapped, so the reconciled `tokens` can fall
outside that range when hidden content or cache noise moved a gap; `validate`
recomputes them from the embedded calibration. The range is the spread of
observed ratios, not a confidence interval: mixed-content gaps, hidden content
and tokenizer differences all widen it. Without a calibration, or with an
insufficient one, ingest keeps `context-delta-char-share/v1` flagged
`uncalibrated` and lists why under limitations. Measured totals are never
replaced by estimates.

### Answer correctness (`codegraph.agent_task_set/v1`)

When the manifest names a task set (see `testdata/transcripts/tasks.json`),
each session's final main-session answer is scored against its task's
`required` and `forbidden` facts. A fact is `substring`, `regex` (RE2) or
`number` (a regex with one capture group whose text, commas removed, equals
`value`); `ignore_case` is optional. The answer is the text blocks of the last
main-session request (identical text or thinking blocks within one
`message.id` count once, so a block the model genuinely repeats is also
counted once); a session whose last request called a tool, that
was followed by a user message, or that has no text is `unscored` with that
reason. User events marked `isMeta` and local slash-command wrappers
(content made only of complete `<command-name>`, `<command-message>`,
`<command-args>`, `<local-command-stdout>`, `<local-command-stderr>` and
`<local-command-caveat>` elements, as written by `/exit` or `/model`) do not
count as a following user message; any other text, or an unclosed or
mismatched tag, does. Because `isMeta` events are ignored, an interactive
session interrupted after a custom slash command is scored on the previous
answer, and interactive sessions can end in other shapes this does not
recognize; headless (`claude -p`) recordings avoid both and are the reliable
path. Records store per-fact results, counts and the answer's
SHA-256, not its text, plus the task-set file itself as `task_set_json`, the
exact bytes ingest hashed; `validate` rechecks them against
`task_set_sha256` and checks every evaluation against the facts they define.
Per arm, `correctness` sums scored and unscored sessions, required facts
matched over total, forbidden hits, and sessions with every required fact and
no forbidden one, over included (uncontaminated) sessions. This is rubric
matching, not a semantic judgment: an answer can match every fact and still be
wrong, or be right in words the rubric does not anticipate. No model judges
answers. Subagent answers are not scored. Task-set tasks with no session are
listed as limitations.

### Schema compatibility

`codegraph.agent_transcript_ingest/v3` (methodology `agent-transcript-ab/v3`)
adds per-session `subagents` and `workload`, an `agent` on each contamination
call, the optional embedded `calibration`, the residual `uncalibrated` flag and
per-category `calibrated` estimates, and per-arm `subagent_processed_tokens`
and `total_processed_tokens`; linked Task/Agent calls are no longer
contamination attempts. `validate` and `summarize` still accept `/v2` and
`/v1` records under their own rules (`testdata/ingest-v2.json`,
`testdata/ingest-v1.json`). `codegraph.agent_run_manifest/v1` gains only the
optional per-session `subagents` list.

`codegraph.agent_transcript_ingest/v2` (methodology `agent-transcript-ab/v2`)
adds `task_set_json`, per-session `residual` and `evaluation`, per-arm
`correctness`, and `residual_*` per-arm statistics, and changes contamination
outcomes for the not-found and `command -pv` fixes; the version is bumped so a
v1 reader never sees fields or outcomes it does not know. `validate` and
`summarize` still accept `/v1` records under v1 rules (no residual,
evaluation or task set). `codegraph.agent_run_manifest/v1` gains only the
optional `task_set` field; existing manifests are unchanged.
`codegraph.benchmark/v1` is unchanged.
