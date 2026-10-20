# Change-risk verification on real code

What `codegraph diff` (model `codegraph.change-risk/v1`, see
`docs/change-risk.md`) was checked against on pinned real repositories, what
held, and what did not. Reproduce with `scripts/change-risk-corpus/` (see its
README); the run below used Go 1.26.1 on darwin/arm64, binaries built from
`v2.0` at `c30415f` with the go-ast signature fix, with `CGO_ENABLED=1` (Go
parser profile `treesitter:go:v3`; byte-identical to the build without the
fix) and `CGO_ENABLED=0` (`go-ast:go:v4`). The earlier run at `97430a4` used
`go-ast:go:v3`, whose signatures were wrong; where its results differ they are
noted below.

The score is an evidence index, not a probability. Nothing below treats an
earlier CodeGraph output as ground truth: every label comes from the Go
toolchain run on both revisions, or from the construction of a controlled
edit checked by that toolchain.

## Corpus

| repository | URL | pinned commit | `go.mod` |
|---|---|---|---|
| uuid | https://github.com/google/uuid.git | `2d3c2a9cc518326daf99a383f07c4d3c44317e4d` | yes |
| errors | https://github.com/pkg/errors.git | `87f8819acf6dc28bf5d3c14b334268236d686f48` | no (GOPATH layout; the oracle copy gets one) |
| codegraph | https://github.com/isink17/codegraph.git | `355de90cef8758457afe2968e14d88332306987e` | yes |

32 cases (`scripts/change-risk-corpus/manifest.tsv`):

- 3 self diffs (each pin against itself).
- 18 real commits against their first parent: 7 uuid, 7 errors, 4 codegraph.
- 11 controlled edits, committed with a fixed identity and date so their SHAs
  repeat (verified across two runs for `uuid-remove-used` and `errors-move`):

| case | head commit | edit |
|---|---|---|
| uuid-move | `8a22eadb1747acfe2601badcd369f34b50fbc225` | `xtob` cut from `util.go`, pasted into `hash.go` (same header, other standard imports) |
| uuid-move-header-differs | `393c1d6648a2d8dfa9559e1c4d14d0b1b591b08a` | `xtob` moved into `uuid.go`, whose copyright line names another year |
| uuid-remove-used | `c6bcd787b586e5e7ff7b364571bcbb45a68cc10f` | `encodeHex` deleted, its three callers kept |
| uuid-rename-definition | `94caa0f09dc38d09ee0e50127f9e5e35a1371cc2` | `xtob` renamed at its definition only |
| uuid-rename-everywhere | `ae237c550e361cfa90d71504d1ecc4e473ddbbde` | `xtob` renamed at the definition and all six calls |
| uuid-add-parameter | `cd1bc9626623e336ddd7ecf627207a16ffee90cf` | `encodeHex` gains a parameter, callers unchanged |
| uuid-body-change | `c3919f446aa55952adc69e73af43e5f74fab3ddb` | `xtob` swaps its nibbles (body only) |
| errors-remove-used | `15cd7753cef278dd2bda8d4c55822ab4c5ecaa9e` | `callers` deleted, its five callers kept |
| errors-move | `afc742aabeb64aa5b92b4fc132aa993eb3710756` | `funcname` moved from `stack.go` to a new `funcname.go` |
| codegraph-move | `a3be77bceeccdc31e1be141f84cf96e8a2862f55` | `moduleRoots`, `goIgnoredDirectory` moved to `risk.go`, which gains `"path"` |
| codegraph-rebind | `c90957e47ee7152ea8e4d70648fa302328c95b57` | same move, but `risk.go` imports `path "path/filepath"`: compiles, `path.Dir` now means `filepath.Dir` |

## Per-case checks

For every case each binary ran twice (128 diff documents, all exit 0).

| check | result |
|---|---|
| Self diff is empty (no file, symbol, edge or test-link change, no pair or refusal) | 3/3, both builds. `CGO_ENABLED=0` on codegraph reports `limited_evidence` (go-ast is not call-capable), still with an empty change set. |
| Two runs byte-identical | 64/64 case-build pairs |
| Score traceable: per component `count` = evidence rows, `points` = min(count x points_each, max), score = min(sum, 100), assessment follows the documented rule, every row names declaration, dependent and site | 128/128 documents |
| Source checkout unchanged: HEAD, symbolic ref, `status --porcelain=v2 --untracked-files=all --ignored`, index entries, refs, stash, and no file in the work tree newer than the run start | 32/32 |
| CGO / `CGO_ENABLED=0` agreement | identical 29, differs 3 (the three codegraph controlled and self cases, in `assessment` or `uncertainty` only). With `go-ast:go:v3`: identical 11, same modulo signature text 15, differs 6 |
| Construction label confirmed by the toolchain | 11/11 controlled |
| `--fail-on-risk low` on three controlled cases | exit codes as documented, JSON byte-identical to the run without the gate. `uuid-add-parameter` rerun with `go-ast:go:v4`: exit 1 in both builds (with `go-ast:go:v3` `CGO_ENABLED=0` passed it with exit 0) |

