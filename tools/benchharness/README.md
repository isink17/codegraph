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
  truth universe are counted separately. `coverage` is emitted only when the
  truth enumerates the whole universe. No accuracy, recall or F1 is emitted.
- Context: for each task, the exact bytes of (a) a fixed read-all-sources
  baseline and (b) the codegraph CLI query output. Estimated tokens are
  `ceil(bytes/4)` (`internal/tokenest`), not provider tokens. Evidence
  completeness is whole-word presence of expected identifiers. Answer quality
  is not measured (no model is run) and is serialized as `null`.
- Fewer bytes is never reported as better on its own.

`validate` rejects records missing source SHA, fixture identity, methodology,
ground truth, limitations, observations, or ratio numerators/denominators, and
any accuracy/F1 claim or recall without complete ground truth. `--expect-sha`
and `--fixture` reject stale records.
