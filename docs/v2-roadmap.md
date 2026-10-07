# CodeGraph v2.0 master roadmap

> **Internal pre-release planning document.** Remove this file during the final
> documentation hygiene pass before the v2.0 release candidate.

This is an execution and dependency roadmap. Plane owns live task state; Git and
the current source tree are authoritative for what is implemented. Keep parked
and POST-V2 items parked unless their owner explicitly changes scope.

## 0. Preserve and reconcile current work

- Recover and review the existing CG-18 semantic-diff first slice before any
  expansion; preserve its worktree and evidence.
- Reconcile CG-35 documentation with the Phase 1/2 implementation already on
  v2.0 and keep the future producer boundary explicit.
- Keep owner-deferred and POST-V2 work parked. CG-21 remains POST-V2 unless
  current repository authority changes it; do not revive CG-36 without owner
  authorization.

## 1. Existing correctness foundation

Close the correctness contracts before downstream risk or trust claims:

- CG-14: methodology and scope contract for evidence-backed relationships.
- CG-18: semantic graph diff, preserving deterministic output and unresolved
  ambiguity.
- CG-19: edit-safety and risk foundation, reusing CG-18 and existing evidence
  rather than inventing duplicate engines.

## 2. Benchmark methodology now, final numbers later

- CG-22: freeze methodology, representative workloads, and release floors.
- CG-23: define the machine-readable benchmark artifact and reproducibility
  contract.
- Run final public benchmark execution only after feature freeze and after the
  methodology and artifact contracts are closed.

## 3. Trusted compilation-scope evidence

CG-35 already has source-fact and dependency-lifecycle foundations. The missing
piece is trusted, current compilation-scope evidence:

- Define an explicit, user-invoked exporter contract, beginning with one Kotlin
  Gradle compilation.
- Evidence must be versioned and fingerprinted and describe selected,
  generated, and excluded sources, dependency metadata, and compiler identity.
- Define Store lifecycle, invalidation, and stale-evidence behavior.
- Keep all six scope dimensions independently `UNKNOWN` unless current evidence
  supports each dimension.
- Keep the classifier disabled until a separate compiler-oracle-backed proof
  authorizes activation.
- Ordinary indexing must never implicitly execute Gradle, Maven, or project
  code.

## 4. Comparative implementation reconnaissance

Before difficult parser, resolver, or runtime work, perform comparative
implementation reconnaissance / external-reference audit. Inspect relevant
public prior art, regression tests, design notes, and issue history. Record:

- our current behavior;
- comparable implementation behavior;
- transferable ideas and incompatible assumptions;
- useful regression tests;
- licensing and provenance obligations if implementation code is later reused.

Initial areas: Kotlin/JVM ownership and interop; Swift member/owner resolution;
C/C++ parsing, build metadata, and include resolution; Windows writer/process
lifecycle; worktrees; SQLite/WAL and resolver scaling; grammar provenance and
version selection; framework and dynamic boundaries.

## 5. Go / Tree-sitter infrastructure decision

- Go remains the default implementation language.
- Compare the current Tree-sitter binding with the official current Go binding.
- Profile filesystem, parse, extract, store, resolve, synthesis, and MCP stages.
- Do not introduce Rust or a native kernel unless measured evidence justifies
  an isolated component; do not perform a speculative rewrite.

## 6. Language Platform V1

Define one per-language contract for language ID, extensions and detection;
grammar source and pinned provenance; build-mode parser availability;
symbol/import/call/reference/type/inheritance capabilities; resolver and
framework/cross-language capabilities; known limitations; and quality status,
fixtures, and benchmark references. Re-certify existing languages through this
platform before treating expansion as complete.

## 7. Language expansion

Wave 1 candidates: Dart, Scala, Lua/Luau, Terraform/OpenTofu, Solidity, Nix,
Vue, Svelte, and Astro.

Wave 2 candidates, only if Wave 1 quality remains strong: R, Objective-C,
Erlang, VB.NET, Pascal/Delphi, COBOL, CFML, ArkTS, Liquid, and CUDA/Metal.
v2.0 does not require every Wave 2 language.

Every retained new language needs pinned parser provenance, focused torture
fixtures, representative real-repository validation where practical,
deterministic graph output, a wrong-binding audit, documented limitations,
fresh and incremental validation, and release-platform build validation.

## 8. Selected cross-language and framework richness

Prioritize high-value, evidence-backed relationships. Keep dynamic and
reflection-heavy boundaries explicit when static evidence is insufficient.

## 9. Runtime hardening

Harden multi-MCP writer ownership, stale writer/process state, Windows semantics,
worktrees, watcher and update durability, rebuild exclusivity, SQLite/WAL
behavior, and safe operation on large repositories.

## 10. Performance pass

Wait until the feature surface is close to frozen. Optimize measured bottlenecks
only. Finish CG-22 release floors and CI budgets, and CG-23 release benchmark
evidence, in this phase.

## 11. Documentation and launch surface

### 11.1 README split

The root `README.md` should become a concise GitHub/launch landing page covering
what CodeGraph is, why it matters, quick install, a 30-second usage example,
strongest capabilities, supported agents, a compact language/quality summary,
a reproducible benchmark/example, and a link to detailed docs.

Move technical reference material to `docs/README-detailed.md`, including or
linking to the MCP tool reference, progressive disclosure, compact output,
query contracts, graph capability and limitations, database/index behavior,
CLI reference, configuration, embeddings/agentic mode, architecture,
build-from-source, and troubleshooting. Do not perform this README rewrite in
this wave.

## 12. Pre-release hygiene and release gates

Before RC:

- Audit every remaining PRE-V2 item and choose ship, defer, remove, or POST-V2.
- Remove `docs/v2-roadmap.md`.
- Review and remove historical roadmap documents that no longer help users or
  contributors.
- Remove superseded task-specific handoff/planning documents and benchmark
  scratch/planning documents.
- Retain durable user/contributor docs describing actual behavior,
  architecture, limitations, or provenance.

Only after this hygiene pass proceed to the existing release checklist, RC, tag,
and publish sequence.

## Explicit v2.0 non-goals

- Hosted/team product or private commercial backend.
- Matching another project's feature count or supporting every language.
- Eliminating every dynamic-analysis boundary.
- A speculative full-engine rewrite.
- Already-deferred POST-V2 items.

## v2.0 exit criteria

- Correctness foundation is closed with evidence-backed, ambiguity-safe
  relationship contracts.
- Semantic diff and edit-safety contracts are implemented and reviewed.
- Benchmark methodology and reproducible release evidence are complete.
- Compilation-scope evidence has a safe, explicit producer path.
- Language Platform V1 is maintainable and retained languages have documented
  quality, limitations, fixtures, and platform validation.
- Selected bridges are evidence-backed; unsupported dynamic boundaries remain
  explicit.
- Supported-platform runtime behavior is robust, including writer, worktree,
  watcher, rebuild, and SQLite/WAL lifecycle.
- Performance work targets measured bottlenecks.
- The concise README and separate detailed docs reflect shipped behavior.
- Internal planning artifacts are removed.
- No PRE-V2 release blocker remains unresolved.
- The release artifact matrix is validated.

## Intended dependency queue

The current evidence-based queue and any deviations from the initial proposal
are maintained in the coordinator report for this wave. Re-evaluate dependencies
against current tickets and source before starting each phase; this document
defines phase order, not live task status.
