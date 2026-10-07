# CG-35: bounded Kotlin callable type identity

Design only; CG-35 remains In Progress. No general Kotlin type resolver or
compiler frontend is proposed for this iteration. The current Kotlin v11 ABI
refusals remain authoritative, including all 24 `Internal.parseCookie` calls.

## Evidence boundary

A spelling is not a type identity. Persist source location, compilation scope,
lexical scope, declaration identity and evidence completeness separately from
the callable's JVM name and arity. Duplicate, missing, malformed or conflicting
facts produce unknown, never a first match or name-based fallback.

| Identity question | Repository source facts | Compilation/classpath evidence |
| --- | --- | --- |
| Ordinary class | Qualified declaration, lexical owner, modifiers, generic parameters, source set, import path | Whether the indexed sources cover the actual compilation scope; dependency class identity and metadata |
| Kotlin builtin | Validated fully qualified spelling or canonical explicit import; higher-priority source shadows | Compiler/stdlib identity and complete higher-priority package/dependency shadow absence for default imports |
| Imported or shadowed name | Structured explicit/alias/wildcard imports, package, local declarations and type parameters, conflicts | Selected source sets, excluded/generated sources and dependencies that affect lookup precedence |
| Typealias | Alias declaration and target spelling, owner, scope, dependencies; unknown/cyclic targets refuse | Kotlin dependency metadata includes aliases absent from JVM class names; canonical alias expansion requires scope-complete evidence |
| Value/inline class | Explicit value/inline modifier, underlying type spelling, nullability and use position | Actual compiler ABI lowering, compiler version and metadata; nominal qualified identity alone does not prove representation |
| Unknown/ambiguous declaration | Parser errors, missing declarations, duplicate candidates, unsupported shape | Missing or conflicting scope/classpath facts retain unknown status |
| JVM callable identity | Owner/facade, validated annotations, visibility, staticness, receiver, parameter/result spellings and supported fixed shape | Actual JVM method name, descriptor, mangling and generated members; source name plus parameter count is insufficient |

The decisive preserved counterexample compiles identical consumer source with
three classpaths. Default `Long`/`String` spellings produce `call`,
`call-QfCEE6I` or `call-5w0zCZw`; all three descriptors are
`(JLjava/lang/String;)Ljava/lang/String;`. Dependency same-package aliases to
value classes change the method name without changing any indexed consumer
source. Repository absence is therefore not proof of canonical builtin identity.
A value-class result can also change the descriptor without changing the name.

## Minimum infrastructure for a future fixed-arity subset

1. Establish an explicit, versioned compilation-scope completeness boundary:
   selected sources, generated/excluded sources, dependency metadata and compiler
   identity. Alternatively consume independently verified compiler identity facts.
   Unknown completeness refuses default-import identity claims.
2. Persist structured imports, nominal declaration kind, value/inline and generic
   blockers, alias hazards, lexical shadows, and parameter **and result** type
   syntax. Preserve provenance and unknown states. Existing symbol kind `class`
   is insufficient to distinguish ordinary and value/inline declarations.
3. Use a bounded nonrecursive classifier for top-level, nongeneric, fixed-shape
   callables only: validated unique explicit identities, ordinary class facts,
   supported nullability and proven JVM name/owner/shape. Unresolved aliases,
   wildcard hazards, generics, nested or extension shapes, ambiguity and unknown
   annotations/mangling refuse extraction. Do not expand the short-name whitelist.
4. Record positive type-to-callable dependencies and negative lookups used to
   exclude shadows. Import/package/type/alias changes, deletion and ambiguity must
   withdraw dependent callable facts, re-decide Java edges, and reconcile call
   references. A type edit can change a callable identity in an unchanged file.
5. Version parser facts and persisted evidence if implemented later. Prove profile
   upgrade reparsing, old-database repair and fresh/incremental convergence before
   enabling any recovery. No parser-profile or schema change accompanies this design.

This is one CG-35-owned prerequisite, not several speculative implementation
projects. Schedule compilation-scope identity work separately after reviewing its
cost and evidence contract; CG-36 must not silently implement it.

## Current implementation audit

### IMPLEMENTED

- Migration 006 stores Java/Kotlin type declarations and callable parameter and
  result positions, syntax state, modifiers, generic/value-class syntax,
  alias-target spelling, and parser provenance. Kotlin typealiases are
  file-scoped source facts and do not create graph symbols. The facts describe
  source syntax, not canonical JVM identity.
- Java and Kotlin adapters emit those facts in their current parser profiles.
  Profile-upgrade coverage reparses stored files; resolver semantic generations
  and resolver policy remain unchanged.
- Migration 007 adds independent `source`, `generated`, `excluded`,
  `dependency`, `external_metadata`, and `compiler_identity` scope dimensions.
  Each dimension is `complete`, `incomplete`, or `unknown`, with existing
  repositories initialized to `unknown`.
- `internal/store/jvm_type_dependencies.go` stores positive and negative
  observations only when scope is complete. Duplicate/conflicting candidates
  become ambiguous; absent or insufficient evidence stays unknown. These are
  invalidation observations, not callable-identity proof.
- The indexer rebuilds dependencies after changed-file persistence and before
  resolver pass 2. Provider and consumer changes mark affected observations
  dirty before recomputation, so a failed rebuild remains unusable and can be
  retried. Fresh indexing and incremental updates converge in the tested
  mutation matrix.

### LIFECYCLE AUDIT

