# CG-36 Java/Kotlin interop baseline

Preparation snapshot only. It pins a source corpus and records the current
`v2.0` behavior; it is not final acceptance while CG-32 changes Kotlin parse
output.

## Pins and reproduction

| Input | Pin |
| --- | --- |
| CodeGraph | `c6bfae3db50717d5241c8d0007f41df6201b9dc2` (`origin/v2.0` at preparation) |
| Mixed corpus | `square/okhttp`, tag `parent-4.12.0`, commit `4984568367caaf359b82c452bd28b5e192824d1c` |
| Source languages | 152 Java files; 251 Kotlin files (403 total) |

Reproduce in a clean checkout without adding generated files to the source
corpus:

```sh
git clone --depth 1 --branch parent-4.12.0 https://github.com/square/okhttp.git /tmp/cg36-okhttp
git -C /tmp/cg36-okhttp rev-parse HEAD
go run ./cmd/codegraph index /tmp/cg36-okhttp
go run ./cmd/codegraph stats /tmp/cg36-okhttp
go run ./cmd/codegraph update_graph /tmp/cg36-okhttp
```

Record the `symbols`, `references`, and `edges` counts and compare normalized
rows (source path and qualified identities, not database row IDs) between a
fresh index and the incremental result. The snapshot below used separate clean
clones for fresh and unchanged incremental indexing.

## Baseline at the pinned CodeGraph revision

Fresh index: 492 indexed files, 6,081 symbols, 36,394 references, 38,260
edges; 9.2 s reported total. Unchanged update: 0 files reparsed, 9 ms. A second
fresh clone took 9.5 s. Sorted normalized row SHA-256 values matched exactly:

| Rows | Count | SHA-256 |
| --- | ---: | --- |
| Symbols | 6,081 | `ee4c5e8b27601ee113098aaba6a60c3d5b9f4b6416ca4c95c748d1a55f847022` |
| References | 36,394 | `8b0acbe2aaf0053f00b0e604a8cd122bf225ab5d4a5893c59888384bdb9ecfdc` |
| Edges | 38,260 | `792c3113d4bc59bc17957cd07d829821a4ad2ad04ac5c199ff1560a9faa39021` |

There are 44 resolved cross-language call edges: 42 Java→Kotlin and 2
Kotlin→Java. All use package/import scope evidence. Source inspection of all 44
call sites found zero wrong target identities. This is a precision check over
resolved edges, not a recall oracle: unresolved and refused calls must be
counted separately when acceptance scoring is added.

Representative pinned source facts (repository paths are relative to the
OkHttp checkout):

| Direction | Caller source | Exact declared destination |
| --- | --- | --- |
| Java→Kotlin | `mockwebserver/src/test/java/okhttp3/mockwebserver/internal/http2/Http2Server.java:68`, `Platform.get()` | `okhttp/src/main/kotlin/okhttp3/internal/platform/Platform.kt`, `okhttp3.internal.platform.Platform.Companion.get` |
| Java→Kotlin | `okhttp/src/test/java/okhttp3/internal/ws/WebSocketRecorder.java:47`, `Platform.get()` | same Kotlin companion callable |
| Kotlin→Java | `okhttp/src/test/kotlin/okhttp3/CallKotlinTest.kt:229`, `RecordingProxySelector()` | `okhttp/src/test/java/okhttp3/internal/http/RecordingProxySelector.java`, `okhttp3.internal.http.RecordingProxySelector` |
| Kotlin→Java | `okhttp/src/test/kotlin/okhttp3/internal/platform/android/AndroidSocketAdapterTest.kt:83`, `DelegatingSSLSocketFactory(...)` | `okhttp/src/test/java/okhttp3/DelegatingSSLSocketFactory.java`, `okhttp3.DelegatingSSLSocketFactory` |

## Pinned acceptance cases already in-tree

These tests are the source-grounded minimal oracle to retain and extend; each
checks resolved identity or an explicit refusal, rather than name overlap:

- Java→Kotlin file facade, object instance/`@JvmStatic`, and companion:
  `internal/indexer/kotlin_java_interop_source_truth_test.go`.
- Kotlin→Java peer add/delete, import scope, staticness, and ambiguity:
  `internal/indexer/jvm_core_interop_lifecycle_test.go`.
- Exact callable arity and fresh/incremental lifecycle:
  `internal/indexer/kotlin_java_arity_lifecycle_test.go` and
  `internal/indexer/kotlin_companion_integration_test.go`.
- Refused fake facade, unsupported extension/parameterized ABI, and unsupported
  companion shape: `TestJVMGeneratedABINameAndCompanionRefusals` in
  `internal/indexer/jvm_core_interop_lifecycle_test.go`.
- Preserve the name-only negative exactly:
  `TestEcosystemInteropLosesNameOnlyRecall` in
  `internal/store/query_language_gating_test.go`. It has no import or binding
  evidence and must remain refused in both caller and callee queries.

## Expanded pinned-corpus source audit

The checkout at `/tmp/cg36-okhttp` was already present at the pinned commit.
Its existing CodeGraph database reproduced the baseline totals above with an
unchanged index (0 files reparsed). The audit below compares emitted edges to
the checked-in caller and declaration source; equal fresh/incremental digests
are not treated as correctness evidence. This audit ran 2026-10-02 with
CodeGraph `c6bfae3db50717d5241c8d0007f41df6201b9dc2` against OkHttp
`4984568367caaf359b82c452bd28b5e192824d1c`; the reproduction commands above
pin the tag and the corpus commit can be checked with `git rev-parse HEAD`.

