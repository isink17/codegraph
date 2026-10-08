# Dart grammar provenance

The CGO Dart adapter parses with
[`UserNobody14/tree-sitter-dart`](https://github.com/UserNobody14/tree-sitter-dart)
commit `8bd7e003770e2599ccf6b83a6a2a650ec71d73c2`, MIT licensed ("Copyright
(c) 2020-2023 UserNobody14 and others"; upstream `package.json` says ISC, which
contradicts the license file). Upstream publishes that commit's parser as
ABI 15, which the `github.com/smacker/go-tree-sitter` runtime cannot load, so
`parser.c` (about 6.8 MiB) is generated from the unmodified upstream grammar
with tree-sitter CLI v0.25.10 at ABI 14; `scanner.c` and the headers are
vendored unmodified in `internal/parser/treesitter/dartgrammar` with the
upstream `LICENSE` verbatim. `PROVENANCE.md` there records the file hashes,
why this commit and not a later one, and the refresh steps. Its MIT notice
ships in `THIRD_PARTY_NOTICES` with every release archive and the npm package.

The adapter (profile `treesitter:dart:v3`) records:

- `import`, `export`, `part` and `part of` URIs as written (a `part of`
  library name is not a URI and is not recorded), including the
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
  A setter's stable key ends in `=`. Local functions of a file without a
  parse error, keyed `func:dart:local:name:line:col`.
- Call references: `f()`, `a.b.f()`, member calls named by the member alone
  when the receiver is not a plain dotted path, cascade sections, and
  `new`/`const` constructions.

Call edges exist only for a bare call `f(...)` that lexical scoping provably
binds to a function of the same file (strategy `dart_lexical_function`):

- a local function, when every other token `f` in the enclosing top-level
  declaration is a bare call and the call follows the declaration inside its
  block;
- a top-level function, when the file has no `part`/`part of` directive (the
  library is this one file), it is the only top-level declaration named `f`,
  `f` appears in no directive, top-level variable, typedef or type header, and
  every other token `f` in the enclosing top-level declaration is a bare call.
  Library declarations shadow imports, and an inherited member never shadows a
  lexical name, so neither can change the target.

Every other call stays an unresolved reference: what a qualified Dart call runs
depends on the receiver's static type (extension methods, cascades), mixin
linearization, implicit `this`, library privacy across `part` files and
package resolution. The proof is by spelling, so a name that is also a
parameter, variable, pattern, member, member access, named argument or
`show`/`hide` name anywhere in the caller's top-level declaration refuses even
where Dart would bind it. A file with a parse error builds no call edge. Unnamed
extensions, local variables and destructuring patterns are not recorded. In a file
with a parse error only the declarations that end before the first error (or
missing token) are recorded: text before it parsed without recovery, while
recovery after it may close a body early, swallow later declarations into an
unclosed one or read a misspelt keyword as another declaration. A type that
does not end before the error is dropped with all its members, and directives
follow the same rule. A broken file with more `}` than `{` tokens records no
declarations at all: recovery closes a body at an earlier `}` and moves the
members after it out to the top level before it reports the excess brace. Call
references are kept unless they sit inside an error node. Null-aware
elements (`[?x]`, `{?k: ?v}`, Dart 3.8), dot shorthands (`.red`, `.new()`,
`case .red:`, Dart 3.10), `get`/`set` as identifiers and labeled statements
parse. A dot shorthand names no type, so a shorthand call (`.make()`,
`.new()`, `const .new()`) yields no call reference. A generic call with one
type argument (`f<int>(x)`) is sometimes parsed as a comparison and then
yields no call reference.

Non-CGO builds use `heuristic:dart:v1`: type declarations, typed function
headers and directive URIs from regular expressions, without call references.
Its comment stripper does not nest block comments (Dart's `/* /* */ */`
does), and it can misread multi-line `'''` strings, so text there may yield
spurious symbols.
