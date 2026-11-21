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
| `authz_txn.py` | `CommitTxn`; `_issue` is referenced only by `authz.revalidate` (split out to avoid an import cycle); from PR-4c also the store capability and `_issue_effect`, referenced only by `invocation._f_complete` |
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
| `gitbytes.py` | pure byte primitives `gitraw` and `gitclass` share: hex ids, `.git` name detection, loose-object decoding; each caller keeps its own size bound and error policy |
| `indexverify.py` (PR-2) | index parser, worktree comparison, `observe`, result model |
| `fscap.py` (PR-4c) | the filesystem capability and its checks; no production backend before PR-4d |

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
  or `CommitTxn`;
- the in-memory filesystem model, and any constructor of the store or
  filesystem capability other than `invocation._f_complete`'s one mint (PR-4c).

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

**`.git` names.** An index path or a tree entry name with a component that
`gitbytes.is_gitdir_name` reads as `.git` is UNKNOWN (`malformed-index`,
`malformed-object`). That is every case spelling (`.GIT`, `.Git`) and every
spelling with the code points HFS+ ignores, as Git's `is_hfs_dotgit` does
(U+200C..U+200F, U+202A..U+202E, U+206A..U+206F, U+FEFF), for example
`.g<U+200C>it` and `<U+FEFF>.git`. The refusal holds on every volume,
case-sensitive ones included (amended by #548; PR-3b uses the same predicate,
see "Gitdir name and restorable gitdir").

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
- PR-3a.1 (PR-3b contract R4.1 §2), extended by PR-4c (P-B1(b) below): the
  effect is `freeze(verb, slot, op_id, ctx, cycle_id, cap, fscap)` and the
  PR-3b check is
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
- `_traces` (from PR-4c): a burn's cycle has a trace exactly when a journal
  or artifact key carries its cycle id, or a barrier `refreshed` record
  names it. Artifact and blob bytes are never scanned for identifiers, and
  blobs live in a separate `blobs` store, so bytes an agent controls cannot
  create a trace.
- PR-3b Git classification: a repository is located by any spelling of its
  `.git` component (R2 K3: case-insensitive, plus the code points HFS+
  ignores), but only the exact bytes `.git` are accepted as its gitdir; any
  other spelling, and two spellings under one root, are `unknown` (amended
  2026-11-17, "Gitdir name and restorable gitdir" below). So is a root with a
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
  bytes and records the packed oid. Git's oid is the loose file's id or,
  when that file is a symbolic ref, the oid its target resolves to, or no
  oid at all when that target does not resolve (dangling or looping), and
  Git then has no HEAD. Any oid Git reaches this way
  is the value of a captured ref and so a verified tip (a name that folds
  onto a captured `.lock` file, which the classifier skips, is refused
  instead; see "packed-refs" below), and its objects are preserved, but the
  preservation document's `head` names the packed commit, not Git's. With a synthetic final observation that reuses
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
- PR-4 entry criterion (NB-16, owner, mandatory). Before PR-4 restore or
  release is implemented, PR-4 must show with tests that all five hold for
  every path of the slot (worktree, gitdirs and linked-worktree pointers):
  1. Capture distinguishes file types (regular, executable, directory,
     symbolic link) and records the type with each entry.
  2. A symbolic link is never silently represented as a regular file:
     neither its target string nor the bytes of the file it points to are
     recorded or restored as a regular file's content.
  3. Restore never follows a symbolic link out of its authorized tree, for
     any path it creates, writes or replaces.
  4. Every directory Git needs to accept a gitdir (`refs`, `objects`),
     empty or not, is recorded by capture and recreated by restore; it is
     never implied by the files below it.
  5. Restore never lets Git discovery, run from the slot root or from a
     nested repository's worktree, open another repository than the
     captured gitdir (for example a bare repository at the slot root).
  The PR-3b manifest (`codegraph.o5.manifest/v2`, entries `[path_hex,
  hex64]`) is untyped and cannot express 1, 2 or 4, so it must change in
  PR-4. PR-3b does not meet this criterion and claims nothing about it;
  NB-13 below only refuses symbolic links at administrative directory
  names, and "Gitdir name and restorable gitdir" below only refuses the
  slots where 4 and 5 visibly fail for the untyped manifest. Those two
  refusals may be relaxed only together with tests that show 4 and 5.

## Owner decision P-B1 option (b): restricted effect authorities (2026-11-15, recorded 2026-11-18)

Owner decision on PR-3b contract R4.1 §13 P-B1 (BX-1, N-R41-1), first
recorded with #545, and its 2026-11-18 refinement that separates
store-writing authority from filesystem-effect authority. It authorizes no
implementation and does not enable protected mode; each PR that
implements it needs its own security review.

### Common rules
- No broker code passes the effect, or lets it hold, the CommitTxn or any
  object whose methods write a store other than through the store
  capability's checked operations. The journal, artifact and blob stores are
  capability-gated: they refuse the CommitTxn and accept only the store
  capability, so that capability replaces the CommitTxn's write authority
  over them rather than sharing it. The effect receives exactly two
  capabilities: the store capability (A) and the filesystem capability (B).
- The rule above binds the production effect, which first exists in PR-4f:
  it never possesses the CommitTxn, the world, a store object or any other
  object from which a write other than the two capabilities' checked
  operations can be reached. The PR-4c stand-in effect is test code outside
  the installed file set. It is inert: it holds the test world, it has no
  authority in an installed broker, and its direct writes to test-owned
  stores are test setup, not a write API.
- Both are created together, only inside phase W of an authenticated owner
  invocation, from that invocation's live CommitTxn, after the F WAL intent
  for the verb is durable and the canonical cycle id has been validated from
  the cycle's own burned document. Exactly one call site creates them, at
  most one pair per CommitTxn and per invocation; a second mint attempt
  refuses and changes nothing. The same mint creates the read-only reader
  and the FsCap authority below, and every rule of this paragraph covers
  them too. They have no public constructor and are never derived from
  request fields, an arbitrary object, a deserialized value or a test
  fixture; an installed broker contains no other way to create one
  (packaging gate). No agent-controlled data mints or widens either
  capability.
- Trust model. The broker's installed code is trusted. Agent code never runs
  in the broker's interpreter and never receives a capability object. Python
  object boundaries do not isolate hostile code running in the same
  interpreter: the seal, the single call site and the static allowlists are
  defence in depth against mistakes in trusted code, not an isolation
  boundary. At run time each use is checked against the capability's own
  open state, which the call site ends by sealing the FsCap authority after
  the effect returns or raises and before the CommitTxn closes, and against
  its bindings; that stops misuse driven by data, not by code.
- Activation prerequisites. Protected mode may be activated only after each
  of these gates is verified independently on the installed artifact; no PR
  may drop, weaken or redefine one without its own owner decision:
  - AP-1, trusted process: agent code never executes in the broker's
    interpreter or process; the broker runs only installed,
    packaging-verified code with a clean import path and environment; its
    entry points are confined;
  - AP-2, privilege isolation: the model B accounts, the sudo rule and the
    session verifier, verified in a VM and then on the host;
  - AP-3, platform: the platform assumptions P1–P37 and P39–P43; the P35
    holder condition, shown on the target platform, that no non-root process
    holds a descriptor or working directory on any directory of the
    root-only chain (`owner/`, the staging root, `staging/<cid>/`, the
    quarantine root, `quarantine/<op_id>`) and that the chain has been
    root-only since its creation, which is an activation condition and not a
    run-time proof; the floor; and the other preconditions of the activation
    PR. If one of them cannot be shown, protected execution refuses.
- Until the activation PR, protected mode is refused with the constant
  refusal by the merged B preconditions (A32, `require_synthetic`) before
  any write; that refusal stays mandatory and unchanged. The call site's
  mint also refuses protected mode, taking the mode from the invocation's
  own trust, never from the effect. The mint refusal is defence in depth
  behind A32: it is reached only after the F WAL intent is appended, so it
  ends `UNFINISHED` with that intent pending, never with the constant
  refusal.
