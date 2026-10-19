# Change risk (`codegraph diff`)

`codegraph diff <base> <head>` and the MCP `diff` tool report `diff.risk`, an
evidence-backed change-risk assessment computed from the same semantic diff the
rest of the document shows. CLI and MCP share one implementation, so both
return the same `risk` for the same two commits.

The risk always covers the whole change set: `--limit` and `--offset` page the
change lists but never change `risk`.

## What it is and is not

- The score is an **ordinal evidence index** (`score_kind:
  evidence_index_not_probability`), not a calibrated failure probability.
- Every point needs a resolved edge in the base graph between two declarations;
  the head side is then judged by the names the dependent references, resolved
  or not. File churn, symbol counts, Git history and unresolved references
  without such a base edge never add points.
- No assessment proves a change safe. Only resolved edges are cited, so an
  unresolved, dynamic, reflective or cross-language dependent can still break.
  `risk.coverage` is therefore never `complete`.

## Assessments

| `assessment` | Meaning | `score` | `level` |
|---|---|---|---|
| `risk_evidence` | At least one rule cites a dependent relationship. | 1..100 | `low`, `medium` or `high` |
| `limited_evidence` | No rule fired, but `uncertainty` lists what the evidence cannot rule out. | 0 | `none` |
| `no_risk_evidence` | No rule fired and nothing in the change is uncertain. Still not proof of safety. | 0 | `none` |
| `insufficient_evidence` | Declarations or edges were not compared (indexing policy, schema, parser or resolver semantics differ, a pending transition, dirty or failed files). | `null` | `unknown` |

Levels: `low` from 1, `medium` from 25, `high` from 50.

## Rules (model `codegraph.change-risk/v1`)

A dependent relationship is one (dependent declaration, depended-on declaration)
pair taken from the resolved base edges, where the dependent still exists in
head. A dependent exists in head when its full identity (path, language, kind,
qualified name, signature and stable key) is unchanged, when `declaration_pairs`
proves it moved with a renamed file, or when its signature changed and, among
the declarations with its path, language, kind and qualified name that have no
counterpart on the other side, it is the only one in base and one is left in
head; the last two are cited with `dependent_head`. With no such head
declaration the dependent was removed. This is pairing by elimination: it holds
only when exactly one such declaration is left on each side. Any other count
(two overloads whose signatures both changed, two in base against one in head,
one in base against two in head) is not scored and is listed as
`dependent_identity_ambiguous`; if no
other surviving dependent cites the removal it depended on, that removal shows
as uncertainty too. A dependent that is gone from head is not scored. Several
call sites between the same pair are one relationship, and each relationship is
cited by at most one rule.

A relationship is judged by what the same dependent references **by the same
name** in head (the last segment of the reference: `p.T.M`, `obj.M`, `ns::M`
are all `M`), not by the call site alone. A call site is a line and column, and
an edit that shifts lines can land an unrelated call on it; such coincidences
appear in `edges.retargeted` but are not cited as risk. A head reference that
resolves to another declaration the dependent already used in base (a sibling)
belongs to that other relationship. When the depended-on declaration was
removed, sibling references beyond the sibling's own base sites count as a
rebinding, so removing one overload whose callers fall back to a sibling
overload is cited. No more sibling references than before cannot tell a dropped
call from a dropped sibling call plus a rebinding; that is
`dependent_rebinding_unproven`, not points.

| Rule | `change` | Cites | Points each | Cap |
|---|---|---|---|---|
| `removed_declaration_dependent` | `declaration_removed` | The declaration was removed and the dependent still references its name without resolving it. | 20 | 60 |
| `dependent_resolution_lost` | `resolution_lost` | The declaration still exists, but the dependent's references by that name no longer resolve. | 10 | 30 |
| `dependent_retargeted` | `binding_changed` | The dependent's references by that name now resolve to a different declaration and none to the original (or to the declaration `declaration_pairs` proves is the same). | 5 | 20 |
| `changed_declaration_dependent` | `declaration_changed` | A declaration kept its identity but changed visibility, staticness or arity while other declarations still resolve to it in head. | 5 | 20 |

