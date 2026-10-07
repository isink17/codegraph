TEMPORARY PRE-RELEASE RESEARCH.
REMOVE OR CURATE BEFORE V2 RC.

# V2 implementation reconnaissance

Research-only notes from 2026-10-07. No implementation code was copied. These
observations propose tests and future investigations; they are not product
contracts or release claims.

## Comparative implementation patterns

| Area | Useful pattern | Regression test worth adding | Incompatible assumption / provenance concern | Recommended action |
| --- | --- | --- | --- | --- |
| Kotlin/JVM | Keep Kotlin source owner separate from JVM facade/companion owner and descriptor. `@JvmStatic` can expose both a static bridge and companion instance method; top-level functions use file facades; `@JvmField` changes field exposure. | File facade, `@file:JvmName`, multifile facade, companion with/without `@JvmStatic`, duplicate static/instance overloads, Java `Type::method` references. | Source name/owner does not prove JVM owner/name/descriptor. Kotlin's interop docs are the authority for language behavior; each future fixture needs compiler/version provenance. | Keep compiler-backed identity gated on complete scope/compiler evidence. Ref: <https://kotlinlang.org/docs/java-to-kotlin-interop.html#static-methods> |
| Swift | Preserve implicit `self`, explicit `self`, lexical owner, and extension target as separate evidence. Distinguish protocol requirements/witnesses from extension-only default implementations. | Implicit vs explicit self, nominal/protocol extension, base plus extension overloads, witness vs extension-only call, nested closure capture. | An extension spelling alone does not prove its extended nominal type; protocol extension methods are not all dynamically dispatched. Swift language reference is the source oracle; no code copied. | Add Swift resolution only after extension-target and protocol-dispatch evidence is modeled. Refs: <https://docs.swift.org/swift-book/documentation/the-swift-programming-language/methods/> and <https://docs.swift.org/swift-book/documentation/the-swift-programming-language/extensions/> |
| C/C++ | Use per-command compile database entries for working directory, source, defines, and include paths. Treat preprocessing output as configuration-specific evidence. | C vs C++ `extern "C"`, macro-controlled branches, include-path selection, function-pointer typedefs, callback registration, preprocessing variants. | `extern "C"` changes linkage, not semantic owner. A macro's source spelling may not be active in a given compile. Function pointers/callbacks are indirect without points-to evidence. Clang compile database and C++ language linkage docs are cited; no code copied. | Consume `compile_commands.json` as explicit build evidence before attempting macro-aware identity. Refs: <https://clang.llvm.org/docs/JSONCompilationDatabase.html> and <https://en.cppreference.com/w/cpp/language/language_linkage.html> |
| Runtime / worktrees / Windows | Identify a process by PID plus start time or boot identity; distinguish a Git worktree root/index from its common object directory. Keep mutable graph identity tied to the worktree root. | PID reuse, two worktrees sharing common objects, Windows drive/UNC/case paths, symlink/junction escape, atomic replacement, lock and cleanup behavior. | PID alone can be reused; Git common-dir is not a unique working tree. Windows path and rename behavior cannot be inferred from Unix tests. No external implementation was copied; platform behavior still needs native tests. | Audit process identity and shared-index ownership before adding multi-client runtime state; add Windows-native tests to those changes. |
| SQLite / resolution | Measure writer wait, transaction duration, WAL size/checkpoint delay, resolver candidate counts, and query plans before changing concurrency. SQLite WAL still serializes writers; readers use snapshots and long readers can delay checkpoint progress. | Concurrent readers plus one writer, long read transaction/checkpoint, candidate fan-out scaling, indexed/unindexed query plans, cancellation while waiting for a write. | A pool of writers cannot make SQLite WAL multi-writer. Network filesystems are outside WAL's same-host shared-memory model. CodeGraph currently sets one open connection per Store (`internal/store/store.go:977`), uses WAL (`:1172`), and obtains explicit `BEGIN IMMEDIATE` write locks (`:1272`, `:1312`). | Preserve one-writer semantics; optimize measured transaction length and candidate selection first. Ref: <https://sqlite.org/wal.html> |

## Tree-sitter binding decision

Current CodeGraph dependency: `github.com/smacker/go-tree-sitter` at
`v0.0.0-20240827094217-dd81d9e9be82`. Candidate: official
`github.com/tree-sitter/go-tree-sitter v0.25.0`.