- Each is bound to exactly one canonical `cycle_id`, the permitted F verb,
  the slot and `op_id`, and the `auth_digest` of the authorization that W
  burned. For a `resume-step`, the verb, slot, `op_id`, `cycle_id` and
  `auth_digest` are those of the pending F WAL intent that the `resume-step`
  document names by digest and that the F WAL fold has validated as a
  burned, stored document of that verb and operation. The `resume-step`
  document authorizes only the completion, and the F WAL completion entry
  keeps recording its digest, as merged. A completed intent is never pending
  again, so it is never bound twice. Before a resume mint, the call site
  takes the identity of the pending authorized effect from two independently
  authenticated sources and refuses any conflict between them: the
  operation's F WAL log of the slot the request names, re-read inside W and
  folded again over burned, stored documents, whose last entry is the
  pending intent, with no completion after it; and the original
  authorization document that the intent's `auth_digest` names, re-read,
  re-hashed and burned by an earlier ledger entry. The bound slot is that
  log's slot and equals the original document's slot; the bound `op_id` is
  the intent's and equals the original document's; the bound verb is the
  original document's and equals the intent's step; the bound `cycle_id` is
  recomputed from the authenticated cycle whose last F WAL entry is that
  intent, and for a `freeze-start` intent its `auth_digest` is the cycle's
  `freeze-start` authorization; the bound `auth_digest` is the intent's. The
  `resume-step` document, re-read by its own digest, re-hashed and burned by
  this invocation's ledger entry, only selects: its slot, `op_id` and
  transition must equal the bound slot, `op_id` and verb, and its
  `target_digest` must equal the digest of the pending intent. Nothing from
  the invocation's planning snapshot or from the `resume-step` document
  enters the binding, and no comparison takes both sides from one source.
  The holder is the root broker in phase W; no other process, verb or
  invocation can hold either.
- Both are valid only while that CommitTxn is open, and both are closed when
  the effect returns or raises, by the seal of their FsCap authority. Any
  later use raises. Neither is serializable, and a resume receives new
  capabilities from a new invocation; a capability cannot be replayed.
- Neither creates provenance, trust or authorization. Objects and filesystem
  states they produce are inputs that PR-3b `check_fresult` and the D-*
  checks re-hash and validate; none is decisive on its own, and no stored
  conclusion among them is trusted.

### A. Store capability: restricted writes to approved namespaces
- Every store write acts on descriptors derived from the store root; the
  capability names keys, never paths or descriptors.
- Permitted writes, each an exclusive or content-addressed create:
  - the journal namespace `journal/<cycle_id>/<seq:08d>` of its own cycle,
    with consecutive `seq` starting at 0;
  - the artifact-metadata namespace `artifact/<cycle_id>/<sha256>` of its
    own cycle, where the key is the digest of the bytes;
  - the raw-blob namespace `blob/<sha256>`, where the key is the digest of
    the bytes, in a store that `_traces` does not scan.
- Every journal or artifact object carries this cycle's header; the
  capability refuses one that does not. This cycle's header is the one the
  call site derives at creation from the validated containment context and
  the cycle's burned `freeze-start` document, as the PR-3b reader derives
  it; the effect cannot supply, replace or broaden it.
- Every journal record has the schema of the cycle's `seq 0` record, and
  that schema must be one the mode accepts: `journal/v3` only in protected
  mode; `journal/v2` or `journal/v3` in synthetic mode, where merged v2
  behaviour is unchanged except the owner-approved PR-4b tightening of
  `freeze-start` `NONE`.
- A binding's blob set is stored as header-carrying artifacts, never as a
  raw blob. Today one `{header, set}` object bounds a binding to about 722
  distinct blobs, and a larger set refuses; this is a temporary fail-closed
  limit. A representation for larger sets needs its own erratum and owner
  approval before the PR that implements it; this decision approves no
  encoding and no new capacity.
- Forbidden: the ledger, authorization documents, custody, Tier C, the F
  WAL, gate records, cutover and cutover-head records, enumerations, barrier
  records, evidence and evidence bodies; every other cycle's journal and
  artifact namespaces; any delete, rename, truncate or overwrite of a store
  object; any listing outside its own prefixes; and every filesystem
  operation outside the stores.
- The first durable write is `journal/<cycle_id>/00000000`
  (`kind: "start"`). No blob, artifact or filesystem effect precedes its
  durability, and the record stays discoverable by its cycle-scoped key. An
  object observed to exist is relied on only after it has been made durable;
  existence is never durability.
- A blob or artifact that already exists is accepted only if its bytes
  re-hash to its key. Non-identical content is never overwritten; it is a
  refusal.
- A key listing used by a capability, a resume or `_traces` is complete or
  raises `Refused(STATE_UNKNOWN, "artifact_inventory")`; never a partial or
  empty list in place of an error.

### B. Filesystem capability: explicitly authorized operations
- It performs only these operations, each for its own cycle and verb:
  - `freeze-start`: create the empty root-only per-operation quarantine
    directory and its empty `linked` directory; rename the object whose
    identity is the cycle's `root_id` from the slot's configured name to the
    quarantine name, with at most one successful rename and none after a
    completion or abort record;
  - `freeze-continue`: set the owner, mode, ACL and flags of the cycle's
    objects (the slot root and its captured entries, identified by their
    captured identities) to the fixed revoke targets;
  - `thaw`: set those attributes back to the captured originals, and rename
    the root back to its original name, with at most one successful rename;
  - `verify-preservation`: create the cycle's staging directory if absent, a
    new, uniquely named staging attempt root under it, and new empty
    directories and new regular files inside the attempt that build the tree
    the cycle's verified final manifest describes; after the build, change
    the attempt root's mode from 0700 to 0755 once; nothing outside the
    attempt, and no other mode change;
  - read-only descriptor walks below the slot root, the quarantine root or
    the staging attempt, as the verb needs, and reading the identity and
    type of what the slot name holds with one no-follow `fstatat` relative
    to the pinned slot parent, which opens nothing (D2.2.1 §17.3.3 step 2
    and the thaw pre-check). A regular file's content is read only if it is
    owned by the agent uid with one link (slot root), shows the revoke
    target values with one link (quarantine), or was created by this attempt
    (staging); a cycle object identified by its captured identity may also
    be opened, without reading its content, for the attribute operations
    above.
- No operation publishes anything. Handing a staging tree to a slot is Phase
  3 publication, outside this decision.
- Layout. The authorized layout, the root identity, the parent
  relationships, the allowed operations and the endpoint names are fixed at
  mint: the configured slot parent and slot name, the configured quarantine
  root, the names `quarantine/<op_id>`, `linked` and the quarantine
  destination name, the configured staging root and `staging/<cid>/`, and
  the bound `root_id`. A configured directory that exists at mint is pinned
  by the identity and the device the OS reports when the mint opens it by
  descriptor; each root keeps its own device, and the staging root may be on
  another volume than the slot and the quarantine. A directory that does not
  exist at mint has no identity at mint: it acquires one only through its
  authorized creation below, and every later operation verifies it against
  that creation or against the durable journal intent that names it. No
  identity is assumed for an object that does not exist.
- Journal state. Every filesystem operation, read walks included, needs a
  `journal/v3` cycle, and first re-reads the cycle's journal by exact key
  from `journal/<cycle_id>/00000000` through the merged walker, with this
  cycle's header and operation, up to its last record. A journal the walker
  rejects, or one with a record after `end`, refuses every operation; a
  journal whose last record is not the state listed for an operation refuses
  that operation; neither changes anything. The states are: for
  `prepare_quarantine`, `ep.done`; for `move`, `move.intent`; for an
  attribute change, its own open `revoke.intent` or `thaw.intent`; for
  `move_back`, `thaw.move.intent`; for the staging steps, `revoked`; for
  read walks and flushes, any state the walker accepts. Every check of an
  operation runs before its first filesystem write, so a refusal leaves the
  filesystem and the stores unchanged.