A dependent that no longer references the name at all (its caller was updated)
is not cited. A removed declaration without any resolved base edge from a
surviving declaration is listed under `uncertainty`. A depended-on declaration
whose signature changed is a removal plus an addition: callers that now
resolve to the new signature are cited as `dependent_retargeted`, even when
only the spelling changed (`interface{}` to `any`). `declaration_pairs` covers
Go only: exact renames inside one directory, exact renames across directories
when the indexed module evidence gives both files the same module path, import
path, package clause, test role and build constraint (a module root moved with
its packages), and single declarations moved between edited files of one
package when their own bytes and their file's imports are unchanged (see
below). Declarations of any other move, a declaration whose content changed,
and declarations outside Go are not paired: callers of such a move are cited as
`dependent_retargeted`, and a moved dependent is not scored.

### Go package identity

Each Go file's index records its package clause and its `//go:build` (or
combined `// +build`) constraint, and each completed scan records every `go.mod`
and `go.work` of the repository. A file's import path is the module path of the
nearest `go.mod` at or above it plus its directory relative to that module
root. Since an import path names a directory, two files in different
directories share one only when the module root moved with them and still
declares the same module path. A cross-directory rename pairs only then, and
only when the bytes of that module's `go.mod` are unchanged (a same-directory
rename instead compares every `go.mod` and `go.work` in its directory and its
parents). It is refused otherwise:

| `reason` | When |
|---|---|
| `module_evidence_unavailable` | Either side has no recorded module evidence (an index written before it was recorded), a file has no recorded package identity, a file is outside every module or under a `go.mod` without a module path, or the module's `go.mod` is missing from a revision's tree. |
| `workspace_uncertain` | In either revision a `go.work` lists the file's module in `use`, or a `go.work` cannot be read or parsed, or one of its `use` paths is absolute. A module no `go.work` uses keeps its import identity; a `go.work` added, removed or edited between the revisions is judged by what each revision's `go.work` uses. |
| `module_replace_in_repo` | In either revision the file's module declares a replace by a directory path (to a directory inside or outside the repository), or is the target directory of such a replace in another `go.mod` or a `go.work`, or a `go.mod` with such a replace cannot be read or parsed, or a replace path is absolute. Modules a replace does not involve keep their import identity. |
| `module_path_not_unique` | Two module roots declare the file's module path. |
| `module_identity_changed` | The import paths or the module paths differ: a different package, even with the same package clause and names, or the same import path now owned by another module. |
| `module_root_changed` | The `go.mod` of the file's module differs in bytes between the two revisions. |
| `package_identity_changed` | The package clause or the external test role (`_test` package in a `_test.go` file) differs. |
| `build_role_changed` | The build constraint or the file name's build role differs. |
| `package_ignored` | A path is under `testdata`, `vendor`, or a directory starting with `_` or `.`. |

A Go file removed from one directory and added under another with the same
file name and package identity but different bytes is listed under `refused`
as `content_changed`: the file is not a rename. When several files fit, or the
bytes are shared by other moved files, each candidate is `move_ambiguous`. Its
declarations are then judged one by one, as below.

### Edited moves

A declaration can also be paired when it leaves a removed Go file for an added
Go file of the same package identity (module path, import path, package
clause, external test role, build constraint, file name build role and parser
semantics; the directories may be equal) and nothing it means can differ. The
diff reads both files from Git and parses them again; the declaration is the
whole top-level declaration that holds it (for a constant in a `const ( ...
iota ... )` group, the group). It pairs only when:

- its module's import identity holds as for a cross-directory rename (no
  `go.work` uses it, no replace involves it, its module's `go.mod` bytes are
  unchanged),
- its identity without path and position (kind, qualified name with receiver,
  signature, stable key, visibility, staticness, arity) is unique among all
  declarations of that package identity in each revision,
