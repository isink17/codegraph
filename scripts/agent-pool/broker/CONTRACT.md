# O5 PR-1 protocol core: contract note

The normative contract is O5-DESIGN.r4 with addendum A1 and correction R5-1
(design set `cg150-o5-design-20261110`, independent review PASS, PR-1 blockers 0).
This note records only what PR-1 had to make precise. It changes no owner
decision.

## What PR-1 is

Pure, stdlib-only decision logic plus in-memory stores and an adapter over the
unmodified `recovery_state.py`. Nothing outside the tests imports `broker`.
It installs, enables and deploys nothing; it performs no file, network or
process I/O; it has no root, sudo or live-pool path. The in-memory stores
prove ordering and the write barrier, not filesystem durability. PR-4's real
stores must implement the same interfaces with temp file, `F_FULLFSYNC`,
`renameatx_np(RENAME_EXCL)` and directory sync, and must pass the same tests.

## Module map (r4 §15, with four additions)

| Module | Contract |
|---|---|
| `canonical.py` | D, CANON (r3 §7.2); `serialize` is for store snapshots only |
| `protocol.py` | request parser (r3 §7.1), `parse_doc` |
| `sanitize.py` | `esc`, `render` (r3 §7.3, N3, R5-1 §R4.3) |
| `refusal.py` | closed enum, outcomes (r4 §9, A1 §A5) |
| `opclass.py` | the r4 §6.4 table; delegated rows always refuse |
| `provenance.py` | types and rules; decisive constructor `_produce` is producer-only |
| `authz.py` | principals, `trust_of`, documents, confirm, revalidate |
| `authz_txn.py` | `CommitTxn`; `_issue` is referenced only by `authz.revalidate` (split out to avoid an import cycle) |
| `custody.py` | interface, in-memory fake, keys, receipts, k1–k8 |
| `libview.py` | E-view (r4 §8.5) and the L component |
| `ledger.py` | log format, R5-1 classifier, plan, repair, restore predicate |
| `anchor.py` | the A component |
| `lifecycle.py` | rows, K table, `decide`, `completion_mode`, custody rows |
| `stores.py` (added) | write gating shared by all stores (A1 §A2.3), small fakes |
| `library.py` (added) | adapter over the unmodified library; reads use the library's own readers |
| `invocation.py` (added) | phases V, P, C, R, W of one owner invocation |
| `_testsupport/` | test-only capability and evidence producers (LB5) |
| `fdwalk.py` (PR-2) | descriptor-relative, no-follow, bounded reads |
| `gitraw.py` (PR-2) | strict-subset config, HEAD, refs, loose commit and tree objects |
| `indexverify.py` (PR-2) | index parser, worktree comparison, `observe`, result model |

## Clarifications made in PR-1

1. **Synthetic owner (r4 §4.1).** r4 derives `same-uid-attestation` from a
   "capability-less synthetic context". PR-1 names that context `SyntheticOwner`.
   It satisfies the owner check only with a synthetic store, and custody
   retrieval only from a synthetic custody store. With the protected store it
   refuses (`OWNER_REQUIRED`, `TRUST_INSUFFICIENT`). `owner-authenticated` is
   still produced only with an `OwnerCapability`, which only `_testsupport` can
   construct in PR-1.
2. **Custody record nulls.** D has no null, so an unbound record stores
   `bound = []` and `terminal_ref = {}` (r4 §8.1 writes `null`).
3. **Latest operation (review N-7).** A slot's latest operation is the newest
   committed `adopt` burn for that slot whose library operation exists.
4. **Resume with nothing to resume.** `resume-step` on a readable slot with no
   library pending entry, no marker-less terminal state, no anchor `pending` and
   no F intent is `ALREADY_COMPLETE` with no write (A1 §A3.3, "a second resume
   after completion"). Like every `ALREADY_COMPLETE` shortcut, it is decided only
   after the A1 §A2.6 step 4–5 checks (reads, §6.1 and K consistency) pass.
5. **Resume of a quarantine checks the receipt.** Besides `check_digest` and
   the verifier, RESUME requires the original document's
   `custody_receipt_digest` to equal custody's receipt (A1 §A3.3 wrong token,
   including pre mode).
6. **`binding_digest`** = `digest({verb, slot, op_id, slot_generation})` of the
   document; I-NOREMINT looks up `verb = quarantine` at `g_pre`.
