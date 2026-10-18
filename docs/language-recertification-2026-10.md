# Existing-language recertification (CG-98, 2026-10)

Focused macOS evidence for Go, Java, Kotlin, and Rust. This is not a whole-
language accuracy score, compiler-equivalence claim, or evidence that unresolved
edges are wrong. Parser registration, extracted call edges, and resolver support
are separate capabilities; absent proof remains unknown.

## Runtime capability snapshot

The CGO registry uses tree-sitter profiles for all four languages and records
call-edge production. In `CGO_ENABLED=0`, Go uses `go/ast` and retains calls;
Java, Kotlin, and Rust use heuristic symbol-only adapters. No-CGO update of a
tree-sitter graph is refused when it would discard semantic edges. Resolver and
type-resolution are not blanket language capabilities: the registry leaves
those dimensions unknown where it has no explicit proof.

| Language | CGO parser profile | CGO capability | no-CGO parser profile | no-CGO capability | Known limits |
| --- | --- | --- | --- | --- | --- |
| Go | `treesitter:go:v2` | symbols, calls, imports; bounded lexical/type evidence | `go/ast` | symbols and calls | interface dispatch, ambiguous or unsupported return types, and shadowed constructors remain unresolved |
| Java | `treesitter:java:v14` | symbols, calls, imports; constrained declaration/arity evidence | `heuristic:java:v1` | symbols only | inheritance and unproven overload/expression receiver typing remain unresolved |
| Kotlin | `treesitter:kotlin:v12` | symbols, calls, imports; selected JVM ABI facts | `heuristic:kotlin:v4` | symbols only | unsupported ABI, inheritance, extensions, and ambiguous interop remain unresolved |
| Rust | `treesitter:rust:v5` | symbols, calls, imports; module and visibility evidence | `heuristic:rust:v1` | symbols only | macro/cfg expansion, trait dispatch, and ambiguous glob/import evidence remain unresolved |

## Focused lifecycle and source evidence

Run these commands from the repository root. Results below are from macOS arm64,
Go 1.26.1, CGO enabled unless marked otherwise, on 2026-10-18.

| Command | Result | Evidence |
| --- | --- | --- |
| `go test ./internal/indexer -run 'Test(GoReceiverThroughFunctionResultIncrementalParity|JavaFieldReceivers|JavaGenericConstructionFollowsIncrementalChanges|KotlinJavaOverloadAndCallArityLifecycle|RustPrivateVisibilityFollowsIncrementalChanges|RustModuleScopeReexportsAndVisibility)$' -count=1` | PASS, 6 tests matched | Fresh index, incremental transitions, explicit no-op update and fresh-graph parity for Go/Java/Kotlin/Rust. |
| `go test ./internal/parser ./internal/cli -run 'Test(Registry|LanguageCapabilities|CgoRegistryProfiles|DefaultRegistryProfiles|ParserProfile)' -count=1` | PASS, 7 tests | CGO profiles and advertised capabilities. |
| `CGO_ENABLED=0 go test ./internal/indexer -run 'Test(GoReceiverThroughFunctionResultIncrementalParity|GoReceiverThroughFunctionResult)$' -count=1` | PASS, 2 tests | Go fallback parser lifecycle and no-op parity. |
| `CGO_ENABLED=0 go test ./internal/parser ./internal/cli -run 'Test(Registry|LanguageCapabilities|NoCgoRegistryProfiles|ParserProfile)' -count=1` | PASS, 6 tests | no-CGO profiles and advertised capabilities. |

Evidence source classification:

- **Compiler-verified:** existing Java fixture contracts record `javac 17`
  oracles; installed compiler version was `javac 17.0.9`. Rust visibility and
  import fixtures record `rustc 1.98.1` oracles; installed compiler is
  `rustc 1.98.1 (48a229cea 2026-09-01)`. This PR reruns the fixtures, not every
  compiler oracle command.
- **Go:** indexing tests run in CGO and no-CGO modes, and Go's existing source
  fixtures include `go vet` evidence. This is not a corpus-wide `go test` of
  arbitrary projects.
- **Kotlin:** parser/indexer fixture evidence only. `kotlinc` is unavailable on
  this machine, so independent Kotlin compiler validation is unverified.

## Remaining acceptance work

This slice does not complete CG-98. Real-repository certification is still
needed for each language: representative pinned repositories, compiler/source
truth comparisons where practical, correct/incorrect/unresolved edge audits,
known-limitations review, and fresh/incremental/no-op convergence over those
corpora. Kotlin compiler validation remains unverified. C/C++ real-repository
certification is excluded here and reserved for the Windows CG-99 campaign.
No public score, benchmark, or whole-language correctness claim is made.