The 44 emitted cross-language calls remain exact: **42 Java→Kotlin, 2
Kotlin→Java, zero wrong targets in the audited emitted set**. The scan also
found **26 source-proven Java→Kotlin calls left unresolved**:

| Caller source and lines | Source evidence | Kotlin declaration | CodeGraph result |
| --- | --- | --- | --- |
| `okhttp/src/test/java/okhttp3/CookieTest.java:96-123,361-394,570-572` (24 calls) | `import static okhttp3.internal.Internal.parseCookie;`; each call is `parseCookie(...)` | `okhttp/src/main/kotlin/okhttp3/internal/internal.kt:30`, `@file:JvmName("Internal") fun parseCookie(...)` | All 24 call edges have null targets |
| `mockwebserver/src/test/java/okhttp3/mockwebserver/MockWebServerTest.java:535` | `import static okhttp3.tls.internal.TlsUtil.localhost;`; `localhost()` | `okhttp-tls/src/main/kotlin/okhttp3/tls/internal/TlsUtil.kt:50`, top-level `fun localhost()` | Null target |
| `mockwebserver/src/test/java/okhttp3/mockwebserver/internal/http2/Http2Server.java:190` | Same static import; `localhost()` | Same Kotlin declaration | Null target |

These imports resolve the exact Kotlin JVM facade and callable in repository
source, so these are recall misses rather than name-only candidates. The
current pinned baseline therefore **does not satisfy recall acceptance**. The
26 misses are limited to two static-imported top-level callable identities;
this evidence does not justify implementing broader ABI families.

Name-collision negative controls were checked separately and are not counted as
misses or emitted false positives:

| Java source | Same-name Kotlin candidate | Why the Java call is not Kotlin interop |
| --- | --- | --- |
| `okhttp-logging-interceptor/src/test/java/okhttp3/logging/HttpLoggingInterceptorTest.java:75,129` | `HttpLoggingInterceptor.setLevel(Level)` in `okhttp-logging-interceptor/src/main/kotlin/okhttp3/logging/HttpLoggingInterceptor.kt` | `:75` declares a private Java `setLevel(Level)` helper; unqualified `setLevel(Level.BASIC)` at `:129` calls that local helper. Its body calls the Kotlin setter through `networkInterceptor` and `applicationInterceptor` receivers. Name-only matching to the Kotlin method would invent an edge. |
| `okhttp-dnsoverhttps/src/test/java/okhttp3/dnsoverhttps/DnsRecordCodecTest.java:31,36-37,41` | `String encoded = encodeQuery("google.com", TYPE_A);` / `TYPE_AAAA` | `DnsRecordCodec.encodeQuery(String, int)` in `okhttp-dnsoverhttps/src/main/kotlin/okhttp3/dnsoverhttps/DnsRecordCodec.kt` | Java declares a private `encodeQuery(String, int)` helper at `:36`; its body calls the Kotlin object through `DnsRecordCodec.INSTANCE.encodeQuery(...)`. The unqualified calls at `:31`/`:41` target the Java helper, not the Kotlin member. |

The controls show why unresolved same-name candidates cannot be counted as
false negatives without caller evidence. The 26 static-import cases above are
confirmed misses; the two controls must remain non-cross-language edges.

For final acceptance, split results into (1) correct exact-identity edges,
(2) wrong edges (must be zero), (3) unresolved source-grounded positives, and
(4) correctly refused negatives. Do not count unresolved cases as false
positives or use name-only overlap as a positive oracle. Re-run the pinned
corpus after CG-32 is integrated, then compare fresh and incremental normalized
rows and rerun the source-truth audit.

## Limits and costs

The real snapshot is a useful mixed-language repository but cross-language
calls are concentrated in test helpers and platform companions. Its 44
resolved edges do not establish broad product-code recall. The checkout was
about 41 MB; fresh indexing took about 9–10 seconds and needs only the normal
CodeGraph SQLite database. No build, Gradle dependency resolution, or generated
database is required. New ABI families remain out of scope.

## Current v2.0 acceptance checkpoint (2026-10-02)

CG-32 is Done. CG-35 and CG-36 remain In Progress. At v2.0
`4b78a3281b55ffc0ac1cf8fbbd3e5efe2b1272a2`, the source-grounded checkpoint is
51 resolved cross-language edges, all original 44 retained, 24 `parseCookie`
calls unresolved, zero audited newly wrong bindings, and no recoveries from the
latest type-identity investigation. The seven existing additions were checked
against exact imports, owners and declarations; this is not a complete recall
oracle over unresolved edges.

The alias-mutation convergence defect is separate from callable type identity:
an unrelated incremental suffix pass bypassed JVM scope refusal for one Kotlin
edge/reference. Repairing that ownership must preserve the checkpoint above,
including the refused `setLevel` and two `encodeQuery` helper controls.

The remaining `parseCookie` recovery depends on CG-35's compilation-scope type
identity evidence. See [bounded design](cg35-type-identity.md). Schedule that
prerequisite separately; do not relax this acceptance contract or mark CG-36 Done.
