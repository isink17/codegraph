# Provenance

Generated Dart parser and scanner, copied unmodified from upstream.

- Upstream: https://github.com/UserNobody14/tree-sitter-dart
- Commit: `c8e7cbbd1589cc2ee1f9b5befa604dc7e953b0af` (2025-10-03, "Add support
  for extension type declarations (#83)"), the last upstream commit whose
  published `src/parser.c` is ABI 14. Upstream has no release tags.
- License: MIT, root `LICENSE` copied verbatim ("Copyright (c) 2020-2023
  UserNobody14 and others"). Upstream `package.json` declares `"license":
  "ISC"`, which contradicts the license file; the license file is what is
  redistributed here. `tree_sitter/LICENSE` upstream covers the Dart pub
  bindings, which are not vendored.
- Generator: tree-sitter CLI v0.25.10 (`parser.c` header), `LANGUAGE_VERSION 14`
  (ABI 14), the highest ABI the `github.com/smacker/go-tree-sitter` runtime
  loads. The commits after this one ship ABI 15 and must not be vendored here.
- Not regenerated: `parser.c` is the file upstream published at that commit.

| File | Upstream path | Bytes | sha256 |
|------|---------------|------:|--------|
| `parser.c` | `src/parser.c` | 6096704 | `911fdf2fb1901a85cdfd98a54d5c3da04b6862d927ce531d5b72f2d9a7ea772f` |
| `scanner.c` | `src/scanner.c` | 3839 | `07a7b7818b175e9460523e705dd88d20f7b5141bac95c593d4426e6d52284996` |
| `tree_sitter/parser.h` | `src/tree_sitter/parser.h` | 7624 | `180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2` |
| `tree_sitter/alloc.h` | `src/tree_sitter/alloc.h` | 985 | `b29c1c9fb7cc82f58c84b376df1297d6e2737a1d655fd356db0859e3c29c2fea` |
| `tree_sitter/array.h` | `src/tree_sitter/array.h` | 10431 | `5bdf6ed1a78e3409fd443e085ca967a64c188a5d082aaf7f819bccd53a471c94` |
| `LICENSE` | `LICENSE` | 1071 | `d270cb3a4985d75033bd77d875ccebff1d66e32788a3f727891e28d76132dd46` |

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

Known grammar gaps at this commit (they parse with an error): null-aware
collection elements (`[?x]`, Dart 3.8), dot shorthands (`.value`, Dart 3.10),
`get`/`set` used as identifiers, labeled statements.

## Refresh

1. Download the files in the table from the upstream repository at the exact
   commit into an empty directory; do not run its build scripts.
2. Confirm `src/parser.c` has `#define LANGUAGE_VERSION 14`. A newer ABI needs
   a runtime migration first.
3. Copy the files to these paths unmodified, update the commit, size and
   sha256 values (`shasum -a 256`), and keep `LICENSE` verbatim.
4. Run `go test ./internal/parser/treesitter/... ./internal/indexer -run Dart`
   and bump the Dart parser profile in `../profiles.go` if any parsed output
   can change.