7. **Replacement documents (A1 §A3.2).** Besides the fields A3.2 lists and
   `repair_plan` (N5-14), `tierp_state_digest` and `anchor_digest` may differ: they
   bind the phase V snapshot, which every extra burn changes. A `ready`
   document's `preservation` field follows the run's own preservation gate.
8. **RS-1 refusal before display.** Until RS-1, an `owner-authenticated`
   `request`, `cancel` or `quarantine` refuses `TRUST_INSUFFICIENT` in phase V
   (the r4 §8.2 replica of the library's trust check), so nothing is burned.

9. **Restore needs an LF-terminated current file (A1 §A4.7).** Condition 3
   counts only LF-terminated lines, so a committed `TAIL_UNTERMINATED_VALID`
   entry would escape condition 2. `restore_acceptable` therefore refuses
   whenever the current file has any byte after its last LF. In PR-1 this makes
   every `UNCLASSIFIABLE` file with a non-empty tail unrestorable, including a
   self-valid unterminated final entry and A4.7's complete-but-invalid final
   segment. No in-band path terminates such a file, because `plan()` refuses an
   `UNCLASSIFIABLE` log. An LF repair followed by a re-run of the predicate
   would belong to a separately authorized PR-4 out-of-band runbook. A file that
   ends in LF is compared line by line as before.
10. **I-ORPHAN fails closed on unreadable sources (A1 §A4.9).** Every reference
    source is read for every slot. Any of the following refuses every non-read
    owner verb, `stop` included (`READ_ONLY` verbs skip phase V and still answer):
    - an anchor log that is `UNCLASSIFIABLE` or has no entries: `STATE_UNKNOWN`;
    - a listed library operation that cannot be read: `STATE_UNKNOWN` or `BUSY`;
    - a custody record or tombstone that fails lookup's checks: `CUSTODY_CORRUPT`.

    The slot set includes every slot that has an F WAL. A skipped source could pin
    a burn that a phase W trim would then remove, or hide a deleted burn. No
    narrower exception is claimed. PR-4 must enumerate slots from protected
    configuration, so that a slot cannot leave the set by losing its Tier C
    record and its anchor log.

    The refusal is global: an unseeded slot (a Tier C record or F WAL without
    an anchor entry) or a `BUSY` operation on any slot blocks every non-read
    verb, where r4 refused only that slot. The PR that adds `migrate` must order
    the anchor seed before a slot can be enumerated, or define a reviewed
    exception, or migration itself will be refused.

## Review N5 dispositions

| Item | Disposition |
|---|---|
| N5-1 | Plan values per action: `trim` (offset = body, len = tail, sha256 = tail); `terminate` (offset = body + core, len = trailing NULs, sha256 = core); `record` (offset = body, len = 0, sha256 = sha256 of empty). `test_n5_1_plan_values_per_action` |
| N5-2 | The burn rule applies in steps 3, 8c and 9: a complete or prefix entry that repeats a committed `auth_digest` or `nonce_digest` is invalid. `test_n5_2_burn_rule_applies_to_complete_entries`, V10e |
| N5-3 | Both step-11 evidence checks are implemented: (a) an evidence item's bytes must match its `(offset, sha256, len)` name; (b) every `repair` item referenced by a committed entry must exist. Either failure is `UNCLASSIFIABLE(EVIDENCE_INVALID)`, so every owner verb refuses `LEDGER_UNAVAILABLE` with no write. Tests: `test_n5_3_*`, `test_n5_3_evidence_checks_refuse_every_owner_verb` |
| N5-4 | Any lexer exception counts as no value; deep `[` and `{"a":` nests are `UNCLASSIFIABLE`. `test_n5_4_lexer_is_total` |
| N5-5 | Anchor `W` = `acquire`, `clear-suspect`, `mark-suspect`, `migrate`, `quarantine`, `release`. `migrate` is the seed entry's verb; `complete`/`abort` carry the verb of the pending entry they resolve (enforced by `anchor.fold`) |
| N5-6 | Evidence is written only inside the CommitTxn-gated log append; snapshots include evidence |
| N5-7 | No `trim` plan when 16 evidence items are pending: `LEDGER_UNAVAILABLE` in V |
| N5-14 | The comparator names `repair_plan` as replaceable (item 7 above) |

## Packaging gate for PR-4 (not implemented here)

Underscore names and the AST tests in `test_static.py` are development checks.
They are not an OS security boundary. Before any privileged install, PR-4 must
prove on the installed artifact that none of the following is present in, or
can be loaded by, the privileged broker:

- `_testsupport` (PR-1 has no bundle manifest; the exclusion its docstring
  names is still to be built);
- `SyntheticOwner` and the synthetic store kind, which PR-1 defines in
  `authz.py`, so PR-4 must move them out of the installed modules rather than
  only omit `_testsupport`;
- `authz._new_capability` and `provenance._produce` reachable from anything
  but the production entry points;
- any development-only constructor for `OwnerCapability`, decisive provenance
  or `CommitTxn`.

The proof must cover the installed file set, the import path, environment
variables and writable directories.

## Read-only Git observer (PR-2)

`indexverify.observe(Context)` reports one worktree's state from reads only. It
is not called by anything outside its tests.

**Roots.** The caller names three directories: the worktree (A), the Git
administrative directory (B) and the common directory (C). For an ordinary
repository B is `A/.git` and C is B. Each root must be an absolute, normalized
path and is opened one component at a time from `/` with `O_NOFOLLOW`. A
symbolic link anywhere in it gives UNKNOWN. Repository pointers select nothing:
they are compared, never followed.
- Ordinary: `A/.git` must be a directory with B's device and inode, and B has
  no `commondir`.
- Linked: `A/.git` must be the regular file `gitdir: <B>\n` with B written
  exactly; `B/commondir` must name C (absolute, or relative to B); `B/gitdir`
  must be exactly `<A>/.git\n`. Anything else, including a relative `.git`
  link, is IDENTITY_CHANGED, and the named location is never opened.

**Reads.** Every open is relative to an open directory descriptor, with
`O_NOFOLLOW`, `O_NONBLOCK` for files, and no write flag. FIFOs, sockets and
devices anywhere in the worktree give UNKNOWN without being opened. A regular
file is read only if it has one link and lies on its root's device; its
metadata (kind, mode, device, inode, links, size, mtime, ctime) must be equal
before the open, after the open and after the last byte. All sizes, counts,
depths and the zlib output are bounded (`fdwalk.Limits`); exceeding one is
UNKNOWN. In particular, a name component is at most 255 bytes, so a long
non-ASCII name that APFS accepts can observe as UNKNOWN. A path in the index
or in a tree is at most `max_path` bytes, and every path, worktree paths
included, is at most `max_depth` directories deep. Every parsed tree entry, directories included, and every
listed directory name counts against `max_entries`. Work and memory stay
linear in the input; the largest permitted index takes seconds, not minutes. The worktree is walked twice and every repository input is read
twice; any difference, or a root path that now opens another directory, fails
closed.

**Supported subset.** Index versions 2 and 3 with stage-0 entries and no flags,
optional extensions `TREE`, `REUC`, `EOIE`, `IEOT`. Config: only
`core.{repositoryformatversion 0|1, filemode, bare = false, logallrefupdates,
ignorecase, precomposeunicode, symlinks}` with literal `true`/`false`,
`extensions.objectformat = sha1`, and the `remote`, `branch` and `user`
sections without partial-clone keys. HEAD on `refs/heads/*` or detached; loose
or packed refs; loose commit and tree objects, SHA-1 only. Everything else is
UNKNOWN, including: packed-only objects (no pack reader), SHA-256, alternates,
grafts, replace refs, shallow, partial clone, includes, quoting, `core.worktree`,
attributes files anywhere (`.gitattributes`, `info/attributes`),
`config.worktree`, sparse checkout, `index.lock`, assume-unchanged,
skip-worktree, intent-to-add, split index, untracked cache, fsmonitor, sparse
index, index version 4, conflicts and submodules.

**Results.** CLEAN means only: HEAD's tree equals the index, every index entry
exists with the same type, executable bit and content hash (computed from the
bytes, never from stat data), and nothing else exists in the worktree except
the root `.git`. Untracked and ignored files and empty directories are
NOT_CLEAN. One observation stops at its first IDENTITY_CHANGED or UNKNOWN
finding; NOT_CLEAN collects every difference. Reasons are at most 16 fixed codes with escaped, truncated paths; results
carry digests, never file contents. An `Observation` has no truth value.
`combine` gives the worst status of an attempt and turns disagreeing CLEAN
results into UNKNOWN, so a retry cannot upgrade an earlier result.

**Identity.** The binding holds each root's device, inode and birth time, and
the digest of the pointer bytes. macOS reports `st_birthtime`; Linux does not,
and none is invented. With `require_birthtime` (the default) a missing birth
time is UNKNOWN. With it off, the binding detects a replaced root only while
the inode number is not reused (P7).

**Provenance and limits.** The observer produces no provenance and is absent
from the `_produce` allowlist; a static test keeps it from importing the
protocol core's authority modules. Its digests are supporting observations,
unprivileged in PR-2. CLEAN is not proof of quiescence or immutability:
someone who can write the tree can change a file and restore its mtime, and
only the ctime check detects that. SHA-1 ids are checked for integrity only,
not for collision resistance. Most real repositories have packed objects or
config outside the subset and so observe as UNKNOWN; Git can call clean what
this observer calls NOT_CLEAN (empty directories, case or Unicode
normalization, `filemode = false`). The result is defined over
repository-local state only: environment variables and global or system Git
config are never read, so a consumer must not equate CLEAN with what a Git
process under some other configuration would print. Object stores hard-linked
by a local clone observe as UNKNOWN.

## Containment protocol (PR-3a)

Normative basis: the PR-3a contract R2 as amended by R3, R4 and R4.1 (CG-150
design evidence). PR-3a is inert: it adds pure modules and a universal gate,
no producer, no opclass row and no OS effect.

- `o5contract.py`: constants, `cycle_id`, `barrier_id`, the sealed boundary
  types (`PreBasis`, `ContainmentContext`, `HandoffVerdict`,
  `PreservationDecision`, `Discharge`), `Wiring` and the cell vocabulary.
  Seals are an accident guard, never authority.
- `cutover.py`: the Model B head rule over ledger burns, documents, records,
  head entries, witnesses and the floor; `cutover_allowed`; the G7a gate.
- `barrier.py`: chain verification and derived state, overlap,
  `handoff_allowed`, the release table (D1/D2 here; D3/D4/D3H/D4H only via a
  PR-3b `Discharge`).
- `freeze.py`: the F relation and fold, cycles and closure kinds,
  `classify_burn`, the gate G0–G8, `basis_for`, `quiescence_check` rules 1–6
  and the release precondition.
- `evidence.py`: ingestion (an `IngestCapability`, which is not a `CommitTxn`)
  and selection.

Rules worth repeating:

- Protected mode is a constant refusal (`freeze._protected`).
- Without a PR-3b provider every F verb, quarantine, quiescence, P/PC cell,
  handoff and D3/D4 release refuses; only D1 and pure classification work.
- `ready_pending` (P16/P17) is an open cycle for every rule; only an
  authenticated, marked READY whose gate document carries `D-READY` closes it.
- The H-EST input binds `holder_chain = cycle.holder_chain` and
  `release_target = null`; the proposed handoff document is validated without
  a burn and never counts as a committed authorization.
- The in-memory record stores give no rollback resistance; the PR-4 off-host
  floor is the only rollback anchor.
- PR-3a.1 (PR-3b contract R4.1 §2): the effect is
  `freeze(verb, slot, op_id, ctx, cycle_id)` and the PR-3b check is
  `check_fresult(ctx, verb, result, cycle_id)`. A `freeze-start` W derives
  `cycle_id` from its own burned, confirmed document before it appends the new
  F WAL intent. Every later F verb, and every F intent resume, recovers the
  identity from the original authenticated cycle and validates it; a resume
  never creates a cycle from its own authorization. The `refreshed` record
  reuses the same value. It is a binding, never an authorization, and never a
  request field.
- PR-3b (`preserve.py`, `journal.py`, `gitclass.py`): the provider reads only
  what PR-3a selected, through a read-only resolver, and writes nothing. The
  effect's artifacts are cycle-bound (`journal/<cycle_id>/<seq>`,
  `artifact/<cycle_id>/<digest>`, `blob/<sha256>`); their writer is PR-4.