- Completed EP. `prepare_quarantine`, `move`, every attribute change and
  `move_back` need the cycle's completed EP: the canonical `ep.done` record
  at `seq 1`, found by that re-read, reported durable by the store machinery
  through the read-only reader ("Reader"), and the EP binding it names (the
  manifest, its blobs and `originals`) re-read and validated as the PR-3b
  reader does. A record that is buffered, not yet put, not reported durable,
  of another cycle or only asserted by the caller is never relied on.
- Intent authority. The filesystem capability never writes, mints,
  authorizes or commits a journal record, an intent included. An intent
  exists only because the store capability put it, with its checked header,
  `seq`, `prev` and schema, in an exclusive put that returns only once the
  store machinery reports it durable. Before an irreversible operation the
  filesystem capability re-reads that intent through the read-only reader
  minted with the pair (see "Reader"), and requires the store's durability
  result. It then checks through the walker that the intent is canonical, of
  this cycle and operation, and the journal's last record (the next `seq`
  absent by exact-key read), and checks its fields against the pinned
  layout, the bound `root_id`, the verified identities and, for a range, the
  range rule below. A value held only by the effect or by the filesystem
  capability, or an intent not re-read this way, authorizes nothing. In
  PR-4c the store and the reader are in-memory models: their durability
  result is modelled state, not proof of durable storage, and the object
  boundary between them is not a security boundary against hostile code in
  the same interpreter.
- Reader. The read-only reader is created by the same single mint as the
  pair, in phase W, and only the filesystem capability holds it; the effect
  never receives it. Its implementation is the installed `fscap` module's,
  fixed by the packaging gate; no world attribute, configuration value,
  request field, argument or effect supplies, replaces or wraps it. It
  carries the pair's binding (slot, `op_id`, `cycle_id`, verb, `auth_digest`
  of the bound intent) and reads only by exact key: this cycle's
  `journal/<cycle_id>/…` and `artifact/<cycle_id>/…` keys, and
  `blob/<sha256>` keys in its allow-set. The allow-set starts empty. The
  filesystem capability adds a key to it only from a binding that the walker
  or the PR-3b reader has already validated through this reader: the blob
  set and the `originals` digests of the EP binding that the durable
  `ep.done` names, and, for `verify-preservation`, the blob set of the
  `final` binding that the mint fixed from committed F WAL state. No key
  comes from the effect, the caller, a request, a listing or unvalidated
  bytes. Its only other operation asks the store to make bytes it already
  holds at such a key durable and returns the store's result. It never
  writes, changes, deletes or lists, holds no CommitTxn and no write method,
  and mints nothing, no verifier attestation included. Every read is a new
  exact-key read; no earlier result is returned in its place. It is valid
  only while the pair is open, is closed with it, including when a store
  failure closes the pair, raises on any later use, and is not serializable;
  its allow-set ends with it and never reaches another invocation. A missing
  key, unparsable or non-canonical bytes, a foreign header, a digest that
  does not re-hash, or a store failure is a refusal, never a default.
  Nothing it returns authorizes anything until the walker or the PR-3b
  reader has validated it, and bytes an agent could have written are never
  trusted. The filesystem capability never relies on a journal head cached
  earlier: each operation re-reads the journal, and immediately before an
  irreversible operation it re-checks that the intent is still the last
  record. This object boundary, like every one in PR-4c, is not isolation
  from hostile code in the same interpreter.
- Private pre-publication construction. Only these steps need no per-object
  journal intent; every other filesystem effect follows the durable-intent
  rule below:
  - OD-4C-1a, private quarantine scaffolding: `prepare_quarantine`, for
    `freeze-start`, creates the new empty directory `quarantine/<op_id>`
    under the configured quarantine root and the new empty directory
    `linked` inside it, only while the re-read durable journal ends exactly
    at `ep.done`. Once any later record exists (a `move.intent` or an
    `end`), `prepare_quarantine` refuses and creates nothing;
  - OD-4C-1b, private staging construction: `staging_begin`, `mkdir` and
    `create_file`, for `verify-preservation`, create the new directory
    `staging/<cid>/` under the configured staging root, a new attempt root
    under it, new empty directories inside the attempt root and new regular
    files inside the attempt; `set_mode` changes the attempt root's mode
    from 0700 to 0755. They run only while the re-read journal ends exactly
    at `revoked`.
- Creation. Only the steps above create objects. Each create is exclusive,
  in a parent the capability holds by descriptor, of one single-component
  name, with no symbolic link followed and no `..` or multi-component name
  resolved. The parent is a configured directory pinned at mint, a directory
  this capability created in this invocation below one, or an existing
  directory accepted below. A create never replaces, renames over, opens for
  writing or changes an existing object; it only adds a new entry to its
  parent. Each created object must be on its parent's device, which is its
  root's pinned device. Each name comes from the pinned layout, from the
  capability (the attempt name) or, inside the attempt, from the cycle's
  verified final manifest, never from a request, the slot or the caller.
  Every step is for the capability's own slot, `op_id`, cycle and verb.
- Existing state at creation. Only `prepare_quarantine` and `staging_begin`
  meet existing state, and they refuse anything unexpected. An existing
  `quarantine/<op_id>` is accepted by `prepare_quarantine` only if, opened
  by descriptor without following a link, it is a directory with uid 0, gid
  0, mode 0700, no ACL, no file flags and the quarantine root's device,
  whose complete entry list is empty or exactly `linked`; an existing
  `linked` only if it is such a directory with no entry. An existing
  `staging/<cid>/` is accepted by `staging_begin` only if it is a directory
  with those attributes on the staging root's device; its entries (earlier
  attempts) are neither opened nor changed. Any other existing state
  refuses, and nothing is changed, removed, recreated or adopted. An
  existing attempt root, or an existing name inside the attempt, refuses.
- Creation modes. Every object is created with an explicit mode, never one
  that relies on the umask: `quarantine/<op_id>`, `linked`,
  `staging/<cid>/`, the attempt root and every directory inside the attempt
  0700; every regular file inside the attempt 0600. Each operation checks,
  through the object's own descriptor, the mode of its phase together with
  uid 0, gid 0, no ACL, no file flags and the root's device: the creation
  mode above for every object, except that the attempt root may show 0755
  only after this capability's own `set_mode` changed it in this invocation.
  A setuid, setgid or sticky bit, any other permission bit, an inherited ACL
  or group, metadata taken from a source object, or 0755 on an attempt root
  that this capability has not changed itself refuses. No other mode change
  of a staging object is authorized, including the modes and `x` bits the
  final manifest records; those are a PR-4e design and test obligation that
  needs its own approval.
- Attempt root mode. `set_mode` applies only to the attempt root that this
  capability created in this invocation, once, from 0700 to 0755, and only
  after the build is finished: this capability has created, in this
  invocation, exactly the `f` and `d` entries of the final manifest. It runs
  only while the attempt stays private and unpublished: `staging/<cid>/`
  still shows uid 0, gid 0, mode 0700, no ACL, no file flags and the staging
  root's device through the held descriptor. It finalizes the attempt:
  afterwards `mkdir`, `create_file` and a second `set_mode` on that attempt
  refuse, and no mode of it returns to 0700. The capability records the
  change in its own state; a later operation in the same invocation (a flush
  or a read walk) accepts 0755 only from that record and only while the
  isolation check still holds. An attempt is never used by another
  invocation: after a crash its provenance is not reconstructed, it is
  neither reused nor finalized, and no invocation infers authorization from
  seeing 0755; any reuse would need a later recovery design that verifies
  it. A mode change after which any object would be reachable outside that
  root-only parent chain is publication, not this step, and is refused.
