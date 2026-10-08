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
notice must ship with binary distributions. It is reproduced, with the
notices of the other bundled tree-sitter grammars, in `THIRD_PARTY_NOTICES`,
which ships in every release archive and in the npm package.

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

Damaged Scala 3 indentation syntax often parses without an error, so the
same bound applies at the first member that clean, consistently indented
source never lays out that way:

- in a `:` body or an unbraced extension, a member starting a line at a
  column other than the body's first member, or no deeper than the line its
  owner starts on (a first member indented deeper than the rest, a deleted
  `:` on a nested type);
- at the top level, a member indented deeper than the type before it when
  that type has a `:` body, or when it has no body and the member is not a
  type (a deleted `:` on a top-level type);
- at the top level or in a package body, anything that is not a package
  clause, import, export, definition or comment (a misspelt `package`, a
  stray block);
- an `end X` marker that does not directly follow X in its scope, or is not
  aligned with the line X starts on (a member dedented out of the body the
  marker closes); `package a.b:` closes with `end b`.

A body that ends where the offending member starts is kept. Columns are
measured from a definition's modifiers or keyword, not from an annotation on
a line of its own. Braced bodies, members sharing a line with earlier code,
top-level members under a braced type, and type members under a bodyless
type (`sealed trait C` followed by indented `case object`s) are not checked,
so source that the grammar parses without an error and that is valid,
consistently indented Scala keeps every declaration. A top-level `def` or
`val` indented under a bodyless type (`case class P(x: Int)` then
`  def helper = 1`) is treated as a deleted `:`, and it and everything after
it are dropped; Scala 2 rejects that layout and Scala 3 reports it as
indented too far. Inconsistent indentation inside a `:` body
(mixed tabs and spaces, a member one column off) is treated as damage.

What remains: shifting a whole line by one full indentation step can
produce another valid program (a member dedented into the enclosing body,
or indented into the preceding `:` body, with no end marker to contradict
it), and nothing in the source then shows the damage; that member is
recorded under its new owner. A deleted `:` on a top-level type whose
members are themselves types (`class A\n  class B`) is not detected,
since braced Scala 2 may indent a type under a bodyless one.

## Call resolution

Only same-file local defs. Implicit and given scope, extension methods,
inheritance, overload resolution and `apply`/`unapply` desugaring decide what
any other Scala call runs, and source syntax alone does not prove them, so
those calls stay unresolved references with no edge.

The profile `treesitter:scala:v4` records local defs (a `def` statement in a
block, an indented block or a case clause, outside any local class, object,
trait, given or anonymous class) as `func:scala:local:<owner>.<name>:<line>:<col>`
and emits a call edge for a bare `f(...)` whose innermost local scope
declaring a def `f` declares exactly one, when:

- nothing in that whole scope spells `f` except the def's name, bare callees
  `f(...)` and selected members `x.f`; any parameter, lambda parameter,
  val, var, pattern or extractor binding, `for` generator, given, implicit,
  nested def, extension method, local object, class or type of that name,
  and any value use (`map(f)`, `f _`, `s"$f"`, a named argument `f = 1`,
  `f[T](...)`) refuses every call in the scope;
- the scope holds no `import` or `export`, wherever it is;
- a case clause counts as the scope only for a call in its body: its
  defs are not in scope in its pattern or guard (`case n if f(n) => def f...`
  looks for `f` further out);
- no class, object, trait, enum, extension, given, anonymous class
  (`new T { ... }`), quote or splice lies between the call and the scope,
  since a member or inherited member there could shadow the def.

A local def is the highest-precedence binding of the innermost scope that
declares it, so it shadows members, inherited members, explicit and wildcard
imports and package members of every enclosing scope in Scala 2 and Scala 3;
it cannot be overridden, and bare calls are not subject to implicit
conversions or extension-method lookup. The resolver
(`scala_local_function`, high) binds the edge to the local symbol at the
recorded position or leaves it unresolved; no repository-wide strategy
answers a Scala call.

A local def's stable key carries its line and column, as a Lua local
function's does, so editing lines above it changes its key: graph diffs and
symbol history show it as removed and added, while its call edges follow.
Local defs are never cross-language link ends and are never reported by
dead-code queries, since a call to one may simply not have been proven.

Only files that parse with no error, no layout damage (above) and no
expression-level damage get local defs or edges. Expression-level damage is
a hard keyword parsed as an identifier, an indented block whose line-leading
statements do not share one column deeper than their owner, or a def or val
body starting a line no deeper than its definition. The grammar also
produces these for some valid code (a multi-line `||` chain parsed as
postfix statements, `with` or `match` after a multi-line expression), whose
tree is wrong; those files lose local edges. Macros that rewrite a block
(`utest`'s `Tests { ... }`) are taken as written. Non-CGO builds use the
symbols-only `heuristic:scala:v1`.