Diff time per run (run at `97430a4`): 0.2 to 0.4 s for uuid and errors, 2.6 to 4.8 s for the four
codegraph history commits, 34 to 38 s (`CGO_ENABLED=1`) and 21 s
(`CGO_ENABLED=0`) for the codegraph pin, which indexes its whole tree twice.
No performance budget is defined for `diff`, so these are measurements only.

Fresh against incremental indexing does not apply: `diff` indexes each revision
into a fresh temporary database and never reads or updates the user's index.
The MCP `diff` tool shares the implementation; its document equality with the
CLI is covered by `TestDiffCLIAndMCPReturnSameDocument` on a fixture, not
rerun on this corpus.

## Results

`oracle` is `go build ./...`, `go vet ./...`, `go test ./...` exit codes on base
/ head (`-` not run: the codegraph suite is too long for an oracle; errors
tests fail on every revision because they expect a GOPATH source path).

| case | label | oracle base / head | `CGO_ENABLED=1` | `CGO_ENABLED=0` | pairs / refused |
|---|---|---|---|---|---|
| uuid-move | compiles | 0,0,0 / 0,0,0 | no_risk_evidence | no_risk_evidence | 1 / 0 |
| uuid-move-header-differs | compiles | 0,0,0 / 0,0,0 | risk 15 (retargeted 3) | risk 15 | 0 / 1 context_changed |
| uuid-remove-used | compile_break | 0,0,0 / 1,1,1 | risk 60 (removed 3) | risk 60 | 0 / 0 |
| uuid-rename-definition | compile_break | 0,0,0 / 1,1,1 | risk 60 (removed 3) | risk 60 | 0 / 0 |
| uuid-rename-everywhere | compiles | 0,0,0 / 0,0,0 | no_risk_evidence | no_risk_evidence | 0 / 0 |
| uuid-add-parameter | compile_break | 0,0,0 / 1,1,1 | risk 15 (retargeted 3) | risk 15 (`go-ast:go:v3`: **no_risk_evidence**) | 0 / 0 |
| uuid-body-change | behavior_break | 0,0,0 / 0,0,1 | no_risk_evidence | no_risk_evidence | 0 / 0 |
| errors-remove-used | compile_break | 0,0,1 / 1,1,1 | risk 60 (removed 5) | risk 60 | 0 / 0 |
| errors-move | compiles | 0,0,1 / 0,0,1 | risk 10 (retargeted 2) | risk 10 | 0 / 0 |
| codegraph-move | compiles | 0,0,- / 0,0,- | no_risk_evidence | limited_evidence | 2 / 0 |
| codegraph-rebind | rebind_compiles | 0,0,- / 0,0,- | risk 15 (retargeted 3) | risk 15 | 1 / 1 context_changed |
| uuid-2d3c2a9 | history | 0,0,0 / 0,0,0 | risk 5 (retargeted 1) | risk 5 (`go-ast:go:v3`: no_risk_evidence) | 0 / 0 |
| uuid-0e97ed3, -6e10cd1 | history | all 0 | no_risk_evidence | same | 0 / 0 |
| uuid-e8d82d3, -a2b2b32, -9ee7366, -b35aa6a | history | all 0 | limited_evidence | same | 0 / 0 |
| errors-49f8f61 | history | 0,0,1 / 0,0,1 | risk 40 (removed 2) | risk 40 | 0 / 0 |
| errors-ee1923e | history | 0,0,1 / 0,0,1 | risk 15 (retargeted 3) | risk 15 | 0 / 0 |
| errors-614d223 | history | 0,0,1 / 0,0,1 | limited_evidence | same | 0 / 0 |
| errors-004deef, -6d954f5, -7f95ac1, -e9933c1 | history | 0,0,1 / 0,0,1 | no_risk_evidence | same | 0 / 0 |
| codegraph-13fbb0f | history | 0,0,- / 0,0,- | limited_evidence | same | 0 / 3 context_changed |
| codegraph-60228c8 | history | 0,0,- / 0,0,- | limited_evidence | same | 0 / 2 context_changed |
| codegraph-89f3557 | history | 0,0,- / 0,0,- | risk 20 (retargeted 7) | risk 20 (`go-ast:go:v3`: risk 15, retargeted 3) | 0 / 3 (1 context_changed, 2 order_dependent) |
| codegraph-f2932b4 | history | 0,0,- / 0,0,- | limited_evidence | same | 0 / 1 context_changed |

### Confusion over independently labeled cases

Only the 11 controlled cases carry a defensible label. "Break" means the head
breaks a surviving dependent: the compiler rejects it (4), its tests fail
(`uuid-body-change`), or a moved declaration's identifier binds another
package (`codegraph-rebind`, by construction, compiles). "Same" means the
program means the same: a pure move or a complete rename (5). A prediction is
"flagged" when `assessment` is `risk_evidence`.

| | flagged | not flagged |
|---|---|---|
| break, `CGO_ENABLED=1` (6) | 5 | 1 (`uuid-body-change`) |
| break, `CGO_ENABLED=0` (6) | 5 (`go-ast:go:v3`: 4) | 1 (`uuid-body-change`; `go-ast:go:v3` also missed `uuid-add-parameter`) |
| same, both builds (5) | 2 (`uuid-move-header-differs`, `errors-move`) | 3 |

