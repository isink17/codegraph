# Kotlin Gradle evidence artifact

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
`codegraph.gradle_kotlin_evidence/v2`. It identifies producer name/version,
opaque repository identity and canonical root marker, repository-relative build
root, selected Gradle project and Kotlin compilation, and observable Gradle,
Kotlin plugin/compiler, Gradle runtime JDK, and selected Kotlin toolchain
version configuration. Tool values may be absent only when the corresponding
identity dimension is incomplete or unknown. Runtime and selected toolchain
configuration remain separate; neither version string identifies JDK contents.

The artifact carries one SHA-256 input fingerprint. The producer computes it
deterministically from all relevant inputs used for this selected compilation:
settings/build scripts and wrapper configuration; selected Gradle model and
source-set identity; source contents; generated/excluded roots and their
selection inputs; dependency graph and available metadata; and compiler/tool
identity. Unknown inputs and their states are part of the fingerprint. A
timestamp is never a freshness key. An importer must compare against a
fingerprint recomputed for current inputs and refuse a mismatch as stale.

The six v1 evidence dimensions are independent: `source`, `generated`, `excluded`,
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
Import validation is read-only and does not write scope evidence to SQLite.
The classifier remains disabled.

## Current producer

`codegraph export-gradle-evidence` explicitly runs the executable supplied with
`--gradle` for one selected Kotlin/JVM project and compilation. It resolves that
compilation's Gradle classpath and records positive module coordinates plus the
Kotlin source-set roots. It never discovers or launches a project wrapper
implicitly. The integration fixture pins Kotlin Gradle Plugin 2.3.20 and asserts
Gradle 9.3.1.

The v2 producer records the selected Kotlin compile task path and configured
compiler version, then makes the evidence task depend on that selected compile
task. It disables Gradle build-cache reuse, observes the selected task through a
Gradle task listener, hashes bounded declared task inputs and declared `.class`
outputs both immediately before and after the selected task, and records whether
the input hashes stayed stable. It records repository-relative output roots and
class hashes, and does not clean or overwrite existing build outputs. The
producer records whether the declared output roots were empty before the task;
nonempty roots remain untouched and keep output trust incomplete. The pinned
fixture verifies
Gradle 9.3.1, `:app:compileKotlin`, and Kotlin 2.3.20. `TASK_REPORTED_WORK`
means only Gradle reported work for that task. `UP_TO_DATE`, `FROM_CACHE`,
`NO_SOURCE`, skipped, and no-work outcomes retain their observations without
promoting execution or output trust. Raw skip text and machine paths are not
serialized.

Gradle's project/task history is kept in a per-repository CodeGraph directory
under the user's cache directory so repeat explicit exports can report
`UP_TO_DATE`; this is distinct from Gradle build-cache reuse, which remains
disabled. The cache contains Gradle's local project state, not evidence or a
compiler cache attestation.

The producer deliberately reports source and dependency dimensions as
`incomplete`; generated/excluded roots and external metadata as `unknown`; and
compiler identity as `incomplete`. The v2 tool fields explicitly call out the
Gradle runtime JDK. Selected Kotlin toolchain and compiler runtime remain
unknown. Compiler artifact candidates are task-input file names and byte hashes;
they do not prove which artifacts were loaded. The v2 proof dimensions for task
execution, ABI input closure, and output provenance remain `incomplete`, even
when the task reported work and output hashes exist: clean output state,
generated/excluded inputs, full compiler/plugin/options/toolchain closure, and
external dependency freshness are not established. `CurrentFingerprint`
covers the local repository file tree and reported source roots; v2 also checks
each listed class output against its recorded hash. It cannot
revalidate external dependency artifacts or environment-driven Gradle model
changes. The artifact is not imported into Store, and the classifier remains
disabled.

The decoder continues to accept `codegraph.gradle_kotlin_evidence/v1` as its
original diagnostic format. V1 artifacts cannot contain the v2 proof section,
and v1 evidence is never promoted based on fields added in v2. Re-export is
required to obtain v2 observations; even v2 does not authorize resolver
bindings while its required trust dimensions are incomplete or unknown.

The artifact contains no arbitrary environment variables, credentials, build
logs, or absolute machine paths. Source roots are canonical repository-relative
paths, and escaping symlinks fail closed. The output must be outside the
repository and is never overwritten. Ordinary index/update/watch/serve/MCP
paths do not call the producer.
Directory symlinks in the scanned repository currently refuse fingerprinting.
The output parent must already exist; its canonical path is checked before
Gradle starts, including symlinked parents.

`internal/buildevidence.Decode` validates bounded JSON and normalizes omitted
dimensions to unknown. `Artifact.ValidateCurrent` refuses a changed fingerprint;
`CurrentFingerprint` recomputes the local file inputs above. These checks do not
attest omitted Gradle inputs or promote incomplete evidence to complete.
Local fingerprint records use length framing and per-file content hashes. Artifacts
created with the earlier delimiter-only fingerprint must be explicitly re-exported;
their old fingerprints fail the current freshness check.

## Stored JVM type evidence

Independently of this artifact, the index stores JVM source facts (migrations
006 and 007): Java and Kotlin type declarations, callable parameter and result
positions, modifiers, generic and value-class syntax, and Kotlin typealias
target spellings, plus six per-repository compilation-scope dimensions with
the same names and states as above. Every dimension starts and stays `unknown`,
because nothing imports build evidence. Type-dependency observations are
recorded only when scope is complete, so in practice none are.

These facts describe source syntax, not canonical JVM identity. No resolver
consumes them to bind a call. Kotlin calls whose JVM name or descriptor depends
on the compilation (value and inline classes, typealiases from dependencies,
mangled names) stay unresolved: a repository's source alone cannot prove which
JVM method such a call reaches.
