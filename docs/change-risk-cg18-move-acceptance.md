# CG-18 modified-content move acceptance

Task: `c28-cg18-move-continuity`. Production baseline:
`783d1222ecdbfb8fec6b54090f34678c58e6a368`.

No incorrect production result was reproduced on these pinned source trees.
The task adds acceptance coverage only; declaration continuity, ambiguity
refusal, uncertainty reporting and risk gate policy are unchanged.

`TestDiffMoveContinuityCLIAndMCP` in `internal/cli/diff_move_test.go`
creates two local commits from literal source strings. A fixed caller in
`example.com/app` calls `example.com/lib/p.A`. The library module moves from
`libs` to `lib` with identical `go.mod` bytes, while its source changes:

| Head source change | Declaration pairs | Risk evidence | Score / low gate |
|---|---|---|---|
| Add a doc comment to `A` | `libs/p/a.go` to `lib/p/a.go`, `p.A` | None | 0 / pass |
| Add sibling function `B` | Same pair for `p.A` | None | 0 / pass |
| Change `A`'s body | Refused: `declaration_content_changed` | One `dependent_retargeted` | 5 / fail |
| Add two competing destinations declaring `A` | Refused: `declaration_not_unique` | One `removed_declaration_dependent` | 20 / fail |
| Move the edited file into package directory `q` | No pair: import identity differs | One `removed_declaration_dependent` | 20 / fail |

The ambiguity fixture intentionally contains duplicate declarations: continuity
must refuse even when a parser can emit both declarations. The different-package
fixture intentionally leaves the original import in the caller, so the removed
declaration rule takes precedence over generic resolution loss.

Each case checks the CLI gate exit, score, component counts, pairing identity
and refusal where applicable, then compares the entire decoded public CLI JSON
document with the MCP `tools/call` result. Running with CGO enabled selects the
default tree-sitter adapter; `CGO_ENABLED=0` selects the Go AST fallback. Existing
`internal/diff` lifecycle, edited-move, import-binding and continuity-safety
tests cover the lower-level adapters and conservative eligibility boundaries.

Validation commands (run on macOS; Windows and Linux runtime behavior was not
executed for this task):

```sh
go test -timeout 20m ./internal/diff ./internal/cli
CGO_ENABLED=0 go test -timeout 20m ./internal/diff ./internal/cli
go vet ./internal/diff ./internal/cli
git diff --check
```

On this agent's macOS environment, put `/opt/homebrew/bin` first in `PATH`:
the Apple `/usr/bin/git` launcher emits denied xcrun cache writes into combined
fixture output, contaminating commit IDs before comparison. Homebrew Git avoids
that environment failure without modifying the fixture helper or product.
