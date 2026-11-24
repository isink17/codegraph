# Bounded full Go-suite run

`scripts/go-suite/run.sh` runs `go test ./...` once with a per-package
timeout, prints one line per package as it finishes, and writes a
machine-readable summary. Use it whenever "the full suite passed" has to be
stated locally.

    scripts/go-suite/run.sh                 # ./...
    scripts/go-suite/run.sh ./internal/...  # a subset
    OUT=evidence/go-suite TIMEOUT=20m scripts/go-suite/run.sh

It runs `go test -json -count=1 -timeout $TIMEOUT` with no `-short`, no skip
list and no `-run` filter. `OUT/events.jsonl` is the raw event stream, and
`OUT/summary.json` holds, per package:

- `result`: one of
  - `pass`
  - `no_tests`
  - `fail`
  - `timeout`: the test binary panicked at `-timeout`
  - `build_failed`
  - `incomplete`: started but never finished, for example because the run was killed
- `elapsed`
- test counts, including subtests

A line of the event stream that is not JSON, for example the half line of a
killed run, is counted in `malformed_lines` and fails the run.

The summary also records the `go test` exit code, wall time, the timeout and
`all_passed`. The script exits 0 only when `go test` exits 0 and every package
is `pass` or `no_tests`. A timeout is always a failure, never a pass, and is
named as a timeout. `scripts/go-suite/run_test.sh` checks this classification
against a recorded `go test -json` stream.

## Timeout

The default is `TIMEOUT=20m` per test binary, the same failure ceiling the
macOS and Linux legs of `.github/workflows/ci.yml` use, and that the local CI
reproduction uses. Go's `-timeout` is a ceiling, not a target: a passing
package does not wait for it, and a hang still fails at it.

Why not lower: an earlier campaign ran with `-timeout 90s`, and it failed
only because `internal/store` exceeded that ceiling. That was a wrong bound,
not a product failure. The reference run of this script on Darwin arm64, Go
1.26.1, CGO, commit 605daac, took 199 s wall time with every package passing:

| Package | Elapsed | Tests passed |
|---|---|---|
| internal/indexer | 191.5 s | 1,593 |
| internal/store | 182.0 s | 3,192 |
| internal/diff | 135.5 s | 419 |
| all 53 packages | 199 s wall | 7,218 |

Focused `internal/diff` tests ran in another checkout during part of
that run, so its timings are slightly loaded. 20m leaves more than 6×
headroom over the slowest package for a loaded or slower machine.

Do not run two full suites at the same time on one machine, for example in two
checkouts or a suite beside the local CI reproduction. The packages then
compete for the same cores, and timings and timeouts stop being comparable.

Results are local to the machine and toolchain that ran them. They are not
GitHub Actions results, and a macOS run says nothing about Linux or Windows.

## Faster local feedback: the opt-in sharded runner

`internal/store`, `internal/indexer` and `internal/diff` run their tests one
after another and take most of the suite's wall time while other cores sit
idle. `tools/shardsuite` splits those packages by top-level test across
separate `go test` processes and runs every other package in one ordinary
`go test`, all at the same time:

    go run ./tools/shardsuite                              # ./..., default shards
    go run ./tools/shardsuite -timings OUT_OF_EARLIER_RUN  # balance shards by test time
    go run ./tools/shardsuite -baseline RUN_SH_OUT         # also compare test identities
    go run ./tools/shardsuite -shard ./internal/store=4,./internal/indexer=4 ./internal/...
    go run ./tools/shardsuite parity RUN_SH_OUT SHARDED_OUT

It is opt-in developer feedback. **`scripts/go-suite/run.sh` stays the only
evidence that "the full suite passed"**, for release checks and any statement
that has to hold. Sharding changes how tests are grouped into processes:
each shard runs its package's `TestMain` and package-level setup again, and
tests that would have run in one process no longer share it. Tests of one
package that used to run one after another now run at the same time in
separate processes, so anything shared outside the private temp directory
(a fixed port, a file in the repository, a path under the home directory)
can collide and fail. `TestMain` and `init` in a listed package must not
print (stdout or stderr), because the list is parsed from that output; a list with
unexpected lines is refused. That is a
different execution than CI and `run.sh`, so a sharded pass does not replace
them.

How it works:

- `go test -json -list .` over the requested packages gives the expected
  top-level tests, examples and fuzz targets per package. It runs with the same
  environment as the shards, so `GOFLAGS` (for example `-tags=...`) and
  `CGO_ENABLED` decide which files and tests exist, exactly as they do for
  the run itself.
