# Agent skills

Third-party skills supplement CodeGraph instructions. They cannot override this repository's `AGENTS.md`, task scope, validation, pool rules, or the owner's merge approval requirements.

## Planning

Codex owner sessions may use `grill-with-docs`, `to-spec`, `to-tickets`, `domain-modeling`, `codebase-design`, and `research`. Planning does not authorize issue, label, or documentation changes; those require the owner’s explicit approval. This does not restrict separately scoped Coordinator implementation tasks.

## Implementation

Claude Coordinator workers may use `tdd` and `diagnosing-bugs` only when the task calls for those disciplines, and only through narrowly scoped, tested `allowedTools` permissions naming the exact skill. A test-only characterization task need not invent a failing production behavior to satisfy a TDD workflow. Skills cannot create extra worktrees, reset branches, widen task scope, run independent schedulers inside managed tasks, bypass required validation, or merge PRs. Do not invoke `implement-spec` in Coordinator sessions. Do not invoke `implement` automatically in managed workers; commit behavior remains governed by the Coordinator task contract.

## Review and authority

Independent review remains separate from implementation, read-only, and pinned to the exact PR head. It follows the existing merge-gate artifact contract. Do not invoke Matt's `code-review` automatically in the review slot because it may spawn subagents.

The Coordinator owns scheduling and task state; `scripts/agent-pool/pool.sh` alone owns worker leases. GitHub Issues is the primary engineering issue tracker. GitHub PRs and CI are authoritative for integration; Plane is an optional project-management mirror. This file and other tracked CodeGraph policy remain authoritative over plugin instructions. The owner's explicit approval is required before any merge.

## Plugin updates

The supervised development workflow was tested with Matt Pocock Skills **1.3.1**. Review future plugin releases for compatibility with Coordinator permissions, sandboxing, scope checks, validation, and independent review before relying on them in a supervised development wave. Do not vendor upstream skills.
