# Language adapter contract

This document records the onboarding contract implemented by `internal/parser`
and the build-specific registries in `internal/cli/registry_{cgo,nocgo}.go`.
The production interfaces and tests are authoritative when implementation
details change.

## Mandatory adapter contract

Every `parser.Adapter` provides:

- `Language() string`: a stable, non-empty, lowercase language identifier. It
  is the key used by graph records, parser profiles, capabilities, and
  language-specific rules. Keep an established identifier when implementation
  or file coverage changes.
- `Supports(path string) bool`: the adapter's path fallback predicate. For
  registered extensions, `Registry.AdapterFor` selects the extension owner
  first; it does not ask all adapters to arbitrate.
- `Parse(context.Context, path string, content []byte) (graph.ParsedFile,
  error)`: return normalized graph facts for that file, or an error. Set the
  parsed language to `Language()`. Use one-based source lines and columns for
  known `graph.Position` values; leave uncertain facts absent rather than
  guessing.

`graph.ParsedFile` is the shared output shape. Emit only facts supported by
source evidence. Symbols need a stable semantic `StableKey` independent of
database IDs, traversal order, and file-system order. References and edges
that cannot be proven resolved must keep their target identity absent; an
ambiguous candidate must not be made into a resolved target. Preserve the
source location and spelling available from syntax. The meanings of symbol
kinds, edge kinds, scope evidence, imports, and language-specific syntax remain
language-specific, subject to those evidence and identity rules.

The registry consumes the following optional interfaces:

- `ExtensionProvider.Extensions() []string`: declare extensions owned by this
  adapter. The registry trims surrounding whitespace, lowercases values, and
  adds a leading dot. Empty entries are ignored and duplicates collapse.
  Different language IDs claiming the same normalized extension make it
  ambiguous: `AdapterFor` returns nil and `ExtensionCollisions` reports sorted
  language IDs. Never rely on registration order to break a collision.
- `ProfileProvider.Profile() Profile`: identify the persisted parser semantics
  as well as call-edge capability and semantic epoch. Production default
  registries require a known profile and positive semantic epoch. It remains
  optional on the base `Adapter` interface for test and transitional adapters;
  missing or conflicting profiles are surfaced by `LanguagesMissingProfile`.

Adapters sharing a language ID must have compatible profile ID and epoch. A
missing or conflicting profile removes the language profile; the registry
does not choose one. Multiple adapters may share a language when their
profiles agree, such as separate extensions routed to the same semantic
implementation.

## Profiles and compatibility

`parser.Profile.ID` is deterministic semantic provenance, conventionally
`<implementation>:<language>:vN`. Bump the ID when unchanged source could yield
different parser-owned persisted facts, including symbols, calls, imports,
re-exports, or scope evidence. Do not bump for refactors or performance changes
that preserve output. The ID describes parser semantics, not binary bytes,
build time, Git SHA, or CGO setting.

`SemanticEpoch` is the language-level compatibility generation across parser
families. Follow `SemanticEpochForProfile` and its compatibility table. Families
share an epoch only while they implement the same safety generation. Do not
infer compatibility from a matching version number or adapter name. A profile
ID change requests reparsing; it does not repair already-wrong stored rows.

`EmitsCallEdges` declares whether the adapter builds call edges. It must agree
with emitted graph behavior. It does not claim that those edges are resolved.

## Capabilities and build modes

`Registry.Capabilities` reports parser availability and profile-derived
dimensions. A known profile supports symbols; its `Calls` value reflects
`EmitsCallEdges`. Resolver dimensions (`CallResolution`, `ImportResolution`,
and `TypeResolution`) remain unknown unless a separate producer explicitly
establishes them. A nil field means unknown, not false and not support.

The optional no-CGO availability map records whether a parser and call graph
are available in a no-CGO build. It is build-independent metadata attached to
the registry in either build. Tree-sitter adapters may be replaced by fallback
adapters in no-CGO, with different profile IDs and graph capabilities; a
symbols-only fallback must declare `EmitsCallEdges: false`. Availability data
must match the actual `!cgo` registry and must not imply resolver or type
support. A future language can be absent in one build; consumers must preserve
that distinction rather than assume every registered language is available in
both modes.

## Incremental indexing and integration

The adapter parses one file and owns its stable file-local facts. The existing
indexer owns discovery, scheduling, incremental invalidation, persistence, and
cross-file resolution. A language adapter must not add special cases to those
generic layers merely to register itself. Parser semantic changes use profile
versioning; resolver policy changes belong to the resolver's existing
ownership and compatibility rules.

Minimal onboarding checklist:

1. Choose a stable language ID and declare its extensions.
2. Implement `Adapter`; implement `ExtensionProvider` for registered file
   types and `ProfileProvider` for stable production provenance.
3. Emit normalized `graph.ParsedFile` facts with stable symbol keys and source
   positions. Leave unproven and ambiguous targets unresolved.
4. Assign a profile ID and semantic epoch; declare call-edge capability
   truthfully.
5. Register the adapter in the appropriate CGO and no-CGO constructors, or
   document why only one build supports it; update no-CGO availability metadata.
6. Add positive, negative, and lifecycle fixtures described below.
7. Check user-visible capability output and both build modes.

## Required fixture coverage

- **Positive:** representative declarations, source ranges, stable keys,
  extension selection, and each semantic fact the adapter claims to emit.
- **Negative:** unsupported syntax, shadowing or ambiguity, unresolved calls or
  references, and cases where a tempting target cannot be proven. Assert no
  fabricated identity or capability.
- **Lifecycle:** profile identity and epoch, deterministic repeated parsing,
  extension collision refusal, build-mode availability, and incremental versus
  fresh indexing when parser-owned facts change.

Use focused parser fixtures for syntax and registry contracts. Add indexer or
store fixtures only when the new language's semantics require coverage of
existing generic integration. A test-only adapter must not require edits to
indexer, persistence, query, or watcher code.

## When an engine or schema change needs an owner decision

Pause normal adapter onboarding and document evidence if it requires a new
persisted fact, a changed meaning for an existing graph field, cross-language
identity, resolver evidence the current graph cannot represent, a migration,
or generic indexer/query/watcher behavior specific to one language. Keep the
adapter contract change separate until the engine or schema owner accepts that
design. Do not add speculative interfaces or parser abstractions to avoid that
decision.
