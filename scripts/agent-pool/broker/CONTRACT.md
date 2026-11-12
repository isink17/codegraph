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

## Not in PR-1

PR-2..PR-5, RS-1, RS-4, NL1–NL6, L1–L8, P1–P24, real file I/O, any account,
sudo rule, LaunchDaemon or live pool access.