Compiler-verified breaks alone: 4/4 flagged with both builds (3/4 with
`CGO_ENABLED=0` and `go-ast:go:v3`). These counts come from 11 hand-built cases and are not a
precision or recall estimate for other code.

The 18 history commits have no independent label beyond "both revisions build
and vet", which does not prove a change safe. Their 4 flagged commits were
checked against the source: each cites a real change (a signature changed
with callers updated in uuid-2d3c2a9 and codegraph-89f3557, the `Frame` type
redefined in errors-ee1923e, `Cause` split into build-constrained files in
errors-49f8f61); none is a compile break.

## Known false negatives

- **Type parameter lists are not part of a Go signature.** Both parsers store
  a function's receiver, parameters and results as source text, without its
  type parameter list, so a change to a type parameter or its constraint alone
  (`func F[T any]` to `func F[T comparable]`) is not seen in either build. Not
  exercised by this corpus. (`go-ast:go:v3` missed every Go signature change
  with `CGO_ENABLED=0`: it stored `func encodeHex*ast.FuncType` before and
  after a parameter was added; `go-ast:go:v4` stores the source text, as the
  tree-sitter parser does.)
- **Body-only changes are not modeled.** `uuid-body-change` swaps two nibbles,
  breaks every parse, and scores 0. The rules score only resolved base edges
  whose targets were removed, retargeted or changed in visibility, staticness
  or arity; this is by design and documented in `docs/change-risk.md`.
- **Arity changes score as retargets with `CGO_ENABLED=1`.** A changed
  signature is a new declaration identity, so `uuid-add-parameter` cites
  `dependent_retargeted` (5 each), not `changed_declaration_dependent`; the
  break is flagged, but `low` (15) for a change that does not compile.

## Known false positives

- **A reformatted signature is a changed declaration.** The signature is source
  text, so rewrapping a parameter list or editing a comment inside it gives the
  declaration a new identity and its callers are cited as retargeted, in both
  builds. Not exercised by this corpus.

- **Moves refused by the conservative file-context rule.** A cut and paste is
  paired only when the two files' headers are byte-equal and every differing
  import binds a provable name. `uuid-move-header-differs` (copyright years
  differ) and `errors-move` (no `go.mod`, so no package identity) are not
  paired and cite their callers as retargeted (15 and 10). In the four
  codegraph history commits, 9 moved-declaration candidates were refused and
  none paired; as in the earlier campaign, those moves also edited the
  declarations or moved test-order-dependent declarations.
- **Build-constrained variants.** errors-49f8f61 moves `Cause` from
  `errors.go` into `cause.go` (`+build !go1.13`) and `go113.go`
  (`+build go1.13`). Head has two constrained declarations, the resolver
  binds neither, and the two test callers are cited as
  `removed_declaration_dependent` (40) although both revisions build and vet
  (Go 1.26, so the `go1.13` variant is the one compiled).

## Refused and unknown cases

- 11 refusals in all, each with its reason: 9 `declaration_context_changed`,
  2 `declaration_order_dependent`. No refusal was missing a reason, and no
  pair was produced in a case whose head misbinds (codegraph-rebind pairs
  `moduleRoots`, which spells no rebound name, and refuses
  `goIgnoredDirectory`, which spells `path`).
- `limited_evidence` (score 0 with uncertainty) in 8 history commits and, with
  `CGO_ENABLED=0`, in every codegraph case (go-ast is not call-capable).
- No case was `insufficient_evidence`.

## Acceptance (ticket criteria)

| criterion | status |
|---|---|
| Diff of a repo against itself is empty | implemented and validated (unit tests; 3 real self diffs, both builds) |
| Diffs byte-stable across runs | implemented and validated on one platform (64/64 pairs; unit tests diff every scenario twice). Linux and Windows not run. |
| No unexplained score: every component traceable to evidence rows | implemented and validated (128/128 documents) |
| User checkout never mutated (tested) | implemented and validated (`TestCompareCommitsExactTreeDeterminismAndCheckoutImmutability`; 32/32 corpus cases) |
| `codegraph diff` and MCP equivalent | implemented; MCP parity validated on a fixture only |
| Semantic projection (files, symbols, signatures, edges, test links) | implemented; signatures agree across builds (29/32 documents identical); type parameter lists are not stored (above) |
| Revision indexing in a temporary database per revision | implemented and validated |
| Change risk with documented inputs, component scores and evidence rows | implemented and validated for traceability; quality limits above |
| CI mode (JSON, exit threshold, reproducible) | implemented and validated (unit tests; 6 corpus runs) |
| Global matrix: determinism / lifecycle / performance / platform | determinism validated; lifecycle covered by unit tests only; performance measured without a budget; platform darwin/arm64 only |
| Benchmark diff on real corpora | done (this document) |

Recommendation: not ready for unconditional acceptance. The `CGO_ENABLED=0`
signature defect is fixed (`go-ast:go:v4`; every compiler-verified break is
flagged in both builds), but the platform matrix needs a Linux and a Windows
run once CI is available.