- Restore source. Every name, object type and file byte that
  `verify-preservation` creates comes from the cycle's verified final
  manifest. The mint fixes it from committed F WAL state: the `final`
  binding of the cycle's `SNAPSHOTTED` completion, re-read inside W. The
  capability reads that binding and its manifest through the PR-3b reader
  for the `final` phase: this cycle's id, a header equal to the cycle's, a
  blob set equal to the manifest's file digests, Q4 `COMPLETE`, and an entry
  set equal to that of the EP that `ep.done` names. This is the manifest
  that a later verify binding must name, since the merged checks require the
  verify binding's manifest to equal the `final` binding's. File bytes are
  read from the blob each entry names and re-hashed against that digest;
  each file is bounded by the configured per-file limit and all of them
  together by the manifest's `totals.bytes`. Only `f` and `d` entries are
  built. A name or type not in that manifest, a `l` or `u` entry, another
  manifest, the EP manifest in its place, or a final entry set that differs
  from the EP's refuses. The EP is prior-state evidence and is never a
  restore source.
- Subsequent use. `move` (on the first attempt and on resume), `move_back`
  (on the first attempt and on resume) and every attribute change create
  nothing and never apply the creation checks, including the creation-time
  rule that `quarantine/<op_id>` be empty or hold only an empty `linked`.
  `prepare_quarantine` is never called for them, a missing
  `quarantine/<op_id>` is never recreated, and a replacement found at its
  name is never adopted. Each opens `quarantine/<op_id>` by descriptor from
  the pinned quarantine root under the pinned name, without following a
  link, and requires a directory with uid 0, gid 0, mode 0700, no ACL, no
  file flags and the quarantine root's device, whose identity equals the
  `dst_parent` of the cycle's durable `move.intent` (for `move_back`, also
  the `src_parent` of `thaw.move.intent`, which the grammar makes the same).
  The slot parent, opened from its pinned identity, must equal the intent's
  other parent key. When `prepare_quarantine` ran in this invocation, the
  intent's key must also equal the identity of the directory it created or
  accepted. For `move` and `move_back`, once their intent is authenticated
  under "Intent authority", every state that matches neither case (a) nor
  (b), or neither A nor B, is case (c), or case C: a different identity, an
  absent `quarantine/<op_id>`, an extra or missing entry, a substituted or
  missing tree, a symbolic link, an object whose device differs from the
  device its `root_id` encodes or from the device of the parent it is found
  in (a mount boundary or a device substitution), or an ambiguous name. The
  classification depends only on the observation, never on whether a rename
  was issued in the invocation. For an attribute change or a read walk such
  a state refuses the operation; a protected observation ("Protected
  observations") also poisons the FsCap authority. A directory is used only
  because a durable intent names its identity, never because it exists or
  because of its path. The directory's complete entry list is compared byte
  for byte with the names each case expects, and `linked` must be empty:
  - `move`, with `move.intent` as the last record: case (a), the slot name
    holds the object whose identity is `root_id` and `quarantine/<op_id>`
    holds exactly `linked`: rename once; case (b), `quarantine/<op_id>`
    holds exactly `linked` and the destination name, which holds `root_id`,
    and the slot name is absent: no rename, completion observed; anything
    else is case (c): no rename, `FREEZE_BLOCKED(identity_swap)`;
  - `move_back`, with `thaw.move.intent` as the last record: case A,
    `quarantine/<op_id>` holds exactly `linked` and the destination name,
    which holds `root_id`, and the slot name is absent: rename back once;
    case B, the slot name holds `root_id` and `quarantine/<op_id>` holds
    exactly `linked`: no rename, completion observed, after which the store
    capability accepts `thaw.move.done` and then `end{thawed}`, and `thaw`
    `NONE` is completed only through R5 and the merged and PR-3b checks;
    case C, anything else (the tree in both places or in neither, another
    identity, an extra entry, a symbolic link or an ambiguous name): no
    rename and no record, with nothing created, removed or cleaned up,
    giving `FREEZE_BLOCKED(thaw_target_occupied)` when `quarantine/<op_id>`
    holds exactly `linked` and the destination name with `root_id` but the
    slot name holds another object, and `FREEZE_BLOCKED(identity)`
    otherwise;
  - an attribute change, with its own open range intent as the last record:
    `quarantine/<op_id>` holds exactly `linked` and the destination name,
    which holds `root_id`, and every object is reached by descriptor walk
    from that verified directory;
  - a read walk below the quarantine starts from `quarantine/<op_id>` opened
    and verified in the same way, except that its entry list is reported as
    an observation rather than required.
- FsCap authority. Every operation of the filesystem capability and every
  write of the store capability act through one trusted FsCap authority,
  created by the single mint and bound to exactly the pair's slot, `op_id`,
  `cycle_id`, verb, pending F WAL intent and invocation; at most one exists
  in an invocation. Every alias or wrapper of either capability refers to
  that one authority, and its state lives only there, never in a wrapper; no
  other object holds, copies, replaces or resets it. Its state is
  `FSCAP_OPEN`, `FSCAP_POISONED` or `FSCAP_SEALED` and only moves forward:
  `FSCAP_OPEN` to `FSCAP_POISONED` or `FSCAP_SEALED`, and `FSCAP_POISONED`
  to `FSCAP_SEALED`; the poison reason is set once, with the move to
  `FSCAP_POISONED`. Two flags only ever go from unset to set: the `move` or
  `move_back` call is claimed, and the rename is issued. The capabilities'
  open state is this state: an operation goes on only while it is
  `FSCAP_OPEN`. These are in-memory states of the authority: they are no
  journal record, token, F result, or library, lifecycle or cutover-head
  state, grant nothing, do not survive a crash, and, like every object
  boundary in PR-4c, are not isolation from hostile code in the same
  interpreter.
- Lock and linearization. One lock of the FsCap authority serializes every
  operation of both capabilities and the seal. Each operation takes it
  before its first check and holds it through its last observation,
  filesystem effect or store write. Nothing else runs under it: an operation
  never calls back into the effect, and the call site never holds it while
  it calls the effect, appends the F WAL completion or writes a ledger,
  authorization or gate record. Code holding a store-machinery lock never
  takes it, so the order is always this lock, then the store machinery, and
  no lock-order cycle exists. An operation that never returns (a hung system
  call) leaves the seal waiting and the invocation unfinished, which fails
  closed. The linearization points, each under the lock, are:
  - LP-OBS: an observation's state check and, for a protected observation,
    its classification;
  - LP-POISON: a case (c) or C classification by a protected observation or
    by a `move` or `move_back` call sets `FSCAP_POISONED` and its reason at
    once, before that operation returns;
  - LP-CALL: a `move` or `move_back` call's check; only a call that finds
    `FSCAP_OPEN` and no claimed call goes on, and it claims before it
    re-reads or observes anything, so at most one such call in an invocation
    renames or observes for a rename; a call that finds the authority not
    `FSCAP_OPEN` or a call already claimed refuses at once, without
    re-reading, observing or renaming, and creates no result;
  - LP-RENAME: immediately before the rename, the claimed call checks again
    that the state is `FSCAP_OPEN` and no rename is issued, and marks the
    rename issued; at most one rename is issued in an invocation;
  - LP-PUT: a store write's check of the state and of "Completion records",
    and its exclusive put, in one hold of the lock, so the state cannot
    change between the check and the write;
  - LP-SEAL-HELD: a seal made by the operation that holds the lock (a store
    failure, a content veto, an observation or content read that cannot
    complete), in that same hold and without taking the lock again;
  - LP-SEAL and LP-RESULT, under "Sealing" and "Result reconciliation".
- Protected observations. While the journal's last record is `move.intent`
  or `thaw.move.intent`, every observation that can authorize, refuse or
  influence `move` or `move_back` is a protected observation: the no-follow
  `fstatat` of the slot name, an observation of the slot root's or the
  tree's identity, the opening and checks of `quarantine/<op_id>` and its
  entries, and a read walk below `quarantine/<op_id>` or below the slot
  root. At `thaw.move.intent` these include the thaw pre-check's location
  check, its checks of `quarantine/<op_id>` and its descriptor walk below
  it. Under the lock, a protected observation refuses without observing
  unless the state is `FSCAP_OPEN`; re-reads the journal at that
  observation, never using a head read earlier, and refuses without
  observing unless its last record authenticates under "Intent authority" as
  the bound verb's intent; observes; classifies the state of the slot
  parent, the slot name, `quarantine/<op_id>` and its entries as "Subsequent
  use" does, into (a), (b) or (c), or A, B or C (the content below the root
  is not part of that classification, except that at `thaw.move.intent` an
  object below `quarantine/<op_id>` that fails D2.2.1 §17.3.5 matrix rows
  1-7 is C with reason `identity`; for `move` the recapture decides
  `early_unstable`); and on (c) or C sets `FSCAP_POISONED` with the reason
  "Subsequent use" gives before it returns. A later (a), (b), A or B never
  clears it. An observation that cannot be completed (an I/O error or an
  unexpected `EPERM`) while no (c) or C is established classifies and
  poisons nothing: in the same hold it seals the FsCap authority, so no
  rename follows in the invocation, and W reports `UNFINISHED` (R2); a (c)
  or C established earlier stays and decides (R1), and a rename already
  issued stays recorded as an uncertain effect. A protected observation
  claims no call and issues no rename, so it does not spend the one
  permitted rename. The effect makes these observations only through the
  filesystem capability; an observation made in any other way is not an
  observation under this contract, authorizes nothing, and is excluded by
  the PR-4f packaging gate. Reads in any other journal state, and reads that
  decide no location or identity (staging walks, flushes, the revoke
  pre-check, and the content reads under "Content veto"), are not protected
  observations and keep their own rules.
