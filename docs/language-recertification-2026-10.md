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

The registry contract tests pin immutable capability snapshots (including
pointer fields and extension slices), routing for every declared extension,
normalized language IDs, and the previously supported no-CGO language set.
Additional languages remain allowed; no-CGO availability is checked against
the profiles and call-edge behavior of the registered fallback adapters.

| Language | CGO parser profile | CGO capability | no-CGO parser profile | no-CGO capability | Known limits |
| --- | --- | --- | --- | --- | --- |
| Go | `treesitter:go:v2` | symbols, calls, imports; bounded lexical/type evidence | `go-ast:go:v2` | symbols and calls | interface dispatch, ambiguous or unsupported return types, and shadowed constructors remain unresolved |
| Java | `treesitter:java:v14` | symbols, calls, imports; constrained declaration/arity evidence | `heuristic:java:v1` | symbols only | inheritance and unproven overload/expression receiver typing remain unresolved |
| Kotlin | `treesitter:kotlin:v15` | symbols, calls, imports; primary-constructor evidence and selected JVM ABI facts | `heuristic:kotlin:v4` | symbols only | constructor overload/type inference, inheritance, extensions, and ambiguous interop remain unresolved |
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


## Historical snapshot: focused evidence at `fe83c78125a424a6c68116410a35d3edc7928a38`

The following source-backed evidence is preserved from the earlier report. It
records tests and behavior at that exact commit; it was not rerun on the current
head and must not be read as current validation. The detailed resolver
contracts and limitations remain in [`scope-models.md`](scope-models.md).

### Capability evidence

The registry has 11 IDs: `cpp`, `csharp`, `go`, `java`, `kotlin`, `php`,
`python`, `ruby`, `rust`, `swift`, and `typescript`. It reports parser profile
and call-edge availability separately. Call-resolution and type-resolution
capabilities remain unknown; parser registration does not prove those
capabilities. Extension collisions fail closed.

In CGO builds the registered adapters use tree-sitter profiles and report call
edges. In `CGO_ENABLED=0` builds, Go uses `go/ast` and Python uses its regex
fallback; these retain call edges. The other nine languages use heuristic
symbols-only adapters. The indexer refuses to update a tree-sitter graph with
those degraded adapters, since that would discard call edges.

### Priority languages

| Language | Parser and extracted facts | Resolution evidence | Incremental and build evidence | Known boundaries |
| --- | --- | --- | --- | --- |
| Go | Tree-sitter in CGO; `go/ast` otherwise. Packages, declarations, imports, lexical locals and calls. | Package/import paths and stated local types; unique candidates only. | `TestGoReceiverScopeIncrementalParity`; CGO and no-CGO test runs passed. | Inference, promoted/interface methods, and several package-variable/field paths are not modeled. `go/ast` rejects syntax errors that tree-sitter may recover. |
| Python | Tree-sitter in CGO; regex fallback otherwise. Classes/functions, lexical bindings, imports and calls. | Import/local scope and module identity; ambiguity and unsupported dynamic bindings stay unresolved. | `TestPythonImportScopeFullIncrementalParity`; regex adapter lifecycle and lexical parity passed without CGO. | No `sys.path` or packaging config; wildcard imports and dynamic behavior remain unresolved; fallback omits some module-level calls and keyword syntax. |
| TypeScript/JavaScript | Tree-sitter extracts declarations, imports/re-exports and plain-name/property-chain calls. | Relative in-repository module graph; call ownership refuses unresolved/ambiguous edges. | `TestTypeScriptImportShadowingFollowsIncrementalChanges` passed. The CGO suite exercises it; no-CGO is symbols-only. | No package resolution, CommonJS, path aliases, directory index resolution, or deep namespace members. |
| Java | Tree-sitter extracts package/imports, types, methods/constructors, visibility/staticness and syntactic call arity. | Unique visible types/methods/constructors; overload selection uses only recorded arity. | `TestJavaGenericConstructionFollowsIncrementalChanges` passed. The CGO suite exercises it; no-CGO is symbols-only. | No inheritance/interface dispatch, argument-type overload resolution, or expression receiver typing. |
| Kotlin/JVM | Tree-sitter extracts package/imports/classes/functions, primary-constructor parameter evidence, and selected JVM facade/name/arity facts. | Kotlin calls and conservative zero-argument class construction, plus narrow Java interop, bind only unique modelled JVM shapes. | `TestKotlinConservativeClassConstructionLifecycle` and `TestKotlinJavaOverloadAndCallArityLifecycle` pass. The CGO suite exercises it; no-CGO is symbols-only. | No constructor overload/type inference, inheritance/extensions, type aliases, default imports, or compiler-derived complete ABI. |
| C/C++ (`cpp`) | Tree-sitter C for `.c`; C++ for registered C++ extensions. Declarations, signatures, linkage, includes and calls. | Same-file/include visibility plus exact namespace/class ownership; overload ambiguity refuses binding. | `TestCppQualifiedCallFullUpdateParity` passed. The CGO suite exercises it; no-CGO is symbols-only. | Build configuration, macro expansion, inheritance, ADL, overload argument typing, templates and aliases are not modeled. |

### Focused verification recorded at the historical SHA

- CGO: six selected lifecycle/parity tests passed across the six rows above.
- No-CGO: three selected Go/Python parity tests passed.
- CGO and no-CGO parser registry/profile/capability tests passed.
- Tests establish these focused behaviors only; this snapshot is not a full
  language certification, real-repository audit, or benchmark.

Reproduce the historical focused checks with:

```sh
go test ./internal/indexer -run 'Test(GoReceiverScopeIncrementalParity|PythonImportScopeFullIncrementalParity|TypeScriptImportShadowingFollowsIncrementalChanges|JavaGenericConstructionFollowsIncrementalChanges|KotlinJavaOverloadAndCallArityLifecycle|CppQualifiedCallFullUpdateParity)$' -count=1
CGO_ENABLED=0 go test ./internal/indexer -run 'Test(GoReceiverScopeIncrementalParity|PythonImportScopeRegexAdapter|PythonLexicalScopeRegexAdapter)$' -count=1
go test ./internal/parser -run 'Test(Registry|LanguageCapabilities)' -count=1
CGO_ENABLED=0 go test ./internal/parser -run 'Test(Registry|LanguageCapabilities)' -count=1
```