| Dimension | Current smacker binding | Official Go binding v0.25.0 | Migration implication |
| --- | --- | --- | --- |
| Parser API | `NewParser`, `SetLanguage`, `ParseCtx`; language adapters are bundled packages. | `NewParser`, error-returning `SetLanguage`, `Parse`/`ParseCtx`. | Mechanical call sites differ; errors need explicit handling. |
| Parser/tree ownership | C-backed parser/tree; current shared `common.parse` returns a root node without explicitly closing or retaining parser/tree; lifetime depends on smacker wrappers/finalizers. | Parser, Tree, Node, Cursor, Query and other C-backed values have explicit `Close`; closing a tree invalidates its nodes. | Refactor shared extraction to keep the tree alive through parsing and close tree/parser after extraction; import-only conversion is unsafe. |
| Grammar loading | Many grammars are bundled in the same module, including Kotlin, Swift, C#, Go, Python, Java, JS/TS, C/C++. | Runtime binding has no grammar bundled; Go, Python, Java, JS, C/C++ and others are separate modules. The declared official grammar modules do not include Kotlin, Swift, or C#. | Each grammar becomes a separately versioned dependency. No official Kotlin/Swift grammar module was found in this audit. |
| ABI | Cached source reports Tree-sitter ABI 14 (minimum supported ABI 13). | v0.25.0 targets ABI 15 and accepts minimum ABI 13. | Grammar/runtime compatibility needs explicit pinned version checks. |
| Queries and cursors | Query and cursor APIs are present; resource lifecycle differs by binding. | Query/QueryCursor are present and require `Close`. | Audit all query uses and cleanup paths, not only parser extraction. |
| Changed ranges | No corresponding tree changed-ranges method was found in the current wrapper. | `Tree.ChangedRanges` is available. | Feature availability alone does not improve CodeGraph: production creates fresh parsers per file and does not use incremental edit/reparse. |
| Incremental parsing | Tree edit/reparse APIs exist. | Tree edit/reparse APIs exist. | Any future use needs byte/point, UTF-8, CRLF and edit-range parity tests. |
| Byte/point semantics | Byte offsets and row/byte-column coordinates. | Byte offsets and row/byte-column coordinates. | Pin multibyte UTF-8 and CRLF fixtures before migrating. |
| CGO/linkage | Requires CGO and a C toolchain; grammars are compiled into the module. | Requires CGO and a C toolchain; grammar modules compile separately. | Neither binding provides a no-CGO parser. Static/dynamic and cross-build outputs need per-platform tests. |
| Platform evidence | Current CodeGraph CI is the evidence for supported targets; this spike only ran darwin/arm64. | Source has Windows and Unix support files, but this spike did not run Windows or cross-compilation. | No portability claim from this spike. Run darwin amd64/arm64, Linux amd64/arm64 and Windows natively before a migration. |
| Concurrency | Production creates a parser per parsed file; shared-parser safety is not relied on. | Same constraint until proven otherwise. | Do not pool/share mutable parsers without race and cancellation tests. |
| Maintenance | Pinned pseudo-version timestamp is 2024-08-27; this alone does not prove abandonment. | Official Tree-sitter organization binding, tagged v0.25.0; module declares Go 1.23. | Recheck release cadence and issue/CI state before a future decision. |

### Throwaway measurement

Separate `/tmp/codegraph-ts-spike/current` and `official` Go modules avoided
linking both C runtimes into one binary. Same synthetic Go and Python bytes were
parsed by each binding. Inputs were 1,840 bytes (Go, SHA-256
`2b1b93833d48f1442c95aadb4cf3a4a18def3e1feec0a700dc8270f396e03e8f`) and 1,940
bytes (Python, SHA-256
`ba5c469b57d124857e0d3eeae5eb870211924577d9c7ab5cf3e779127479840b`). Each
operation used a fresh parser or reused one parser, then closed tree/parser;
traversal recursively counted every child. Run command was
`go test -run '^$' -bench . -benchmem -benchtime=100ms -count=10`, summarized
with `benchstat -ignore pkg`.

Environment: Go 1.26.1, macOS darwin/arm64, Apple M3 Pro, CGO enabled, Apple
clang 21.0.0 via `cc`. This was a local throwaway measurement with no
CPU-isolation; it is not an end-to-end CodeGraph result.

| Operation | Language | Current median | Official median | Official delta | Current vs official allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| New parser + parse + close | Go | 196.4 µs | 306.3 µs | +55.95% | 6 vs 6 |
| New parser + parse + close | Python | 235.7 µs | 364.2 µs | +54.53% | 6 vs 8 |
| Reused parser + parse + close | Go | 197.6 µs | 306.6 µs | +55.20% | 4 vs 6 |
| Reused parser + parse + close | Python | 242.0 µs | 361.4 µs | +49.32% | 4 vs 8 |
| Traverse retained tree | Go | 166.4 µs | 149.0 µs | -10.48% | 1 vs 1,000 |
| Traverse retained tree | Python | 147.4 µs | 131.4 µs | -10.87% | 1 vs 880 |

The official binding allocated 2,192/2,288 B per parse operation versus 232 B
for the current binding on Go/Python. Traversal allocated 32,000/28,160 B versus
about 327/164 B. These are API-level measurements on small inputs. The Go tree
node counts differ (1,041 current, 1,001 official) while Python counts match
(881), demonstrating grammar-version/shape differences; parse-time deltas are
not a like-for-like performance result. Raw outputs and temporary harness are
preserved under `/tmp/codegraph-ts-spike/`.

### Decision

**NEED_MORE_EVIDENCE.** Do not migrate in this wave. The candidate lacks the
currently used Kotlin, Swift and C# grammars in its declared module set, changes
the root-node ownership boundary, and the Go grammar produced a different node
count. The measurements do not show a performance case for migration and are
not representative of full CodeGraph indexing. Next decision requires a
grammar-source/license plan, parse/extraction parity for each shipped language,
resource-lifetime refactor proof, and native platform matrix. Check the binding
and each grammar's license/provenance independently before adding dependencies;
the binding license does not cover separately versioned grammars.

Official binding sources: <https://github.com/tree-sitter/go-tree-sitter/tree/v0.25.0>
and <https://pkg.go.dev/github.com/tree-sitter/go-tree-sitter>. Current binding
source: <https://github.com/smacker/go-tree-sitter/tree/dd81d9e9be82>.