- Content veto. At `thaw.move.intent` the thaw pre-check's content checks
  (the `originals` blobs re-hashed and passing `originals_ok`, every
  original owner the agent uid, and every attribute of a revoked or open
  range at its original or target value, D2.2.1 §17.3.5 matrix rows 8-9) are
  made only by the filesystem capability's content read. Under the lock it
  refuses unless the state is `FSCAP_OPEN`, the verb is `thaw` and the
  journal's last record authenticates as under "Protected observations"; it
  first makes every location and identity observation of the pre-check that
  the invocation has not made, as protected observations, and goes on only
  when each classified A, or each showed only a clean device mismatch; and
  it computes the verdict itself from the `originals` blobs the EP names,
  the journal's range records and objects reached by descriptor from that
  protected walk, taking no value, verdict or reason from the effect. A
  definite mismatch is a content veto whose reason is fixed by its source:
  `identity` for an `originals` or owner fault, `journal_mismatch` for an
  attribute value that is neither original nor target. In the same hold the
  veto is recorded and the FsCap authority sealed, so no later `move_back`,
  observation or write runs in the invocation. A veto is not a poison and
  needs no `FSCAP_POISONED`. A read that cannot complete (an I/O error, an
  unexpected `EPERM`, a value it cannot classify) is no verdict: it seals in
  the same way and W reports `UNFINISHED` (D2.2.1 matrix row 10).
  `freeze-start` has no content veto.
- Decision order. For a `move` or `move_back` and every observation that can
  authorize it, the outcome is decided in this order, whatever order the
  effect calls the operations in: (1) the journal walks, its last record
  authenticates as the bound verb's intent, and the authority is this
  invocation's; (2) the protected location and identity observations; (3) a
  (c) or C established by any of them poisons and decides; (4) otherwise, at
  `thaw.move.intent`, the content veto; (5) an observation or content read
  that cannot complete seals and gives `UNFINISHED`; (6) a clean device
  mismatch, only once every identity is established; (7) the result is an
  existing F result, a refusal or `UNFINISHED`. This is the order of the
  safety classification, not of the system calls: the capability makes the
  location observations before any content verdict, so no device mismatch,
  content error or call order masks an established (c) or C, and an I/O
  failure before one is established ends the invocation without a
  classification, leaving a later revalidated resume to observe again.
- Poison. `FSCAP_POISONED` is terminal for the invocation, even when the
  original tree reappears: no further `move` or `move_back`, observation,
  rename or reclassification into (a), (b), A or B, and no retry, caught
  exception or second mint resets it; the filesystem capability refuses
  every further operation, and the store capability every further write.
- Sealing. After the effect returns or raises, the trusted call site seals
  the FsCap authority before it reads any result: it takes the lock, which
  waits for every operation in progress and keeps any new one from starting;
  sets `FSCAP_SEALED` (LP-SEAL); and, in the same hold, takes an immutable
  record of the poison and its reason, the claimed call and its outcome, the
  issued rename, the content veto, the store failure, the observation or
  content read that could not complete, and the observations and store
  writes made through the authority. Sealing is terminal and idempotent: a
  second seal returns the same record, and no alias, wrapper, thread still
  running, retry or second mint reopens the authority in that invocation;
  every later operation of either capability refuses without observing,
  renaming or writing. Every rule that closes the capabilities (the effect's
  return, a store failure, a content veto, an observation or content read
  that cannot complete) seals the authority. A store failure inside LP-PUT,
  of a read or a write of the store machinery, seals in that same hold
  (LP-SEAL-HELD): the failing put sets `FSCAP_SEALED` and takes the record
  before it releases the lock and before the error reaches the effect, and
  never takes the lock again; the call site's later seal returns that
  record. Taking the lock is not by itself the guarantee: the call site
  completes only what the record determines.
- Result reconciliation. From the sealed record and the durable journal
  alone, the call site determines the one result it may complete
  (LP-RESULT); in every other case W reports `UNFINISHED` with the F intent
  pending, writes no F WAL completion and authorizes no further effect in
  the invocation. The first rule that applies decides:
  - R1, poisoned: `FREEZE_BLOCKED` with the recorded reason, whatever the
    effect returned or raised;
  - R1b, a content veto: `thaw` `FREEZE_BLOCKED` with the veto's reason,
    whatever the effect returned or raised;
  - R2, otherwise, the effect raised, a store failure sealed the authority,
    or an observation or content read could not complete: `UNFINISHED`,
    except a binding-free `freeze-start` `NONE(store_space)` or
    `NONE(store_error)` after a store failure while no `seq 0` record of the
    cycle exists; a rename already issued stays an uncertain effect, never a
    pre-effect failure;
  - R3, a claimed call whose outcome is neither the completed case nor a
    `move.abort` (no result without authority, clean device mismatch,
    `move_back` case A after its rename, or a call that ended without an
    outcome): `UNFINISHED`, whatever the effect returned;
  - R4, a rename was issued: for `move`, `MOVED` if the call observed (b)
    after it, or `NONE` with the call's `move.abort` reason if it observed
    (a); for `move_back`, `thaw` `NONE` if it observed B; never
    `FREEZE_BLOCKED` or any other result;
  - R5, the claimed call observed (b) or B with no rename, and no
    observation in the invocation classified (a) or A, which would
    contradict it: `MOVED`, or `thaw` `NONE`, or, for `move`,
    `FREEZE_BLOCKED(identity)` when the EP named by `ep.done` does not
    re-verify (D2.2.1 §17.3.3, resume row (b)), only after the claimed call
    observed (b); an EP check made before or without that call gives
    `UNFINISHED` (R6);
  - R6, no claimed call: the effect's result, except that while the durable
    journal ends at `move.intent` or `thaw.move.intent` no `FREEZE_BLOCKED`
    is completed that R1 or R1b did not give;
  - R7, whichever rule gave it: no `FREEZE_BLOCKED` is completed while the
    durable journal holds `move.done` (`freeze-start`) or `thaw.move.done`
    (`thaw`), or the record holds its put; W reports `UNFINISHED`, the
    result is not rewritten to `MOVED` or `NONE`, nothing is cleaned up, and
    only a later revalidated `resume-step` completes from the durable
    journal;
  - then the merged `check_result` and the PR-3b `check_fresult`; a
    rejection is `UNFINISHED`.