- PR-3b Git classification: a repository is located by any spelling of its
  `.git` component (R2 K3: case-insensitive, plus the code points HFS+
  ignores); two spellings under one root are `unknown`. So is a root with a
  path that shares a file or directory with another manifest path (worktree
  paths included): the same path below the root, or a file named twice, a
  file named like a directory or one directory spelled two ways on a case-
  and normalization-insensitive volume (APFS, HFS+). So is a git path that
  is not UTF-8, and a path below the gitdir that APFS would open as one of
  the administrative names the classifier reads (`shallow`, `info/grafts`,
  `config`, `packed-refs`, `commondir`, `objects/info/alternates`, `refs`,
  ...) under another spelling: Git reads it, a byte comparison would not. A
  manifest path with an empty, `.` or `..` component, a NUL, a component
  over 255 bytes or more than 64 separators is not one the capture walk
  produces (R2 §3.2) and refuses as `artifact_mismatch`. A reachable object
  is `packed` when a version 2 index lists it and the index passes bounded
  structural checks against its sibling pack (R2 K7), whether or not the
  object is also loose (R4.2 amendment 1). That is presence only: packed
  contents are never inflated or hashed, so a pack and index an agent forged
  to be self-consistent can list an object the pack does not hold. A
  reachable object that is neither loose nor listed is `missing_object`.
  Under R4.2 below, `packed` makes Q5 `NOT_ELIGIBLE`.
