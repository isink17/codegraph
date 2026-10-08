# Dart grammar provenance

The CGO Dart adapter parses with
[`UserNobody14/tree-sitter-dart`](https://github.com/UserNobody14/tree-sitter-dart)
commit `c8e7cbbd1589cc2ee1f9b5befa604dc7e953b0af`, MIT licensed ("Copyright
(c) 2020-2023 UserNobody14 and others"; upstream `package.json` says ISC, which
contradicts the license file). The generated `parser.c` (about 5.8 MiB),
`scanner.c` and headers are vendored unmodified in
`internal/parser/treesitter/dartgrammar` with the upstream `LICENSE` verbatim;
`PROVENANCE.md` there records the file hashes and the refresh steps. The
parser is ABI 14, the last ABI the `github.com/smacker/go-tree-sitter` runtime
loads; later upstream commits are ABI 15. Binary distributions must carry the
MIT notice in that `LICENSE`.

The adapter (profile `treesitter:dart:v1`) records:

- `import`, `export`, `part` and `part of` URIs as written, including the
  alternatives of a conditional import, with `as` prefixes, `show` names,
  `hide` names and `deferred` as import evidence. No URI is mapped to a file:
  `package:` URIs need `pubspec.yaml` and `package_config.json`, which are not
  read, and a Dart import is never cross-language bridge evidence.
- Classes (with any of `abstract`, `sealed`, `base`, `interface`, `final`,
  `mixin`), mixins, named extensions, extension types and their
  representation field, enums and their values, typedefs, top-level functions,
  getters, setters and variables, and members: methods, operators, getters,
  setters, fields and constructors. Constructors are `Class.Class` (unnamed,
  also `Class.new`) or `Class.name` (named, factory and redirecting factory).
  A setter's stable key ends in `=`.
- Call references: `f()`, `a.b.f()`, member calls named by the member alone
  when the receiver is not a plain dotted path, cascade sections, and
  `new`/`const` constructions.

It builds no call graph in any build (`codegraph doctor` lists Dart as
degraded): what a Dart call runs depends on the receiver's static type
(extension methods, cascades), mixin linearization, implicit `this`, library
privacy across `part` files and package resolution. Unnamed extensions,
local declarations and destructuring patterns are not recorded. A declaration
whose subtree holds a parse error is skipped with everything inside it; a type
whose error stays inside one member keeps its other members. The grammar does
not parse null-aware elements (`[?x]`, Dart 3.8) or dot shorthands (Dart 3.10),
so a declaration using them drops out.

Non-CGO builds use `heuristic:dart:v1`: type declarations, typed function
headers and directive URIs from regular expressions, without call references.