- A sharded package is split into `K` disjoint sets, each run with
  `-run '^(TestA|TestB|...)$'`. Names are regex-quoted and anchored, so
  `TestA` never also selects `TestAB`. Subtests run inside their parent.
  With `-timings` (an `events.jsonl`, a `run.sh` OUT directory or an earlier
  shardsuite OUT directory) sets are balanced by each test's earlier elapsed
  time, longest first; without it, sorted names are dealt round-robin. Both
  are deterministic. A package that fails to list, or has fewer than two
  tests, runs unsharded. The listed packages must match `go list` for the
  same patterns, and a list that was cut short is an error, so a package can
  never drop out of the run unnoticed.
- `-out` must be outside the module (the default is a new temp directory);
  leftover `shards/*/tmp` trees inside it would be picked up by `./...`, so
  the runner refuses such a path. It must also be new or empty, so an earlier
  run's `summary.json`, `plan.json` and shards can never be read as this
  run's.
- Every process gets its own directory `OUT/shards/<job>/` with
  `events.jsonl`, `stderr.log`, `result.json` and a private `tmp/` that
  `TMPDIR`, `TMP`, `TEMP` and `GOTMPDIR` point at.
- `-timeout` (default 20m) is passed to every test binary, as in `run.sh`.
  `-job-timeout` (default `-timeout` + 5m) kills a whole process tree that
  is still running, for example a hung build.

Each process is classified as `pass`, `no_tests`, `test_fail`, `build_fail`,
`timeout` (a test binary hit `-timeout`, or the job was killed at
`-job-timeout`), `incomplete` (interrupted, a package that started but never
finished, a half-written event line, or a listed test that never reported) or
`inventory_mismatch` (a top-level test ran twice, or ran without being
listed). Skipped tests are counted per package. The run passes only when
every process is `pass` or `no_tests`; otherwise it exits 1 and prints each
failing process and package. `OUT/summary.json` records every process, the
`GOFLAGS` and `CGO_ENABLED` in effect, and the wall time.

On Ctrl-C, SIGTERM or SIGHUP it kills every `go test` process group
(a test that puts its own children in a new process group is outside it), writes the summary
with `interrupted: true`, prints `INCOMPLETE` and exits 130. A run that was
interrupted is never reported as passed, even when every finished shard
passed. A runner killed with SIGKILL cannot do this, and its `go test`
processes keep running until their own `-timeout`.

Pass counts matching is not proof of equivalence. `-baseline` (or the
`parity` subcommand) compares the multiset of package + test identities,
subtests included, with an unsharded `run.sh` run: every identity must be
reported exactly once with the same action. A `t.TempDir` path embedded in a
subtest name is normalized, because each process has a different temp root.

Hardware: sharding helps when cores are idle. With the default
`store=3, indexer=3, diff=2` it runs nine processes at once. Measured on an
Apple M3 Pro (11 cores, 18 GiB), Go 1.26.1, CGO, four interleaved pairs
(`run.sh`, then shardsuite with `-timings` from the first `run.sh` run,
warm build cache) on a shared machine. The pairs used the runner as first
added; one further pair on the runner as merged here gave 197 s against
112 s with identical identities (7,882; the later base commit and this
runner's own tests add tests). Without `-timings` (round-robin)
the speedup was not measured. Peak RSS is the largest of
the four runs, sampled once a second as the summed RSS of the whole process
tree:

| | wall (mean ± stdev) | CPU user+sys | peak RSS (max of 4, sampled) |
|---|---|---|---|
| `run.sh` | 204 ± 15 s (192-222) | 601 s | 2.1 GiB |
| `shardsuite -timings` | 113 ± 6 s (107-120) | 833 s | 3.8 GiB |

That is 1.75-1.85× faster per pair for about 40 % more CPU and almost twice
the memory, with the same 7,778 test identities in every pair. Other
processes, including other test runs, were present at the start of most
runs; pairs 3 and 4 were 192 s against 107-110 s (1.75× and 1.79×). These are
one machine and four pairs. As an estimate, not measured elsewhere: plan for
at least 8 cores and about 4 GiB of free memory, and on 4 cores or fewer
expect little gain, since the suite's
total CPU work then exceeds what the cores can run in
parallel. Do not run it beside another full suite on the same machine.

`CGO_ENABLED=0`: the list and the run both honor it, so the tree-sitter
adapters that need cgo are compiled out of both and the inventory matches
what that build can run. Compare a no-CGO sharded run only with a no-CGO
`run.sh` baseline.

Platforms: written in Go with the standard library only. On macOS and Linux
each `go test` runs in its own process group, so a kill reaches the test
binary too. On Windows it uses `taskkill /T /F`; a shard's `-run` pattern
must also fit the 32,767-character command line, so the runner refuses a
shard whose pattern exceeds 30,000 bytes and asks for a larger `K`. The
runner was not executed on Windows; Windows support is cross-compiled and
reasoned, not run.