- PR-3b object structure (R2 §2.1: a property that cannot be established is
  never success). A reachable loose object with the right SHA-1 is verified
  only when its structure is in the supported subset and every link has the
  type its referrer requires. Its header size is canonical decimal. A commit
  has one `tree`, zero or more `parent`, one `author` and one `committer`,
  in Git's fsck order and ident form (seconds at most 2^63-1, Git's
  `date_overflows` bound), repeats no mandatory header later, and continues
  only `gpgsig`, `gpgsig-sha256` and `mergetag` on following lines. A tag has `object`, `type`,
  `tag` and an optional `tagger`, and its target has the declared type. A
  tree has the PR-2 observer's modes, valid distinct names other than `.git`,
  in Git's order. HEAD and branches name commits. Anything else is
  `corrupt_object`, a defect: Q3 and Q4 `INCOMPLETE`, Q5 `NOT_ELIGIBLE`. This
  is stricter than Git where Git only tolerates (uppercase hex, zero-padded or
  legacy modes).
- PR-3b gitlinks (R4 §5.2, B25: a gitlink entry alone is benign). A `160000`
  entry is recorded as the benign Q2 feature `gitlink` and not followed: the
  submodule commit lives in another object store. A checked-out submodule is
  a nested repository or a gitlink file and is classified as one. Q5 covers
  the bytes the slot held; it says nothing about submodule content that was
  never in the slot.
