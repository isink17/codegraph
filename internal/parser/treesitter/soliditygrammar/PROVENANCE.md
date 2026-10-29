# Provenance

This package contains only the generated parser, its required Tree-sitter
parser header, and the upstream license. It has no upstream Go binding and
adds no runtime dependency.

- Upstream: <https://github.com/JoranHonig/tree-sitter-solidity>
- Commit: `048fe686cb1fde267243739b8bdbec8fc3a55272`
- License: MIT, upstream `LICENSE` copied verbatim; copyright Joran Honig.
- Generator: `tree-sitter-cli 0.25.8`
- Command: `tree-sitter generate --abi 14`
- Runtime: `github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82` (unchanged).

To reproduce, clone the upstream repository into a temporary directory, check
out the exact commit above, install `tree-sitter-cli` version 0.25.8, then run
the exact command above from the repository root. Do not edit `grammar.js` or
generated files. The generator is needed only for regeneration, never for a
CodeGraph build. `binding_test.go` checks the generated parser, header and
license hashes during ordinary CGO tests.

| Packaged file | Upstream/generated source | SHA-256 |
| --- | --- | --- |
| `parser.c` | `src/parser.c`, regenerated as ABI 14 | `77120ebcf942b8890e889edabe8f050caffd6113f8acb9a33195e31b9c32ae15` |
| `tree_sitter/parser.h` | `src/tree_sitter/parser.h` | `180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2` |
| `LICENSE` | upstream `LICENSE` | `8844f0cc9b76b9c8a9d0251904eb7536b6bb9976e0ec577e8f27ab96d42523ef` |

ABI 14 generation changed only `parser.c`: it sets `LANGUAGE_VERSION` to 14,
uses the ABI 14 `TSLexMode` layout, and omits ABI 15-only `TSLanguage` fields.
The grammar source, node types, grammar JSON and parser header are not patched.
