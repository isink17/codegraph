# Optional JVM Binary Signature Inspector

`codegraph inspect-jvm-class` is an explicit diagnostic for one Kotlin JVM
`.class` file. It verifies method signatures in the exact supplied bytes and
does not contribute evidence to indexing, resolution, SQLite, or MCP.

## Build the helper

Build from the repository root with Java 17 and Gradle 9.3.1 available:

```sh
tools/jvm-binary-inspector/gradlew -p tools/jvm-binary-inspector clean installDist
```

The resulting distribution is
`tools/jvm-binary-inspector/build/install/codegraph-jvm-binary-inspector/`.
It contains the helper and an explicit `lib/` classpath. Runtime dependencies
are Kotlin metadata JVM 2.3.20, Kotlin standard library 2.3.20, ASM core and
ASM tree 9.8, and JetBrains annotations 13.0. The Kotlin Gradle plugin is a
build-time dependency. The helper is built with a Gradle application
distribution rather than a shaded JAR so every runtime component remains
visible and independently stageable.

## Run one inspection

Supply the class, installed helper directory, absolute Java 17 executable,
and explicit authorization to execute the helper:

```sh
codegraph inspect-jvm-class \
  --input /path/to/Api.class \
  --inspector-dir /path/to/codegraph-jvm-binary-inspector \
  --java /path/to/java \
  --trust-inspector
```

Java and the helper are required only for this command. Normal CodeGraph
commands do not look for, download, install, or invoke either. The command
does not run Gradle or compile a target project.

The helper is executable code and must be trusted. `--trust-inspector` is an
operator authorization, not a sandbox. CodeGraph stages its JARs and the
input into a private temporary directory and launches the explicitly named
Java executable with an explicit classpath. The inspected class bytes are
parsed as data; the class is never loaded or executed, and its static
initializers do not run.

## Evidence contract

The JSON schema is
`codegraph.jvm_binary_signature_evidence/v1`. The artifact SHA-256 identifies
the exact bytes staged and read by the helper. The classfile owner and method
name/descriptor come from ASM's method table. `metadata_association: unique`
means the supported Kotlin metadata signature matched exactly one method in
that same table. The helper uses the official `KotlinClassMetadata.readStrict`
API and accepts only metadata version `[2,3,0]`; other versions fail closed.

The initial declaration boundary is public ordinary, non-suspend instance
functions declared in public top-level Kotlin classes. Constructors,
overloads without unique signatures, facades, companions, objects, interfaces,
static methods, synthetic methods, bridges, and other unsupported shapes do
not produce verified rows. A method without a supported metadata association
is not presented as an associated Kotlin declaration. The command reports
metadata and method-table counts to make unsupported or unassociated content
visible.

The following remain `unknown`: source-to-binary association, current Gradle
task selection, current build freshness, compiler-input completeness, and
compiler-output ownership. This output does not establish that a class came
from the current source tree or build. It does not claim the artifact is
`ABI_TRUST_COMPLETE`.

## Bounds and limitations

The input class is limited to 16 MiB, the installed helper to 32 JARs and
64 MiB total, records to 4096, and JSON output to 2 MiB. The process timeout
is 15 seconds, JVM heap is capped at 128 MiB, and stack size at 1 MiB. These
limits bound common resource use; they are not a universal CPU or operating
system memory sandbox. Context cancellation stops the child process.

This diagnostic accepts one explicit classfile only. It does not ingest JARs,
scan repositories, discover dependencies, run a build, or affect the resolver.