- Completion. Only the call site writes the F WAL completion, through the
  CommitTxn and after the seal; a journal record put by the store capability
  and the F WAL completion are separate writes with separate durability, and
  no journal record represents an F result. W reports `COMMITTED` only after
  the completion append returns. If the completion is not written, its
  append fails, or `check_result` or `check_fresult` rejects the result, W
  reports `UNFINISHED`, never `COMMITTED` or `ALREADY_COMPLETE`, and makes
  no claim that the completion is or is not durable; the authority stays
  `FSCAP_SEALED` and no further filesystem effect is authorized in that
  invocation. An append that fails may still have written the entry (the
  merged store's fault after the write). On the barrier basis the call site
  writes the `refreshed` record after the completion append, under the same
  CommitTxn, which is a single-use permission and not an all-or-nothing
  transaction (merged `_f_complete`); if that write fails, W reports
  `UNFINISHED` although the completion is durable. A completion without its
  `refreshed` is consistent (merged rule); no `refreshed` is written for it
  later and no repair write exists. So `UNFINISHED` never implies a pending
  F intent: a later authorized invocation re-reads the durable F WAL,
  journal and barrier chain and decides from them alone: after a durable
  completion nothing is pending and the committed effect is never repeated;
  without one the intent is pending and only a separately authorized
  `resume-step` re-evaluates it, as under "across a crash".
  `Result.durable_ref` keeps its merged meaning and wire form and does not
  name the F completion; the durable state is reported in evidence by a
  separate read-only inspection of the F WAL and barrier chain, never by the
  result. Durability is never inferred from process-local state.
- Renames and devices. The object's identity, its device included, comes
  from one no-follow `fstatat` relative to the held descriptor of the parent
  it is found in; for a rename it must be `root_id` and show the device that
  `root_id` encodes, equal to the source parent's device. The source and
  destination parents are the directories held by descriptor and verified as
  above. A rename needs the object and both parents on one device, the
  target platform's condition for an atomic same-filesystem rename (P5). An
  object on another device than its parent, a device that differs from a
  pinned identity, or an ambiguous identity is case (c) or C. That
  observation is a check at one point in time, not protection from a
  concurrent change: the rename is one exclusive rename relative to the held
  parent descriptors, and the observation after it decides.
  `prepare_quarantine` likewise requires the quarantine root and the slot
  parent to show one device. A rename is one exclusive same-filesystem
  rename, issued at most once in an invocation: `EXDEV`, any other error and
  any doubt are never retried in that invocation and never replaced by a
  copy and delete, and nothing is reported done unless the completed case is
  then observed. Creates need no rename, which is why the staging root may
  be on another volume; any later hand-over of a staging tree to a slot
  parent on another volume cannot be an atomic rename and must not be
  treated as one. Outcomes use existing results and reasons only. An F WAL
  completion is not a journal record, and no journal record represents
  `FREEZE_BLOCKED`:
  - no result without authority: in a `move` or `move_back` call, a journal
    the walker rejects, an intent not authenticated under "Intent
    authority", or a journal whose last record is not the call's state
    establishes no result; nothing is written and W reports `UNFINISHED`
    with the F intent pending, which the call site enforces from the sealed
    record (R3) whatever the effect returns. This item governs only those
    two calls; the other operations keep their own rules;
  - the observed case decides: once the intent is authenticated, case (a) or
    A permits one rename; case (b) or B is the completion (`move.done` or
    `thaw.move.done`); case (c) gives `FREEZE_BLOCKED(identity_swap)` and
    case C gives `FREEZE_BLOCKED(thaw_target_occupied)` or
    `FREEZE_BLOCKED(identity)` as "Subsequent use" says, with no journal
    record, no rename, and nothing created, adopted, removed or cleaned up;
    case (c) or C also poisons the FsCap authority (LP-POISON). This holds
    whether or not a rename was issued in the invocation;
  - clean device mismatch: when both parents are identified, verified and
    equal to the intent's keys, the object is `root_id` on its source
    parent's device, each parent shows its own pinned root's device, and
    only the two parents' devices differ, no rename is issued, nothing is
    written and W reports `UNFINISHED` with the intent pending; the FsCap
    authority is not poisoned, the claimed call keeps this outcome, and the
    call site reports `UNFINISHED` from the sealed record (R3) whatever the
    effect returns, with no F result, rename, copy or delete. Before a
    durable `move.intent` the same holds for `prepare_quarantine`. The case
    is classified first: a state that is case (c) or C, a device that
    differs from a pinned identity included, is never this rule;
  - after an issued rename, whatever it returned, the observed case decides
    in the same way, except that for `move` case (a) gives `move.abort` with
    `target_exists` for `EEXIST` and `rename_failed` otherwise, then
    `end{none, reason}`, and for `move_back` case A writes no record and
    leaves `UNFINISHED` with the intent pending. No second rename is
    attempted for that intent in the invocation, whatever the first
    returned; only a separate, authorized invocation observes again, under a
    new authority, as the next two items say;
  - across a crash: the poisoned state is not durable. A crash after case
    (c) or C was observed and before the `FREEZE_BLOCKED` completion is
    durable leaves the journal at the intent and the F intent pending, which
    is the same durable state as a crash before the observation; a
    completion that is not made durable without a crash ("Completion")
    leaves the same state. A later `resume-step`, separately
    owner-authorized and re-validated in its own invocation, repeats every
    check, as D2.2.1 §17.3.3 (resume of `MOVE_INTENT`; step 9 as clarified
    by erratum D2.2.1-E1.1) and §17.3.5 (thaw resume) require: it renames
    once only on a new observation of (a) or A, for `move` only when the
    recaptured EP binding equals `early`; it completes without a rename on
    (b) or B, and gives `FREEZE_BLOCKED` on (c) or C;
  - after a durable `FREEZE_BLOCKED`: the completion leaves no intent
    pending, so no `resume-step` applies. After a `freeze-start`
    `FREEZE_BLOCKED` no invocation renames for the cycle: the merged
    relation starts `freeze-start` only from `NONE`, and thaw is not
    selectable, because that cycle has no `move.done` and no `early` binding
    (D2.2.1 §17.3.5). After a `thaw` `FREEZE_BLOCKED` the merged relation
    allows a new owner-authorized `thaw`; it is a new authorization, not a
    retry, and repeats every check under a new authority: it renames back
    once only on a new observation of A, completes without a rename on B
    (R5), and gives `FREEZE_BLOCKED` on C.
- Completion records. In a `journal/v3` cycle the store capability accepts
  `move.done`, `thaw.move.done`, `revoke.done` and `thaw.done` for a durable
  intent only when the paired filesystem capability has, in this invocation,
  re-read that intent as under "Intent authority" and itself observed the
  completed state for it: case (b) or case B with both parents flushed for a
  rename intent, or every object of the range at its post-value and flushed
  for a range intent. A completion record is never derived from an earlier
  claim, a stored record or the effect's assertion that a step succeeded.
  `move.abort` and `end` are not gated this way: they are written from the
  effect's own observation, so that `move.abort` means no rename took place
  rests on trusted code (D2.2.1 Theorem N-7 (iv)). This is a stated
  trusted-code residual: PR-4f must narrow it through verified observation
  and platform enforcement, and no PR removes it without its own owner
  decision. Once the FsCap authority is poisoned or sealed, the store
  capability refuses them as it refuses every write; the check and the put
  are one hold of the authority's lock (LP-PUT).
- Not exempt: publication, including any mode, owner or ACL change of
  `staging/<cid>/`, of a quarantine directory or of an ancestor, and the
  later hand-over of a staging tree; a rename or move across a protection
  boundary; a revoke or thaw change; any change to a pre-existing protected
  object; the final replacement of the root; any other mode change; and any
  other create. Each still needs its durable journal intent or is forbidden.
  The exemption is from per-object journaling only: it does not bypass the
  capability's bindings, its open state or any other check.
- Privacy. The construction steps keep everything they create private. What
  is checked and what is assumed are separate:
  - checked by the capability and its backend at the point of effect,
    through descriptors it holds and the identities the OS reports: the
    parent's identity and attributes, the attributes of each created object,
    the exclusive create, the entry lists above, the devices before a rename
    and the `staging/<cid>/` state before the mode change;
  - checked by the broker at start, before any verb and outside any
    capability operation: the ancestor rule for `owner/` and the quarantine
    root (every ancestor root-owned and not writable by another uid; the
    chain through a root-owned directory with mode 0700 and no ACL);
  - assumed from the activation prerequisites, not proved at run time: no
    open by id and no `link()` through `/.vol` (P33), no directory hard link
    by a non-root uid (P34), no mount, firmlink or alias that reaches the
    chain (P5, P30 and the broker-start volume checks), and the P35 holder
    condition of AP-3. Neither the capability nor a Python object
    restriction or static scan proves that no external process holds a
    descriptor;
  - the PR-4c in-memory model establishes no OS property; it checks the
    first group against modelled attributes only.
- Crash and replay. The exemption adds no journal kind, and `journal/v3` is
  unchanged. Whether the move or the move back happened is decided only by
  the journal and by observing the object whose identity is `root_id`;
  whether a restore was built is decided only by a `restore_build` artifact
  that names the attempt. A complete or partial quarantine directory,
  `linked` directory or staging attempt is never evidence that an effect
  started or completed. A retry of `prepare_quarantine` while the journal
  still ends at `ep.done` accepts only the existing state above; a partial
  attempt is never attested and never reused, and a resume builds a new
  attempt. Nothing is deleted or cleaned up automatically, and no restore is
  claimed successful without the later trusted-verifier attestation.
- Every irreversible operation (the rename, each attribute change, the
  rename back) is performed only after a journal intent that names it is
  durable, as established under "Intent authority", and only when the object
  shows the intent's pre-value. The capability derives the values itself
  from the cycle's durable records and captured originals; the caller
  supplies no value, path or identity. Both renames' endpoints are functions
  of the pinned layout and of the identities verified as above, and the
  rename back is the exact inverse of the rename. A durable `move.intent` or
  `thaw.move.intent` permits its rename only if its names equal the pinned
  names, its `root` is the bound `root_id`, and its parent keys equal the
  identities verified under "Subsequent use". An intent never grants an
  endpoint, path or root. An attribute change's range is its durable range
  intent's `[lo, hi]`, within the EP's `[0, N]`. A `revoke.intent` carries
  `attr`, `lo`, `hi` and `originals`, and its `originals` must equal the
  `originals[attr]` digest of the EP manifest that the durable `ep.done`
  names. A `thaw.intent` carries only `attr`, `lo` and `hi`, never
  `originals`: it authorizes a change only when its range is the next one in
  the merged grammar's thaw plan (every revoke range, done or open,
  attribute order owner, mode, acl, flags, each attribute's ranges
  ascending), so it equals exactly one preceding `revoke.intent` range of
  the same attribute, and that `revoke.intent`'s `originals` equals the EP
  manifest's `originals[attr]`. Pre- and post-values come only from that EP,
  its `originals` blob for the attribute, re-read and re-hashed against that
  digest, and the fixed targets, never from a value or digest the effect
  supplies. Irreversible operations exist only for `journal/v3` cycles. A
  second rename after a completion or abort record is refused.
