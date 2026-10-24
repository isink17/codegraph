# Benchmark bundles

How a CodeGraph benchmark result is produced, published and re-run by somebody
who does not trust the producer. The record format and run identity rules are in
[../benchmark-methodology.md](../benchmark-methodology.md). The harness commands
are documented in [tools/benchharness/README.md](../../tools/benchharness/README.md).
`v2.0.0-baseline/` holds earlier raw measurements in the older TSV/JSON layout.
They are not bundles and cannot be checked with `verify-bundle`.

## Produce

Build `codegraph` from the commit being measured, then write a bundle from the
committed fixture manifest:

    go build -trimpath -o "$W/codegraph" ./cmd/codegraph
    go run ./tools/benchharness produce \
      --manifest tools/benchharness/testdata/manifests/fixtures.json \
      --codegraph "$W/codegraph" --sha "$SHA" \
      --date 2026-10-23T00:00:00Z --out "$W/bundle"

The bundle holds:

- one `codegraph.benchmark/v1` record per fixture
- `summary.md`
- a copy of the fixture manifest
- `MANIFEST.json`, which records the commit, binary SHA-256, Go version, OS/arch, fixture hashes and the exact commands
- `SHA256SUMS`

Every record passes `validate --expect-sha` before any file is written.

## Publish

Publish the whole bundle directory unchanged, together with the CodeGraph
commit, the harness revision and the exact build command. A table or claim
derived from a bundle must:

- name the bundle and the record it comes from
- label byte-derived token estimates as estimates and keep them apart from
  provider-reported token counts (see the methodology)
- state every denominator (for example "7 of 7 truth targets in the go-calls
  fixture"), and never give a rate without its counts
- keep negative results and refusals, such as unresolved calls, skipped
  scenarios and comparisons the harness refused, beside the positive results
- describe only the fixtures in the bundle: a synthetic fixture says nothing
  about real repositories
- describe the binary that was measured, not a later one

## Re-run

On a separate machine:

1. Check out the recorded commit and the producer's harness revision.
2. Use the Go version and OS/arch recorded in `MANIFEST.json`.
3. Build `codegraph` with the same build command.
4. Run:

       go run ./tools/benchharness verify-bundle \
         --bundle "$W/bundle" \
         --manifest tools/benchharness/testdata/manifests/fixtures.json \
         --codegraph "$W/codegraph"

`verify-bundle` checks `SHA256SUMS`. It refuses missing, unlisted or modified
files. It refuses a binary or fixture manifest whose SHA-256 differs from
`MANIFEST.json`. It then runs `produce` again with the recorded commit and date
in a scratch directory and compares every file byte for byte. Any difference is
printed per file and line, and the command exits non-zero. On success it prints
how many files matched.

A different Go version, OS or architecture changes `MANIFEST.json`, and may
change the binary hash, so on a different platform `verify-bundle` reports a
mismatch rather than a pass. That is the intended result: the bundle describes
one environment.

## What a re-run proves

- Running `verify-bundle` on the machine that produced the bundle checks the
  tooling and the bundle's internal consistency only. It is **not** the
  independent third-party rerun that the publication criterion requires.
- A third-party rerun counts only when somebody other than the producer
  rebuilds the binary from the recorded commit on their own machine and
  `verify-bundle` passes. Record who ran it, where, and with which toolchain.
- Checksums show consistency, not authenticity: whoever controls the bundle can
  regenerate them. Trust comes from the rerun, not from the checksums.
- The commit in `MANIFEST.json` is supplied by the operator. Only the binary
  hash ties the bundle to an executable.

## Known limitations

- The committed manifest pins only the synthetic `go-calls` fixture. No
  real-repository corpus, truth set or third-party code is bundled.
- No bundle has been published, and this document makes no performance or
  quality claim.
- `verify-bundle` reruns the fixtures. It cannot detect a record that is
  wrong in the same way on every run, for example a truth file that is itself
  wrong.