- PR-3b history views. A commit-graph (`objects/info/commit-graph` or
  anything under `objects/info/commit-graphs/`) is a rebuildable Git cache
  that is never trusted: Git takes parents and generation numbers from it
  instead of the commits, so a forged or stale graph changes Git's history
  without changing a verified object. Restore copies bytes and never runs
  Git (R4 §5.4), and PR-3b has no step that drops or rebuilds the graph, so
  ignoring it cannot be enforced here. Any commit-graph is therefore the
  unsupported finding `commit_graph`, like grafts and replace refs (F5 G12):
  Q3 `INCOMPLETE`, Q4 unchanged (its bytes are preserved), Q5
  `NOT_ELIGIBLE`. Another spelling of either name is `unknown`. Git writes a
  graph on `gc`, which also packs the objects, so such a slot is already
  `NOT_ELIGIBLE(packed)` under R4.2. A `refs/replace/*` name is
  `replace_refs` whether the ref is loose or a `packed-refs` line.
- Recorded head on a case-insensitive volume (not changed in PR-3b). Git on
  APFS reads a loose ref under another case (`refs/heads/Main`) for a
  packed ref (`refs/heads/main`); the classifier compares ref names as
  bytes and records the packed oid. Both oids are verified tips and their
  objects are preserved, but the preservation document's `head` names the
  packed commit, not Git's. With a synthetic final observation that reuses
  the classifier this reaches P18 and D4. READY refuses it
  (`NOT_CLEAN(head)`) when `final_observation.head_resolved` is the commit
  the PR-2 observer resolves through the OS, which is Git's. PR-4 must
  produce `head_resolved` that way; the observer's CLEAN status alone does
  not catch this case once the index matches Git's HEAD.
