# HCL grammar provenance

The CGO HCL adapter uses the parser already distributed by the pinned
`github.com/smacker/go-tree-sitter` dependency
(`v0.0.0-20240827094217-dd81d9e9be82`), package `hcl`. That dependency's
`_automation/grammars.json` records the source as
[`MichaHoffmann/tree-sitter-hcl`](https://github.com/MichaHoffmann/tree-sitter-hcl)
(now served from `tree-sitter-grammars/tree-sitter-hcl`), reference `main`,
revision `9e3ec9848f28d26845ba300fd73c740459b83e9b` ("feat: add namespaced
identifiers", committed 2024-06-24).

The pinned package holds `parser.c` (`LANGUAGE_VERSION 14`, 678 states, 121
symbols) and an external scanner `scanner.c` (heredoc templates). Both are
byte-identical to `src/parser.c` and `src/scanner.c` at that revision once the
upstream `#include "tree_sitter/parser.h"` is spelled `#include "parser.h"`,
the only edit the dependency's automation makes. It is the base HCL grammar,
not the upstream `dialects/terraform` variant; Terraform meaning is applied by
the adapter.

At that revision the repository's `LICENSE` file is the Apache License 2.0
(unfilled appendix, no named copyright holder), `Cargo.toml` declares
`license = "Apache"`, and `package.json` declares `"license": "ISC"`; there is
no `NOTICE` file. The license file governs; the `package.json` field is
recorded here as an upstream inconsistency, not resolved by this note. The Go
module's own MIT license names Maxim Sukharev and covers the bindings, not the
grammar.

CodeGraph imports the dependency package and does not copy or modify those
generated C sources. Redistributing a binary that links them carries the
Apache-2.0 notice obligation (section 4); the license text ships in
`THIRD_PARTY_NOTICES` with every release archive and the npm package.

## What the adapter records

`.tf` and `.tfvars` files have Terraform/OpenTofu meaning; every other `.hcl`
file gets only its top-level blocks (`hcl_block`, qualified as type and labels
joined by `.`) and attributes (`hcl_attribute`). `.tf.json` and `.tofu` files
are not indexed.

| Terraform block | Kind | Qualified name |
|---|---|---|
| `terraform {}` | `terraform_settings` | `terraform` |
| `provider "aws"` | `terraform_provider` | `provider.aws` |
| `resource "T" "N"` | `terraform_resource` | `T.N` |
| `data "T" "N"` | `terraform_data` | `data.T.N` |
| `variable "N"` | `terraform_variable` | `var.N` |
| `locals { N = ... }` | `terraform_local` | `local.N` |
| `output "N"` | `terraform_output` | `output.N` |
| `module "N"` | `terraform_module` | `module.N` |
| `.tfvars` `N = ...` | `terraform_variable_value` | `tfvars.N` |

A module's literal `source` is recorded as an import and in the module's
signature as a local path (`./`, `../`) or remote address; it is never fetched
or resolved to a directory.

A traversal rooted at `var`, `local`, `module`, `data` or any other name (a
resource type) becomes a `references` edge to its address (`var.N`,
`local.N`, `module.N`, `data.T.N`, `T.N`); a `.tfvars` assignment references
`var.N`. These are not references: roots `count`, `each`, `self`, `path`,
`terraform` and `ephemeral`; names bound by a `for` expression, a template
`%{ for }` directive or a `dynamic` block (its label or `iterator`); and the
`provider` and `providers` meta-arguments and lifecycle `ignore_changes`. A
splat or non-literal index after the address (`aws_instance.web[*]`,
`aws_instance.web[count.index]`, `local.m[var.k]`) marks the edge dynamic, and
a dynamic edge never binds: the instance a `count` or `for_each` index selects
is a plan-time value. A literal index (`aws_instance.web[0]`) binds.

A Terraform module is a directory. A reference binds only to the one
declaration of its address among the `.tf` files of its own directory. A
duplicate (including `override.tf` merges), a missing declaration, a
declaration in another directory, and every reference in a directory one of
whose `.tf` files was not parsed completely stay unresolved. A cleanly parsed
Terraform file is recorded as complete in `file_scope_evidence`; a file with a
syntax error records no references and no completeness row, and neither does
a failed or oversize parse or the non-CGO fallback. `module.M.out` binds to
the `module "M"` block of the same directory, never to the child module's
output. Hidden directories such as `.terraform/` are not scanned. Nothing
evaluates expressions or plans.

Terraform symbol names are short (`main`, `this`, `region`), so a bare-name
lookup can be ambiguous; query by address (`aws_vpc.main`, `var.region`).
`find_callers` on a declaration lists the blocks that reference it.

The non-CGO fallback (`heuristic:hcl:v1`) records only top-level declarations
of `.tf` files, under the same names, and no references or imports. The CGO
profile declares a relationship graph (its `references` edges), so an index
with Terraform files is not reported as symbols-only, and replacing it with
the fallback is refused as a downgrade.
