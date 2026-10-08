# Provenance

Generated Lua parser and scanner, copied unmodified from upstream.

- Upstream: https://github.com/tree-sitter-grammars/tree-sitter-lua
- Tag: `v0.3.0`
- Commit: `534c461d2b75b0887ec968ef9635f4460b0878b7` (2025-03-21)
- License: MIT, `LICENSE.md` copied verbatim ("Copyright (c) 2021 Munif Tanjim")
- Generator: tree-sitter CLI v0.25.3, `LANGUAGE_VERSION 14` (ABI 14), the
  highest ABI the `github.com/smacker/go-tree-sitter` runtime loads. v0.4.x
  ships ABI 15 and must not be vendored here.

| File | Upstream path | sha256 |
|------|---------------|--------|
| `parser.c` | `src/parser.c` | `f27bed6f236145bd89097a1d7fd26377c8202fc89e3f126ab4c8aef72849d99c` |
| `scanner.c` | `src/scanner.c` | `35bbd630b5a7421d46d2e91185eeea09bf78565d44cb676b63ca20d0f1b54bbd` |
| `tree_sitter/parser.h` | `src/tree_sitter/parser.h` | `11b654fbba7770b1c710cc14b169be2deb5d89c0f87233d4605037c26c1ff657` |
| `tree_sitter/alloc.h` | `src/tree_sitter/alloc.h` | `b29c1c9fb7cc82f58c84b376df1297d6e2737a1d655fd356db0859e3c29c2fea` |
| `LICENSE.md` | `LICENSE.md` | `9a32b02e4c917b1ce6b5e79d8ea81e25cefd7f27d89c7235f2afb262c06cf32e` |

`binding.go` and `binding_test.go` are CodeGraph's own. The parser is linked
against the go-tree-sitter runtime: the vendored `parser.h` is the v0.25
header, whose `TSLanguage` and `TSLexer` start with the ABI 14 layout and only
append fields the ABI 14 runtime never reads, and `parser.c` emits the ABI 14
`TSLexMode` table. The scanner uses plain `calloc`/`free`
(`TREE_SITTER_REUSE_ALLOCATOR` is not defined) and keeps all state in its
per-parser instance.

## Refresh

1. Clone the upstream into an empty directory and check out the exact commit
   of a release tag; do not run its build scripts.
2. Confirm `src/parser.c` has `#define LANGUAGE_VERSION 14`. A newer ABI needs
   a runtime migration first.
3. Copy the files in the table to these paths unmodified, update the table's
   tag, commit and sha256 values (`shasum -a 256`), and keep `LICENSE.md`
   verbatim.
4. Run `go test ./internal/parser/treesitter/... ./internal/indexer -run Lua`
   and bump the Lua parser profile in `../profiles.go` if any parsed output
   can change.