- PR-4 prerequisite. Every `not_preservable` refusal (R4.2 and the object
  rules above) is `check_fresult` rejecting after the freeze-start move ran;
  F stays `MOVE_INTENT` and the resume never repeats an irreversible step.
  PR-4 needs a pre-effect eligibility check and an owner-authorized recovery
  path for such a slot. PR-3b provides neither.

## Owner amendment R4.2 to the PR-3b contract (2026-11-16)

A narrow safety amendment to approved R4.1. It supersedes only the
packed-only ELIGIBLE/READY assumption of R2 K7, R4 §5.2 (row "Packed-only
objects": `eligible`) and acceptance test P2 ("READY reached"). Historical
contracts and evidence stay as written.

- **Rule.** When any reachable object of any captured repository is listed
  by a pack index (feature `packed`; amendment 1 below: also when it is
  loose), Q5 is
  `NOT_ELIGIBLE(packed)`. This covers valid packed-only and mixed
  repositories, deltified objects and forged self-consistent packs alike,
  because PR-3b never verifies packed contents or their closure. No trusted
  packed-content verifier exists; adding one needs its own review.
- **Unchanged.** Q1 (raw capture) and Q4 (R2 §2.1: is preservation complete)
  keep their meaning: a complete capture of a packed repository is still Q1
  and Q4 `COMPLETE`. Q3 stays `INCOMPLETE(packed)` and is still never read as
  a gate; the Q2 feature `packed` feeds Q5 directly. A structurally invalid
  index, or an index without its pack, stays `unknown` (Q4 `INCOMPLETE`).
- **Not a blanket refusal.** A pack file alone changes nothing: when every
  reachable object is a verified loose object and no pack index lists any of
  them, unrelated packs are ignored and the slot stays eligible. (Amendment 1
  below supersedes the earlier wording that duplicate packs are ignored.)
- **Where it refuses.** The EP manifest is the output of the freeze-start
  move effect, so the first refusal is `check_fresult` rejecting
  (`freeze-start`, MOVED) with `not_preservable` after the move ran: F stays
  `MOVE_INTENT`, no attribute is revoked, the barrier stays held and the
  release refuses, the F resume repeats the idempotent effect and is rejected
  again. This is the existing path of every other `NOT_ELIGIBLE` reason
  (shallow, unborn HEAD, alternates). D-QUAR and D-PRES recompute Q5 and
  refuse on their own; READY needs D-PRES, and D4 needs READY.
- **P2 as amended.** Packed-only: freeze-start rejected `not_preservable`;
  recomputed Q1 `COMPLETE`, Q3 `INCOMPLETE`, Q4 `COMPLETE`, Q5
  `NOT_ELIGIBLE`; no preservation document, no READY, no D4.

### R4.2 amendment 1: a pack listing outranks a loose copy (NB-12, 2026-11-17)

Owner decision on review finding NB-12 of `c49f2636`. It widens the R4.2
refusal (fewer slots are eligible) and changes nothing else: Q1, Q4, the
refusal path and the other features keep their meaning.

- **Why a verified loose object is not enough.** Git looks an object up in
  its packs before its loose objects. When a pack index lists an id that
  also exists loose, Git's view of that object is the packed entry, which
  the classifier never reads, and whether a command re-hashes it depends on
  the command and the Git version. A self-consistent forged pack whose index
  lists the HEAD commit's id makes `git log` show the forged commit (Git
  2.38 and 2.54) while the loose HEAD still verifies; one listing the root
  tree's id makes `git checkout` write files the verified tree does not name
  (both versions); for a forged blob entry 2.38 checks out the forged bytes
  and 2.54 the loose ones. Before this amendment both reached
  D4 with the PR-2 observer `CLEAN`, and a routine `git gc` on the restored
  byte copy deletes the true loose object through `prune-packed` and keeps
  the forged one. A D4 recovery claim says Git will see the verified
  history; with a listed duplicate nothing establishes that.
- **Rule.** A reachable object is `packed` when any captured pack index
  lists its id, whatever the loose copy holds. Loose precedence is never
  assumed and duplicate representations are never assumed harmless. The
  loose copy is still read and walked, so a corrupt or mistyped loose
  object, or a missing one below it, stays a defect (Q4 `INCOMPLETE`).
