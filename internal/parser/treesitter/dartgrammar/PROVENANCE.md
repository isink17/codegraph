# Provenance

Dart parser generated from the unmodified upstream grammar; scanner, headers
and license copied unmodified from upstream.

- Upstream: https://github.com/UserNobody14/tree-sitter-dart
- Commit: `8bd7e003770e2599ccf6b83a6a2a650ec71d73c2` (2026-07-06, "fix: enforce
  operator precedence via prec, not rule nesting (#102)"). Upstream has no
  release tags.
- License: MIT, root `LICENSE` copied verbatim ("Copyright (c) 2020-2023
  UserNobody14 and others"). Upstream `package.json` declares `"license":
  "ISC"`, which contradicts the license file; the license file is what is
  redistributed here. `tree_sitter/LICENSE` upstream covers the Dart pub
  bindings, which are not vendored.
- Generator: upstream publishes `src/parser.c` at this commit as ABI 15, which
  the `github.com/smacker/go-tree-sitter` runtime cannot load (it loads 13 and
  14). `parser.c` is therefore regenerated from the commit's unmodified
  `grammar.js` with tree-sitter CLI v0.25.10 (npm `tree-sitter-cli@0.25.10`,
  the CLI that generated the previously vendored parser) as
  `tree-sitter generate --abi 14`. The result is `LANGUAGE_VERSION 14`, its
  `src/grammar.json` and `src/node-types.json` are byte-identical to the ones
  upstream published at the commit, and two runs produce the same `parser.c`.
- Why this commit: it is the newest whose grammar parses Dart 3.8 null-aware
  elements (`[?x]`, `{?k: ?v}`), Dart 3.10 dot shorthands (`.red`, `.new()`,
  `const .new()`, `case .red:`), `get`/`set` as identifiers and labeled
  statements, while still parsing `List<int>.filled(3, 0)` as a constructor
  invocation. The next commit, `be07cf7` ("parse generic method invocation
  with single type argument", #104), parses every `Type<T>.name(...)` as the
  comparison `Type < T > .name(...)` with a dot shorthand, without an error.

| File | Upstream path | Bytes | sha256 |
|------|---------------|------:|--------|
| `parser.c` | generated, see above | 7135048 | `dbb3ac55a867235a428e4c6dfce5d5c52fb7d5f865e6499a03f6024ccf2c0c26` |
| `scanner.c` | `src/scanner.c` | 3839 | `07a7b7818b175e9460523e705dd88d20f7b5141bac95c593d4426e6d52284996` |
| `tree_sitter/parser.h` | `src/tree_sitter/parser.h` | 7624 | `180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2` |
| `tree_sitter/alloc.h` | `src/tree_sitter/alloc.h` | 985 | `b29c1c9fb7cc82f58c84b376df1297d6e2737a1d655fd356db0859e3c29c2fea` |
| `tree_sitter/array.h` | `src/tree_sitter/array.h` | 10431 | `5bdf6ed1a78e3409fd443e085ca967a64c188a5d082aaf7f819bccd53a471c94` |
| `LICENSE` | `LICENSE` | 1071 | `d270cb3a4985d75033bd77d875ccebff1d66e32788a3f727891e28d76132dd46` |

The headers are the ones the v0.25.10 generator writes; they are
byte-identical to the previously vendored ones. Upstream `grammar.js` at the
commit: 105947 bytes, sha256
`86da41913b5c84d3bbc508a8102e45448e06728698ddd20b5d77dbbe02a0f55e`.

`binding.go` and `binding_test.go` are CodeGraph's own. The parser is linked
against the go-tree-sitter runtime: the vendored `parser.h` is the v0.25
header, whose `TSLanguage` starts with the ABI 14 layout (`abi_version` is the
old `version` field) and only appends fields the ABI 14 runtime never reads,
and `parser.c` emits the ABI 14 `TSLexMode` table. The scanner is stateless
(it creates no payload and serializes nothing), so concurrent parses cannot
affect each other. go-tree-sitter's `SetLanguage` ignores an ABI it rejects,
after which every parse fails; `binding_test.go` therefore checks `ABI()` and
that a parse returns a tree. Only one `tree_sitter_dart` may be linked into a
binary: no dependency may bring a second Dart grammar (the go-tree-sitter
`dart` package included).

Known grammar gaps at this commit: a generic call or construction with one
type argument (`f<int>(x)`, `Route<void>(builder: ...)`) is sometimes parsed,
without an error, as a comparison, so it yields no call reference (as with
the previous commit).

## Refresh

1. Clone the upstream repository at the exact commit into an empty
   directory; do not run its build scripts.
2. If its `src/parser.c` has `#define LANGUAGE_VERSION 14`, copy it.
   Otherwise run `tree-sitter generate --abi 14` with a pinned tree-sitter
   CLI (v0.25.x writes the headers above), confirm `src/grammar.json` and
   `src/node-types.json` are unchanged from upstream, and that a second run
   writes the same `parser.c`. Never vendor an ABI 15 parser without a
   runtime migration.
3. Copy the files to these paths unmodified, update the commit, size and
   sha256 values (`shasum -a 256`), and keep `LICENSE` verbatim.
4. Parse a real Dart corpus with the old and new parser and compare the
   recorded symbols and references; check `Type<T>.name(...)` still parses as
   a constructor invocation.
5. Run `go test ./internal/parser/treesitter/... ./internal/indexer -run Dart`
   and bump the Dart parser profile in `../profiles.go` if any parsed output
   can change.