- Forbidden: any delete, any rename other than the two above, any copy in
  place of a rename, any hard or symbolic link, clone or exchange, any
  change to an object that is not one of the cycle's objects or the verb's
  newly created staging objects, any path-string write, and any write to a
  store.
- Failure: a store failure, of a read or a write of the store machinery,
  closes both capabilities and seals their FsCap authority in the hold of
  the failing operation ("Sealing"). If no `seq 0` record of the cycle
  exists, the effect may complete with `freeze-start` `NONE(store_space)` or
  `NONE(store_error)` without bindings; otherwise W reports `UNFINISHED`
  with the F intent pending. A filesystem failure that can be classified
  into a result of the verb's F relation completes with that result after
  the journal records the result requires; an unclassifiable one leaves
  `UNFINISHED`. A resume never repeats an irreversible step that its own
  durable journal shows completed.
- Model limits. The PR-4c model establishes the authority's shared
  synchronization, its monotonic state, classification and terminal
  poisoning, content veto, sealing, result reconciliation and intent
  authentication, as in-process properties of trusted code. It establishes
  no real `fsync` or durability, no process or privilege isolation, no
  absence of external descriptor holders, no protection from hostile Python
  in the same interpreter and no real atomic rename. AP-1, AP-2, AP-3 and
  P35 remain activation prerequisites, and protected mode remains the
  constant refusal.

### Unchanged
- The CommitTxn and the constant protected-mode refusal keep their meaning.
  `IngestCapability` keeps its meaning except that each one is bound to the
  evidence kinds of one producer (a separate evidence-contract change,
  recorded with this decision); that binding takes effect with the PR that
  wires the producers, and until then `IngestCapability` is as merged.
- PR-4c implements both capabilities and their checks as inert in-memory
  models: in-memory stores and an in-memory filesystem model that only tests
  construct. No production filesystem backend exists, and no real store or
  filesystem operation exists before the PRs that add them, each under its
  own review.
- The filesystem capability's backend comes from one place in `fscap`. In
  PR-4c only the test-support model can be installed there; no world
  attribute, configuration value, request field or environment variable
  selects a backend, and an installed broker has none.
- Test-only direct seeding of test-owned stores stays: malformed,
  foreign-cycle or forked objects for negative tests, and setup records such
  as `set_established`. Such a fixture is never routed through a capability
  when that would stop it representing the corruption under test. Seeding
  helpers live only in test code outside the installed file set and are
  never production authority; production writes stay capability-gated.
- In the world, the journal, artifact and blob stores are instances of the
  new interfaces, so no store that accepts a CommitTxn put remains for those
  namespaces. `RecordStore`, `Gated` and the other merged stores keep their
  behaviour, including `RecordStore.put` for existing synthetic code and
  test setup. Only the new journal, artifact and blob interfaces refuse the
  CommitTxn and accept the store capability. Their in-memory model records,
  per key, whether a put or `make_durable` has made the object durable.

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
directories. Git does not read a regular file at one of these names as a
directory; what it does instead depends on the name and, at `objects` and
`refs`, on the file's execute bit. Measured with Git 2.38.0 and Apple Git
2.54.0 on APFS; this is observed behavior, not a guarantee Git makes. At
`objects` or `refs` a file without an execute bit makes Git refuse the
repository (`not a git repository`). With mode 0755 Git accepts the
repository and the reads fail instead: at `objects` the object reads fail
(`bad object HEAD` or `failed to read object ... Not a directory`), at
`refs` the current branch has no commits. At `refs/heads` with a symbolic
HEAD it reports that the current branch has no commits, with or without an
execute bit and also when the branch exists only in `packed-refs`; with a
detached HEAD it carries on. At `objects/pack` it prints `unable to open
object pack directory` and carries on. At the other names it ignores the
file. No safety claim depends on which of these happens. As a
symbolic link to a directory outside the slot, Git 2.38 and 2.54 follow it
(measured for `objects/pack`, `objects/info`, `info` and `refs`; the rule
below does not depend on the other names being followed): a forged pack there
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
  classifier cannot detect the link at all. The classifier cannot enforce
  this without a typed manifest; the PR-4 entry criterion (NB-16, under
  "PR-4 prerequisite" above) is what closes it.

### packed-refs: the subset whose Git reading was measured (B-1, N-1, N-2, 2026-11-17)

Git resolves a packed ref by binary search, trusts a `^` line as the ref's
peeled value and, under some headers, trusts its absence. The classifier
used to keep the first of two lines for one name and accept any `^` line,
so Git 2.38 and 2.54 could resolve HEAD to an unverified commit on the
second line while the slot reached D4. Measured with both versions in
disposable repositories:

- **Repeated name.** Git resolves the second line in both orders without a
  header, and either line depending on layout under `sorted`. `git fsck` on
  2.38 is silent. Any repeated name, also with identical values, is
  `malformed_packed` (Q4 `INCOMPLETE`, Q5 `NOT_ELIGIBLE`). A loose ref still
  takes precedence over a packed line of the same name.
