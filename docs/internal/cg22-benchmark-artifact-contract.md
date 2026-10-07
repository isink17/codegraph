TEMPORARY PRE-RELEASE METHODOLOGY.
REMOVE OR CURATE BEFORE V2 RC.

# CG-22/23 benchmark artifact contract

This contract governs comparable benchmark evidence. It sets no release floor
and does not authorize public performance claims.

## Inputs and comparability

Each artifact records the exact CodeGraph commit, dirty state, fixture ID and
content SHA, canonical root marker, environment identity, effective
configuration fingerprint, and lifecycle mode (`fresh`, `incremental`, or
`no_op`). Incremental and no-op runs name the exact prior state; incremental
runs also name the change set. Required missing or unknown identity fields
make the artifact invalid. v1 refuses dirty runs, different identities,
stale fixture/configuration, and unknown schema versions with explicit reasons.
A timestamp or branch name is never identity.

Environment identity includes OS, architecture, Go version, CGO state, and a
stable fingerprint of relevant runtime/toolchain inputs. It excludes hostnames,
absolute machine-local paths, arbitrary environment-variable values, and
credentials. Configuration fingerprints cover effective benchmark settings,
not secrets or local path spellings.

## Measurements

Store raw timing samples per named scenario, in a declared unit, with warmup
count, sample count, and aggregation policy. Fresh-index setup, incremental
update, no-op update, and warm query timings are separate populations. The
existing `BenchmarkQueryLatency100k` fixture and `codegraph bench-queries`
measure different workloads and must have distinct fixture/scenario identities.

Every metric names its denominator and units. Missing or zero denominators
produce an absent metric and a refusal reason, never a fabricated zero or
percentage. Serialized bytes and deterministic byte-derived token estimates
are recorded separately from provider-reported tokens. Answer quality is a
separate result tied to a frozen task set and rubric; it cannot imply timing or
token comparability.

Public-mode summaries require at least ten measured samples per scenario and
retain raw samples; Go benchmark output remains consumable by benchstat. Any
Markdown report is generated from the validated artifact and is not a second
source of measurements.

## Artifact validation

Artifacts use versioned JSON and reject malformed fields, unknown versions,
missing denominators, invalid lifecycle combinations, stale identities, and
incomparable environment/configuration/fixture inputs. Comparability is an
explicit boolean plus stable refusal reasons. Serialization is deterministic
for the same artifact data. CG-14 must merge before this contract is
implemented; its methodology authority will define the reusable identity and
comparability rules, and this document specifies the CG-22/23 artifact fields.

## Current fixture notes

The synthetic ~100k-symbol query fixture is built through production store
write and resolver paths and uses a deterministic nonuniform graph. Its
canonical repository marker is part of fixture identity. `codegraph
bench-queries` instead reads an already-indexed repository and reports its
root and graph size; it needs a separate repository identity policy before
cross-machine comparison. Until a versioned artifact producer validates these
inputs, benchmark command output remains observational and is not a comparable
release claim.