- **The cases, distinguished.**
  - *Loose and packed representations of the same object* (an index lists a
    reachable id that is also loose): `packed`, Q5 `NOT_ELIGIBLE(packed)`,
    whether the packed entry is genuine (`git repack -a` without `-d`) or
    forged. Git writes the genuine case; it is now unsupported for recovery,
    and its bytes are preserved as for any other `NOT_ELIGIBLE` slot.
  - *Duplicate packed representations* (two or more indexes list the same
    reachable id, with or without a loose copy): `packed`. Which entry Git
    reads depends on its pack order and differs between Git versions (2.38
    and 2.54 pick different packs for the same bytes), so no copy is
    preferred.
  - *Multi-pack-index* (`objects/pack/multi-pack-index`, or anything under
    `objects/pack/multi-pack-index.d/`): Git looks objects up through it
    before any pack index and reads the entry it names even when no `.idx`
    lists that id. It is never parsed here, so any multi-pack-index is the
    unverified finding `multi_pack_index`: Q3 `INCOMPLETE`, Q4 unchanged, Q5
    `NOT_ELIGIBLE`. Another spelling of either name is `unknown`.
  - *Unsupported pack content and index inconsistencies* are unchanged: an
    index that fails the structural checks, one without its pack, or a
    version 1 index is `unknown` (Q4 `INCOMPLETE`); packed contents are
    never inflated, so deltified, forged and genuine packed objects are the
    same `packed` finding.
  - *Unrelated packs* (no index lists a reachable id) are ignored, as before.
- **Acceptance.** Loose-only repositories, signed and merged ones included,
  and those with an unrelated pack still reach D4. Every case above stops at
  freeze-start with `not_preservable` and never reaches READY or D4.

### Administrative directory captured as an entry (NB-13, 2026-11-17)

A manifest entry is `(path, digest)` with no file type, so the classifier
cannot tell a regular file from a symbolic link whose capture recorded some
bytes. Git opens `objects`, `objects/pack`, `objects/info`,
`objects/info/commit-graphs`, `objects/pack/multi-pack-index.d`, `info`,
`refs`, `refs/heads`, `refs/replace`, `modules` and `reftable` as
directories. As a regular file Git skips them. As a symbolic link to a
directory outside the slot, Git 2.38 and 2.54 follow it: a forged pack there
replaces HEAD in `git log`, `alternates` there adds outside objects, `grafts`
there rewrites history, and with a detached HEAD a linked `refs` supplies
replace refs. The classifier saw only the loose objects, so these slots were
eligible.

- **Rule.** Any captured entry exactly at one of those gitdir-relative names
  is `unknown` (Q4 `INCOMPLETE`, Q5 `NOT_ELIGIBLE`). Another spelling of the
  name was already `unknown` through the administrative-name rule. Entries
  below those directories are unchanged.
- **Not covered here.** A symbolic link elsewhere under a gitdir (for example
  `config`, `HEAD`, a ref, a loose object, a `.pack`, `hooks`, `logs`).
  When the capture records the link target as the entry's bytes, a typical
  target such as `/path` does not parse and is refused. That is not a
  guarantee, because some target strings are also valid config or ref text.
  When the capture records the bytes of the linked file instead, the
  classifier cannot detect the link at all. PR-4 must therefore either never
  recreate a symbolic link below a gitdir, or record the type and refuse
  (`git-symlink`, PR-3b contract r2 G16a). The classifier cannot enforce
  this without a typed manifest.

### Scope of the D4 recovery claim (NB-15, 2026-11-17)

D4 states that Git, reading the restored slot, sees for HEAD and every
captured ref exactly the objects the classifier verified as loose objects.
It states nothing else. In particular:

- **Unrelated packs.** A pack whose index lists no reachable id is ignored
  (R4.2), even when its contents are forged. `git fsck` on such a restored
  slot fails (exit 4 for a forged unrelated entry) while D4 holds. Neither
  that nor `git gc`, `repack` or `prune` succeeding is part of the claim.
  Changing this needs a separate owner decision.
- **Unreachable objects** (loose or packed), reflogs, the index, hooks and
  other administrative files are preserved as raw bytes only. They are not
  verified.
- **Evidence limits.** Git 2.38.0 and Apple Git 2.54.0 on APFS, macOS only.
  Linux and Windows runtime, other Git versions and HFS+ were not exercised.

## Not in PR-1

PR-2..PR-5, RS-1, RS-4, NL1–NL6, L1–L8, P1–P24, real file I/O, any account,
sudo rule, LaunchDaemon or live pool access.