- it does not run in file order (see [Shared eligibility](#shared-eligibility)):
  the files differ, so the declaration's place among them is not kept,
- its own bytes are equal, and so are the compiler directives (`//go:`,
  `//line`, `//export`, `//extern`) before it; its doc comment and its position
  may change,
- its file's package clause, imports (name and path, in any order), every
  comment inside or on an import declaration (the cgo preamble, in or outside a
  group), header comments, `//go:linkname` and `//go:cgo_*` directives wherever
  they are, and directives that precede no declaration or an import are equal.

A declaration found on both sides that fails one of these is listed under
`refused` with the base declaration and the reason; one that exists on one side
only is not a move candidate and is not listed:

| `reason` | When |
|---|---|
| `declaration_order_dependent` | The declaration runs in file order (see below). |
| `declaration_embed_unproven` | A `//go:embed` before or inside the declaration cannot be shown to embed the same bytes (see below). |
| `declaration_not_unique` | The identity occurs more than once in the package on either side. |
| `declaration_content_unavailable` | A file could not be read back from Git, its bytes differ from the indexed ones, it does not parse, or the declaration does not map to exactly one top-level declaration that shares no line with another. |
| `declaration_context_changed` | The file's package clause, imports, import comments, header comments, linkname or cgo directives, or unattached directives differ. |
| `declaration_content_changed` | The declaration's bytes or the directives before it differ. |

The module refusals of the table above (`workspace_uncertain`,
`module_replace_in_repo`, `module_root_changed`, `module_evidence_unavailable`)
apply per declaration too.

### Shared eligibility

Every pairing path (exact rename in or across directories, edited move) also
refuses a declaration whose own bytes cannot show that it means the same thing
in its new file. These rows carry the base declaration:

- `declaration_order_dependent`: a receiverless `init` function or a `var`
  declaration with initializer values (for a grouped `var ( ... )`, any spec
  with values) runs during package initialization in file order, and a `Test`,
  `Benchmark`, `Fuzz` or `Example` function of a `_test.go` file (the name
  followed by nothing or by a character that is not a lower-case letter;
  `TestMain` excluded) runs under `go test` in file order. The go command lists
  a package's files sorted by name, test files after the others. An exact
  rename keeps that order when every other `.go` file of the same test role in
  the old directory is in the new directory, on the same side of the file name,
  and no other `.go` file joined it (`a.go` to `b.go` next to `m.go` keeps it,
  `a.go` to `z.go` does not); then such declarations pair. An edited move never
  keeps it. Constants, types, methods, other functions and variables without
  initializer pair regardless of order.
- `declaration_embed_unproven`: a `//go:embed` pattern resolves against the
  source file's directory, so identical bytes can embed different content after
  a move. A declaration with `//go:embed` pairs only when every pattern names
  one file (no glob, no `all:`, no quoted name, a clean relative path) that, in
  both revisions, is a regular file readable from Git, not a directory, not
  under a nested `go.mod`, and has equal bytes. A missing, unreadable or
  changed file refuses; so does every pattern form not listed.

Both refusals read the file back from Git; when an exact rename's file cannot
be read or parsed, or a declaration shares a line with another top-level
declaration, its declarations are refused as `declaration_content_unavailable`.

`score` is the sum of component points, clamped to 100. Each component lists
`points_each`, `max_points`, `count`, `points` and its `evidence` rows:
`change`, `declaration` and `dependent` (declaration identities as in the symbol
sections), `file` (the dependent's file), `sites` (base edge identities as in
the edge sections) and, where head references exist, `head_sites`,
`head_states` and `head_targets`.

`removed_declaration_dependents` keeps listing every resolved base edge into a
removed declaration with the head edge at the same site; it is the raw evidence
and is not scored.

### Model version

The rule table (rules, `points_each`, `max_points`) and what each rule cites are
fixed per model id. `codegraph.change-risk/v1` has not shipped in a release, so
the dependent pairing and sibling counting above are part of v1. After a
release, any change to what a rule cites or scores needs a new model id.

Cross-directory Go pairs, edited moves, the scoped `go.work` and replace
checks, and the shared eligibility refusals keep the model at v1. Every rule is defined in terms of
`declaration_pairs` (a paired declaration is the same declaration, a paired
dependent survives), and those definitions are unchanged. An edited-move pair
asserts the same thing an exact rename does, for one declaration instead of a
whole file: the same package and the same bytes, read in the same file scope
(imports, cgo preamble, directives). An exact rename never proved the rest of
the package unchanged either. A new pair only removes citations and
uncertainty rows of a move whose identity the evidence proves (a caller's
binding to the moved declaration is no longer `dependent_retargeted`, the moved
declaration no longer `removed_declaration_without_resolved_dependents`); it
never adds points. A move without that proof, including a moved declaration
whose content changed, is cited exactly as before. The shared eligibility
refusals only withdraw pairs, so they can only restore citations a pair would
have removed.

## Uncertainty

`uncertainty` rows add no points; any row turns a zero score into
`limited_evidence`.

| `reason` | Meaning |
|---|---|
| `removed_declaration_without_resolved_dependents` | A removed declaration had no resolved base edge from a declaration that survives. Zero resolved callers is not evidence of zero callers. |
| `dependent_site_ambiguous` | Several head edges share the base edge's site. |
| `dependent_identity_ambiguous` | A dependent's identity changed and several declarations under its path, language, kind and qualified name are left without a counterpart, so it cannot be paired. |
| `dependent_rebinding_unproven` | A dependent of a removed declaration references a sibling it already used, no more often than in base (`identity` is the relationship, `detail` the sibling). |
| `dependent_binding_partial` | A dependent still resolves to the declaration, but at fewer references than base sites, next to unresolved or other-target references by the same name. |
| `declaration_ambiguous`, `edge_ambiguous` | The diff could not match identities one to one. |
| `symbols_coverage_partial`, `edges_coverage_partial` | Some files were not compared, call capability is limited, or call columns are missing. |
| `changed_file_without_semantic_coverage` | A changed file has no recognized language, so nothing in it is in the graph. |
| `changed_file_jvm_scope_unknown` | A changed Java, Kotlin or Scala file while the JVM compilation scope is unknown. |
| `comparison_limitation` | Any other limitation from `comparison.limitations`. |

## CI gate

The gate is opt-in and runs in the local binary; it needs no hosted service.

```bash
codegraph diff origin/v2.0 HEAD --fail-on-risk medium
codegraph diff origin/v2.0 HEAD --fail-on-risk 20 --insufficient-evidence pass
```

- `--fail-on-risk VALUE`: a score `1..100`, or `low` (1), `medium` (25),
  `high` (50). The command fails when `score >= VALUE`.
- `--insufficient-evidence fail|pass`: what an `insufficient_evidence`
  assessment does; `fail` by default. Only valid with `--fail-on-risk`.

The full JSON document is printed first and is byte-identical to the ungated
output; one summary line goes to stderr, for example
`diff risk: fail, assessment risk_evidence, score 20 (low), threshold 1, 0 uncertainty rows, comparison limited`.

| Exit | With `--fail-on-risk` |
|---|---|
| 0 | Score below the threshold, or insufficient evidence with `--insufficient-evidence pass`. |
| 1 | Score at or above the threshold. |
| 2 | Invalid input (unknown, repeated or out-of-range flag, wrong arguments) or a failed comparison; no document is printed. |
| 3 | Insufficient evidence with the default `--insufficient-evidence fail`. |

Without `--fail-on-risk`, risk never changes the exit code: 0 on success, 1 on
any error, as before. `--insufficient-evidence` without `--fail-on-risk` is an
error.

The gate decides on `score` only. `limited_evidence` passes when its score is
below the threshold; read `uncertainty` (its count is in the summary line) to
decide whether that is enough for your pipeline.