| Change | Current behavior | Evidence |
| --- | --- | --- |
| Provider declaration or alias changes | Matching consumers are invalidated and recomputed | `TestJVMTypeDependenciesFailClosedAndConverge`, `TestJVMTypeAliasDependencyMutationAndDeletion` |
| Consumer imports change | The consumer's stored syntax is re-evaluated | `TestJVMTypeDependenciesImportOnlyChangeRedecidesConsumer` |
| Provider or consumer is deleted/retired | Dependent observations are invalidated; removed paths do not remain usable | `TestJVMTypeDependencyRetirementLeavesRetryableInvalidation` |
| Language transition | Old-provider dependencies are invalidated | `TestJVMTypeDependencyLanguageTransitionInvalidatesOldProvider` |
| Rebuild failure | Dirty state remains for retry | `TestJVMTypeDependencyRecomputeFailureLeavesDirtyStateForRetry` |
| Fresh versus incremental | Tested final observations converge across add/change/delete/ambiguity transitions | `TestJVMTypeDependencyConvergenceMatrix`, `internal/indexer/jvm_type_dependency_lifecycle_test.go` |
| Scope not supplied | Every completeness dimension remains unknown; no positive or negative completeness claim is made | `TestJVMCompilationScopeMigrationDefaultsExistingReposUnknown` |

### NOT IMPLEMENTED

- No build-evidence exporter or artifact importer supplies selected
  compilation, source-set, classpath, generated/excluded-source, dependency
  metadata, or compiler identity evidence.
- No classifier consumes these source/dependency observations to upgrade an
  unresolved call. The ordinary classifier remains limited to unresolved
  target categories and does not prove JVM callable identity.
- No compiler-backed canonical JVM owner/name/descriptor resolution is
  enabled. The 24 `Internal.parseCookie` calls remain unresolved.

### UNKNOWN

Without trusted build evidence, actual selected sources, generated and excluded
inputs, dependency aliases/classes, external metadata, Kotlin compiler/plugin
identity, and effective JVM ABI are unknown. An observed repository absence is
not proof of a builtin or dependency absence. No scope dimension may be
promoted from unknown based on parser success or dependency observation alone.

### TRUST BOUNDARY

Source parsers may report syntax facts and provenance. Only an explicitly
invoked, validated producer/import path may report build facts. Completeness is
per dimension and requires evidence for that dimension; exporter success alone
does not make any dimension complete. Stale or mismatched build fingerprints
make affected evidence unusable/unknown. Ordinary `index`, `update`, `watch`,
`serve` startup/autosync, and MCP index/update paths do not invoke Gradle,
Gradle wrappers, Maven, Maven wrappers, or project build scripts. Source audit
found process execution only for Git, Go tooling, and OS URL openers.

### FUTURE EXPORTER BOUNDARY

The authorized first producer target is one explicitly selected Kotlin Gradle
compilation. Its versioned artifact must identify the repository/build root,
selected Gradle project and compilation, observable Gradle/Kotlin/JDK identity,
input fingerprint, and per-dimension evidence with provenance. It must not
persist arbitrary environment variables or credentials. Until an artifact is
validated and its fingerprint matches current inputs, imported evidence is
unusable. This contract does not authorize automatic build execution from an
ordinary indexing path or generalize the producer to Maven.

## Failure modes and acceptance oracles

- Compiler plus `javap` on pinned compiler/stdlib/classpath is the ABI oracle;
  retain source hashes, exact JVM owner/name/descriptor and diagnostics.
- Cover ordinary/builtin positive cases and imported, same-package, lexical and
  dependency shadows; aliases to ordinary/value classes; alias removal/cycles;
  value parameters and results; inline classes; generic erasure; nested types;
  conflicting imports/declarations; malformed syntax; annotations and JVM renames.
- Missing scope evidence, parser errors, unknown metadata and ambiguity must
  remain unresolved. Successful compilation of one wildcard example is not a
  general identity proof.
- Compare independent fresh indexing with mutation/update on identical final
  trees using normalized declaration identities, symbols, references, edges,
  provenance/confidence and persisted semantic facts. Never compare row IDs.
- Reverse mutations and deletions must withdraw stale edges/references; competing
  declarations must fail closed; unaffected proven relations must remain intact.
- Preserve CG-36's 51 resolved cross-language edges, all original 44, its 24
  unresolved `parseCookie` calls and zero audited newly wrong bindings. Any future
  recovery needs source/compiler-grounded identity evidence, review and green CI.

## Separate existing convergence defect

At v2.0 `4b78a3281b55ffc0ac1cf8fbbd3e5efe2b1272a2`, adding the preserved
`CG35TypeIdentityProbe.kt` alias file causes the incremental suffix pass to bind
`TaskQueue.kt:146`'s `taskRunner.backend.nanoTime` to
`TaskRunner.Backend.nanoTime` using `dot_suffix/low`. Fresh indexing refuses it.
The alias file emits no symbols or callable facts and changes no persisted
external declaration identity; this is a JVM scope-ownership defect, independent
of unsupported alias ABI extraction. Generic strategies must honor persisted
JVM scope even when transaction-local scope veto tables are empty. Existing stale
suffix bindings need one-time re-decision and reference reconciliation.

Original evidence: `~/Projekti/codegraph-evidence/cg35-type-identity-20261002/`.
Exact mutation reproducer and repair proof:
`~/Projekti/codegraph-evidence/cg35-mutation-convergence-20261002/`.