- **Peel lines.** `git show-ref -d`, `describe` and, on 2.54,
  `for-each-ref %(*objectname)` report a forged `^` value, including one
  that names an absent object or follows a commit ref. A `^` line is
  accepted only directly after a ref line and only when it equals Git's
  peel of the verified value (the final target of its tag chain). Under a
  `fully-peeled` header, or `peeled` for `refs/tags/`, Git 2.38 treats a
  missing `^` line as "not a tag" (2.54 peels anyway), so a tag without one
  is refused there; without such a header Git peels the tag itself and a
  missing line is accepted. A line a loose ref shadows is never read by Git
  and is not checked.
- **Header and order.** The header is accepted only as the first line, only
  with Git's prefix `# pack-refs with: ` (Git 2.54 refuses the file
  otherwise) and only when its traits are printable ASCII and spaces. Git
  reads the traits as a C string, so with `sorted\0` or `fully-peeled\0`
  Git still sees the trait while a byte-wise split does not; the review of
  498983ad reached D4 that way. Under a `sorted` trait the names must be in increasing byte
  order: with an unsorted file Git 2.38 no longer found HEAD's branch, and
  `for-each-ref` listed the packed value of a name a loose ref shadows,
  whose object the classifier never verified.
- **Ref names** in `packed-refs`, a loose symbolic ref and HEAD follow Git's
  check-ref-format: no `..`, `@{`, empty or `.`-leading component, trailing
  `.` or `/`, component ending in `.lock`, control character, space or any
  of ``~^:?*[\``. Git refuses such names (`bad ref`, `packed refname is
  dangerous`, `your current branch appears to be broken`). Stricter by
  CodeGraph policy: names are printable ASCII, the PR-2 observer's packed-ref
  subset, so a non-ASCII name Git accepts (`refs/heads/fünf`) is still
  `malformed_packed` or `malformed_ref`; and HEAD must name
  `refs/heads/[A-Za-z0-9._/-]` as the observer requires, otherwise
  `git_head`. Both only over-refuse. Loose ref file names are not checked
  here: Git ignores a broken one, and HEAD and symbolic refs cannot point at
  it because their targets are checked.
- **Case alias of a `.lock` file.** `refs/heads/x.LOCK` is a valid name for
  Git, which compares `.lock` byte-wise. On APFS Git opens it as a captured
  `refs/heads/x.lock`, which the classifier skips as residue: Git 2.38 and
  2.54 then resolved the ref, or HEAD, to that file's unverified commit
  while the slot reached D4 (review of ce630f01). Any packed name,
  symbolic-ref target or HEAD target that folds (APFS/HFS+ rules) onto a
  captured `refs/**.lock` path is `unknown`.

### Scope of the D4 recovery claim (NB-15, 2026-11-17)

D4 states that Git, reading the restored slot, sees for HEAD and every
captured ref exactly the objects the classifier verified as loose objects.
Two preconditions apply. Git must discover the restored slot's own `.git`
(the B-1 and B-2 rules below refuse what PR-3b can see; PR-4 must hold
NB-16 items 4 and 5). On a case-insensitive volume a ref Git reads under
another case is the exception above ("Recorded head on a case-insensitive
volume"). It states nothing else. In particular:

- **Unrelated packs.** A pack whose index lists no reachable id is ignored
  (R4.2), even when its contents are forged. `git fsck` on such a restored
  slot fails (exit 4 for a forged unrelated entry) while D4 holds. Neither
  that nor `git gc`, `repack` or `prune` succeeding is part of the claim.
  Changing this needs a separate owner decision.
- **Unreachable objects** (loose or packed), reflogs, the index, hooks and
  other administrative files are preserved as raw bytes only. They are not
  verified.
- **Names other than HEAD and captured refs.** Pseudorefs (`MERGE_HEAD`,
  `ORIG_HEAD`, `FETCH_HEAD`), `worktrees/*/HEAD`, sequencer state and Git's
  short-name lookup are outside the claim. A top-level `$GIT_DIR/main` file
  makes `git rev-parse main` warn that the name is ambiguous and resolve to
  that file's commit (measured, Git 2.38 and 2.54), while `refs/heads/main`
  is still the verified one. By Git's documented behavior (not measured
  here) a `MERGE_HEAD` becomes a parent of the next `git commit`.
- **Evidence limits.** The measurements in this section: Git 2.38.0 and
  Apple Git 2.54.0 on APFS, macOS only. Linux ran only the gitdir discovery
  tests of the next section (hosted CI, Git 2.56.1). Windows runtime, other
  Git versions and HFS+ were not exercised.

### Gitdir name and restorable gitdir (B-1, B-2, 2026-11-17)

Git accepts a directory as a gitdir only when it has a valid `HEAD`, an
`objects` directory and a `refs` directory (`is_git_directory`). When it
rejects the slot's `.git`, discovery goes on to check the slot root itself
as a bare repository. The review of 5cfd2721 reached D4 twice with Git
2.38.0 and Apple Git 2.54.0 resolving HEAD to a forged commit of a bare
repository at the slot root, while the classifier had verified the gitdir.

- **B-1: `refs` must be restorable.** The manifest records files, not
  directories, and a restore creates a directory only above a captured
  file. After `git pack-refs --all` no file remains under `.git/refs/`, the
  restored `.git` has no `refs` and Git rejects it. A repository (top level
  or nested) with no captured path below `refs/` is the unsupported
  finding `refs_dir`: Q3 `INCOMPLETE`, Q4 unchanged (every byte is
  preserved), Q5 `NOT_ELIGIBLE`. A gitlink file has no gitdir here and is
  not checked. `objects` needs no rule of its own: Q5 `ELIGIBLE` requires
  HEAD's commit as a captured loose object below `objects/`. Any captured
  file below `refs/` is enough, a loose ref or `.lock` residue: Git does
  not need `refs/heads` or `refs/tags` (measured: the empty `refs/tags` of
  an ordinary repository is not captured and Git still opens the restore).
  The rule therefore holds only if restore recreates every captured file
  below `refs/`, residue included, or records `refs` as a directory
  (NB-16 item 4).
- **B-2: exact `.git` only (amends R2 K3).** R2 K3 locates a gitdir by
  every spelling a case-insensitive volume or Git's `is_hfs_dotgit` may
  treat as `.git`. That stays the detection rule: such a path is never
  worktree content or a tree entry. Acceptance is now narrower. Only the
  exact bytes `.git` are a repository's gitdir; a root whose gitdir is any
  other spelling (`.GIT`, `.Git`, `.g<U+200C>it` and the other HFS+-ignorable
  code points) is `unknown`: Q4 `INCOMPLETE`, Q5 `NOT_ELIGIBLE`, and its
  other findings are still reported. Measured: Git 2.38.0 and 2.54.0 on
  case-insensitive APFS do not open the seven measured
  ignorable-code-point spellings (U+200C, U+200D, U+200E, U+200F, U+202A,
  U+206F, U+FEFF) as `.git`, and open the forged root instead; they do
  open `.GIT` and `.Git`. On a case-sensitive Linux volume (hosted CI, Git
  2.56.1) Git opens none of the nine. The other ignorable code points were
  not measured; they are refused all the same. `.git` is ASCII, so Unicode
  normalization yields no other spelling of it.
- **Accepted over-refusals** until PR-4 meets NB-16 items 4 and 5:
  - a valid repository whose refs are all in `packed-refs` (after `git
    pack-refs --all` or `git gc`; the latter is already
    `NOT_ELIGIBLE(packed)` under R4.2), top level or nested;
  - a valid repository whose gitdir is spelled `.GIT` or `.Git` on a
    case-insensitive volume, where Git does open it.
- **Evidence limits.** Git 2.38.0 and Apple Git 2.54.0 on case-insensitive
  APFS (macOS), and Git 2.56.1 on the hosted Linux runner for the real-Git
  tests. HFS+, case-sensitive APFS and Windows were not exercised
  and no equivalent behavior is claimed for them.

## Not in PR-1

PR-2..PR-5, RS-1, RS-4, NL1–NL6, L1–L8, P1–P24, real file I/O, any account,
sudo rule, LaunchDaemon or live pool access.
