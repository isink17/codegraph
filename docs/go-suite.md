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

Focused `internal/diff` tests ran in another worker slot during part of
that run, so its timings are slightly loaded. 20m leaves more than 6× headroom over the slowest package for a loaded or
slower machine.

Do not run two full suites at the same time on one machine, for example two
worker slots or a suite beside the local CI reproduction. The packages then
compete for the same cores, and timings and timeouts stop being comparable.

Results are local to the machine and toolchain that ran them. They are not
GitHub Actions results, and a macOS run says nothing about Linux or Windows.
