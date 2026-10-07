# CG-35 Kotlin Gradle evidence artifact

This contract defines an explicitly produced artifact for one selected Kotlin
Gradle compilation. It is not a Gradle invocation path for ordinary indexing.

## Invocation and scope

The producer runs only after an explicit user action naming one repository,
build root, Gradle project path, and Kotlin compilation. It may inspect Gradle's
resolved model for that compilation and execute the user's requested Gradle
tasks. That execution boundary must be visible in the command and output. The
artifact importer performs no process execution. `index`, `update`, `watch`,
`serve` startup/autosync, and MCP indexing never invoke the producer or a build.

This contract covers Kotlin Gradle only. It does not define Maven support,
whole-repository completeness, or automatic compilation selection.

## Versioned artifact

`internal/buildevidence.Artifact` uses schema
`codegraph.gradle_kotlin_evidence/v1`. It identifies producer name/version,
opaque repository identity and canonical root marker, repository-relative build
root, selected Gradle project and Kotlin compilation, and observable Gradle,
Kotlin plugin/compiler, and JDK versions. Tool values may be absent only when
the compiler-identity dimension is incomplete or unknown.

The artifact carries one SHA-256 input fingerprint. The producer computes it
deterministically from all relevant inputs used for this selected compilation:
settings/build scripts and wrapper configuration; selected Gradle model and
source-set identity; source contents; generated/excluded roots and their
selection inputs; dependency graph and available metadata; and compiler/tool
identity. Unknown inputs and their states are part of the fingerprint. A
timestamp is never a freshness key. An importer must compare against a
fingerprint recomputed for current inputs and refuse a mismatch as stale.

The six evidence dimensions are independent: `source`, `generated`, `excluded`,
`dependency`, `external_metadata`, and `compiler_identity`. Each records
`complete`, `incomplete`, or `unknown` plus provenance. Producer execution does
not promote a dimension. Complete source/generated/excluded evidence means the
selected compilation's roots were enumerated, including a proven empty set.
Complete dimensions must carry their corresponding payload explicitly; an
empty set is encoded as `[]`, while missing or `null` payloads are refused.
Excluded roots are emitted only when the build model proves them. Dependency
observations use `positive`, `negative`, `unknown`, or `ambiguous`; an
incomplete or unknown dependency scope cannot support a negative fact. Missing dimensions normalize
to explicit `unknown` with `dimension omitted from artifact` provenance.

Roots use normalized repository-relative slash paths. Dependency and external
metadata facts include their evidence provenance and stable identity; external
metadata includes a content SHA-256. The validator rejects absolute or escaping
root paths, malformed fingerprints, unsupported states, missing identity, and
unknown JSON fields. Before marking roots complete, the producer resolves
symlinks and verifies the selected root remains under the repository; an
escaping or unprovable symlink makes that dimension incomplete or unknown, and
absolute machine paths are not persisted. JSON input is bounded to 16 MiB.

## Security and freshness

The artifact has no arbitrary environment-variable map, command-line capture,
credentials, or build logs. Tool identity is an allowlisted set of version
strings. Repository identity must be opaque and must not embed credentials.
Build root and source roots are relative to the repository identity. Exporters
must not serialize environment values merely because Gradle can expose them.
Free-form producer, identity, and provenance fields must use controlled
non-secret values; the schema has no field for credentials or environment
values.

Evidence is usable only when schema, repository/build/compilation identity, and
current input fingerprint match. Otherwise the artifact is rejected or all
affected dimensions remain unknown; timestamp-only acceptance is forbidden.
Import validation is read-only and does not write scope evidence to SQLite in
this first slice. The classifier remains disabled.

## Initial validator slice

`internal/buildevidence.Decode` validates bounded JSON and normalizes omitted
dimensions to unknown. `Artifact.ValidateCurrent` refuses a changed input
fingerprint. This boundary validates artifact claims; it does not attest that
the producer actually observed a complete build model. The future producer
must provide provenance that makes each completeness claim auditable.
