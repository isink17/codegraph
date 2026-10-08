# Scala grammar provenance

The CGO Scala adapter uses the parser already distributed by the pinned
`github.com/smacker/go-tree-sitter` dependency
(`v0.0.0-20240827094217-dd81d9e9be82`, package `scala`). No new module is
added. That dependency's `_automation/grammars.json` records the source as
[`tree-sitter/tree-sitter-scala`](https://github.com/tree-sitter/tree-sitter-scala),
tag `v0.22.5`, revision `d9017869dda79cefe2dc9d23c6125eda0a2d5d22` (tagged
2024-08-08). The tag object resolves to that exact commit.

Two different license files apply, and they cover different code:

- **The grammar.** `LICENSE` at the root of `tree-sitter/tree-sitter-scala`
  at revision `d9017869dda79cefe2dc9d23c6125eda0a2d5d22` is the MIT License,
  "Copyright (c) 2018 Max Brunsfeld and GitHub" (git blob
  `bd0a4d6c7d625591c54e1f023b93af2a4650271a`, sha256
  `1f95ed26e1f4074074c9c7083e61c0a9e4c3b9f435745044995f3beb4ed28575`), and
  that revision's `package.json` declares `"license": "MIT"`. This covers the
  generated `parser.c` and `scanner.c`.
- **The Go module.** `LICENSE` at the root of the `smacker/go-tree-sitter`
  module is the MIT License, "Copyright (c) 2019 Maxim Sukharev". It covers
  the module's Go bindings and updater, not the grammar. The module's `scala/`
  directory ships no license file of its own, so the grammar's notice is not
  present anywhere in the dependency as distributed.

The generated `parser.c` reports `LANGUAGE_VERSION 14`.

The distributed `parser.c` and `scanner.c` are byte-identical to the upstream
`src/parser.c` and `src/scanner.c` at that revision except for header include
paths (`tree_sitter/parser.h` -> `parser.h`, `tree_sitter/alloc.h` and
`tree_sitter/array.h` -> `../alloc.h`, `../array.h`), which the dependency's
updater rewrites for every grammar. CodeGraph imports the dependency package
and does not copy or modify those sources.

MIT requires the grammar's copyright and permission notice to accompany
copies of the software. A CodeGraph binary links the generated parser, so the
notice must ship with binary distributions; the repository has no
third-party notice file yet, an obligation shared with the other bundled
tree-sitter grammars.

## What the adapter extracts (V1)

Only `.scala` is detected. `.sc` (scala-cli/Ammonite scripts) is
intentionally not detected yet because SuperCollider uses the same extension,
although the grammar parses top-level statements. A `.scala` file is parsed
for:

- package clauses, chained (`package a.b` then `package c`) and with bodies
  (`package a { ... }`);
- imports, including comma-separated clauses, selectors, `{A => B}` and
  Scala 3 `{A as B}` renames, `_` and Scala 3 `*` wildcards, and `given`
  imports (recorded as `pkg.given`, not as a name wildcard); hidden
  selectors (`{A => _}`) and given-by-type selectors are not imports;
- classes (including case classes), objects (including companions and case
  objects), traits, enums, defs and abstract defs, vals and vars, type
  aliases, named givens and extension methods, nested at any depth inside
  member bodies;
- call references for `f(...)`, `a.b.f(...)` and `f[T](...)`.

Objects are keyed with a trailing `$` (`object:scala:a.Foo`,
`func:scala:a.Foo$.apply`), so a class and its companion keep distinct stable
keys while sharing the qualified name `a.Foo`. Overloads share one stable key,
as in the Java and Kotlin adapters; their signature and position tell them
apart.

Not recorded: local definitions inside methods and blocks, destructuring
vals, enum cases, auxiliary constructors, anonymous givens, infix and
parameterless member calls. In a file with a parse error, only declarations
and imports that end before the first error are recorded, and none at all
when the file has more `}` than `{` (a missing opening brace moves members
out of their type before any error shows). Packages nested inside a package body are not parsed by this grammar
revision; declarations under them are dropped rather than misplaced.

These rules only apply when the grammar reports an error. Broken source the
grammar parses cleanly is recorded as parsed: in Scala 3 indentation syntax a
deleted `:` that opens a body, or a first member indented deeper than the
rest, moves the following members to package level, and a misspelt `package`
keyword drops the package prefix.

## Call resolution

None. The profile `treesitter:scala:v2` declares no call edges: implicit and
given scope, extension methods, inheritance, overload resolution and
`apply`/`unapply` desugaring decide what a Scala call runs, and source syntax
alone does not prove any of them. Every call stays an unresolved reference.
Non-CGO builds use the symbols-only `heuristic:scala:v1`.
