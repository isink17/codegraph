# Existing language recertification (2026-10)

This is a source-backed evidence snapshot, not a completeness or performance
claim. It records the behavior and tests at CodeGraph `fe83c78125a424a6c68116410a35d3edc7928a38`.
The detailed resolver contracts and limitations remain in
[`scope-models.md`](scope-models.md).

## Capability evidence

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

## Priority languages

| Language | Parser and extracted facts | Resolution evidence | Incremental and build evidence | Known boundaries |
| --- | --- | --- | --- | --- |
| Go | Tree-sitter in CGO; `go/ast` otherwise. Packages, declarations, imports, lexical locals and calls. | Package/import paths and stated local types; unique candidates only. | `TestGoReceiverScopeIncrementalParity`; CGO and no-CGO test runs passed. | Inference, promoted/interface methods, and several package-variable/field paths are not modeled. `go/ast` rejects syntax errors that tree-sitter may recover. |
| Python | Tree-sitter in CGO; regex fallback otherwise. Classes/functions, lexical bindings, imports and calls. | Import/local scope and module identity; ambiguity and unsupported dynamic bindings stay unresolved. | `TestPythonImportScopeFullIncrementalParity`; regex adapter lifecycle and lexical parity passed without CGO. | No `sys.path` or packaging config; wildcard imports and dynamic behavior remain unresolved; fallback omits some module-level calls and keyword syntax. |
| TypeScript/JavaScript | Tree-sitter extracts declarations, imports/re-exports and plain-name/property-chain calls. | Relative in-repository module graph; call ownership refuses unresolved/ambiguous edges. | `TestTypeScriptImportShadowingFollowsIncrementalChanges` passed. The CGO suite exercises it; no-CGO is symbols-only. | No package resolution, CommonJS, path aliases, directory index resolution, or deep namespace members. |
| Java | Tree-sitter extracts package/imports, types, methods/constructors, visibility/staticness and syntactic call arity. | Unique visible types/methods/constructors; overload selection uses only recorded arity. | `TestJavaGenericConstructionFollowsIncrementalChanges` passed. The CGO suite exercises it; no-CGO is symbols-only. | No inheritance/interface dispatch, argument-type overload resolution, or expression receiver typing. |
| Kotlin/JVM | Tree-sitter extracts package/imports/classes/functions and selected JVM facade/name/arity facts. | Kotlin calls and narrow Java interop bind only unique modelled JVM shapes. | `TestKotlinJavaOverloadAndCallArityLifecycle` passed. The CGO suite exercises it; no-CGO is symbols-only. | No Kotlin constructor-call resolution, inheritance/extensions, type aliases, default imports, or compiler-derived complete ABI. |
| C/C++ (`cpp`) | Tree-sitter C for `.c`; C++ for registered C++ extensions. Declarations, signatures, linkage, includes and calls. | Same-file/include visibility plus exact namespace/class ownership; overload ambiguity refuses binding. | `TestCppQualifiedCallFullUpdateParity` passed. The CGO suite exercises it; no-CGO is symbols-only. | Build configuration, macro expansion, inheritance, ADL, overload argument typing, templates and aliases are not modeled. |

## Focused verification

At the recorded SHA:

- CGO: six selected lifecycle/parity tests passed across the six rows above.
- No-CGO: three selected Go/Python parity tests passed.
- CGO and no-CGO parser registry/profile/capability tests passed.
- Tests establish these focused behaviors only; this snapshot is not a full
  language certification, real-repository audit, or benchmark.

Reproduce the focused checks with:

```sh
go test ./internal/indexer -run 'Test(GoReceiverScopeIncrementalParity|PythonImportScopeFullIncrementalParity|TypeScriptImportShadowingFollowsIncrementalChanges|JavaGenericConstructionFollowsIncrementalChanges|KotlinJavaOverloadAndCallArityLifecycle|CppQualifiedCallFullUpdateParity)$' -count=1
CGO_ENABLED=0 go test ./internal/indexer -run 'Test(GoReceiverScopeIncrementalParity|PythonImportScopeRegexAdapter|PythonLexicalScopeRegexAdapter)$' -count=1
go test ./internal/parser -run 'Test(Registry|LanguageCapabilities)' -count=1
CGO_ENABLED=0 go test ./internal/parser -run 'Test(Registry|LanguageCapabilities)' -count=1
```
