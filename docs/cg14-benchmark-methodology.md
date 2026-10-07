# CG-14 benchmark methodology authority

This document defines the minimum evidence needed to compare CodeGraph runs.
It is methodology, not a performance target or release authorization.

## Run identity

`internal/benchmark.RunIdentity` is serialized as
`codegraph.benchmark_identity/v1`. A valid identity records the exact CodeGraph
commit and dirty state, fixture ID and content SHA, canonical root marker, OS,
architecture, Go version, CGO state, environment fingerprint, lifecycle mode,
and configuration fingerprint. Incremental runs also identify the prior state
and change set; no-op runs identify their prior state. Missing fields and
unknown schema versions fail closed. Two clean runs compare only when all
identity fields match; differences are returned as stable refusal reasons.
Dirty runs are refused because the base commit SHA does not identify arbitrary
working-tree edits. Never substitute a branch name, timestamp, machine-local
absolute path, or implicit default for an identity field. Dirty and CGO are
represented as optional booleans so absence is distinct from false.

The fixture SHA covers the versioned fixture definition and its canonical
inputs. The root marker identifies the logical path convention used by the
fixture. The configuration fingerprint is a deterministic digest of the
effective benchmark configuration, excluding secrets and machine-local paths.
The environment fingerprint is a deterministic digest of relevant runtime and
toolchain inputs beyond the explicit OS, architecture, Go version, and CGO
fields. It must not include hostnames, absolute paths, arbitrary environment
values, credentials, timestamps, or other volatile machine identity; its
versioned input list must be recorded alongside any benchmark artifact.

## Run lifecycle and result rules

Every artifact identifies one lifecycle mode: `fresh`, `incremental`, or
`no_op`. Fresh runs begin from an empty index. Incremental runs name the exact
prior state and declared change. No-op runs prove that the declared input is
unchanged. Results from different modes are not interchangeable. Refuse stale
results whenever the CodeGraph SHA, fixture identity/SHA, root marker,
configuration, or required environment identity differs. Dirty results remain
incomparable in v1. A future schema may permit comparison only after defining
and validating a deterministic working-tree diff fingerprint.

Timing records preserve raw samples, warmup count, measurement count, units,
and aggregation policy. Build/index setup time is separate from query latency.
Cold and warm measurements are distinct populations. Public comparisons use
at least ten measured samples per scenario and may be summarized in
benchstat-compatible form; raw samples remain authoritative.

Denominators are named and defined alongside each metric. Missing or zero
denominators produce an absent metric and an explicit reason, never a fabricated
zero or percentage. Byte-derived token estimates, provider-reported token
counts, and answer-quality results are separate fields and separate claims.
Quality results require a frozen task set, expected facts/rubric, and explicit
correct/partial/incorrect/failed counts; they do not imply timing or token
comparability.

## Artifact expectations

Artifacts are versioned JSON with a required schema identifier. Unknown schema
versions, malformed identities, missing denominators, and mismatched run
identity must be refused rather than silently compared. Store raw timing
samples and the exact fixture/configuration identity. Markdown or other human
summaries are derived from the artifact, never maintained as independent
measurements. Provider tokens and quality results are optional and remain
separate from local latency and estimated token counts.

This authority creates no release floors and does not authorize public
benchmark claims. CG-22/23 own the concrete benchmark artifact and execution
policy.
