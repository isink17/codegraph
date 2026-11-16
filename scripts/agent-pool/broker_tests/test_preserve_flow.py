"""PR-3b end to end through the merged PR-3a/PR-3a.1 `invoke`, gate, handoff
interface and release check, with the real provider (`preserve.Preservation`)
and a test-only PR-4 effect stand-in that writes test-owned stores. Every
expected value is recomputed here; negative traces assert that no unintended
effect or persistent write happened."""

import os
import types
import unittest
from unittest import mock

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
from broker import canonical, freeze, gitclass, invocation, journal, o5contract as oc, preserve
from broker._testsupport import o5
from broker.refusal import Code, Outcome, Refused
from broker.stores import Crash
from preserve_world import OTHER, SLOT, outcome, rel

PRES_KEYS = {"head", "complete", "inventory", "preserved", "artifact"}


class Base(unittest.TestCase):
    world_kw = {}

    def setUp(self):
        self.w = pw.World(**self.world_kw)
        self.addCleanup(self.w.close)

    def ok(self, *verbs, slot=SLOT):
        self.w.ok(*verbs, slot=slot)

    def rejected(self, verb, detail, intent, **kw):
        """`check_fresult` rejects: UNFINISHED, the completion unwritten, the
        intent pending, no `refreshed` record."""
        calls = []
        real = self.w.prov.check_fresult

        def spy(*a):
            try:
                return real(*a)
            except ValueError as e:
                calls.append(str(e))
                raise
        self.w.prov.check_fresult = spy
        r = self.w.run(verb, **kw)
        self.w.prov.check_fresult = real
        self.assertEqual((outcome(r), calls, self.w.row()[1]), (("UNFINISHED",), [detail], intent))
        return r

    def refused(self, verb, code, detail, **kw):
        before = (self.w.env.snapshot(), pw.store_bytes(self.w), len(self.w.fx.calls))
        r = self.w.run(verb, **kw)
        self.assertEqual(outcome(r), ("REFUSED", code.value, detail))
        self.assertEqual((self.w.env.snapshot(), pw.store_bytes(self.w), len(self.w.fx.calls)), before)
        return r

    def stuck_at_move(self):
        """Rejected after the move: one irreversible step, no revoke, the
        barrier held, every later verb refused with no effect or write, and
        the F resume re-runs the idempotent effect only to be rejected again."""
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")
        cid = self.w.cid()
        self.assertEqual(self.w.fx.irrev, [(cid, "move")])
        self.assertEqual([r["kind"] for r in self.w.st()["records"]], ["start", "move.done"])
        self.assertEqual(self.w.row(), ("P7", "MOVE_INTENT"))
        self.assertEqual(self.w.release(), DENIED)
        for verb in ("freeze-start", "freeze-continue", "quarantine", "quiescence", "snapshot",
                     "verify-preservation", "preservation", "ready", "thaw"):
            with self.subTest(verb):
                self.refused(verb, Code.VERB_NOT_ALLOWED_IN_STATE, "")
        self.rejected("resume-step", "fresult_rejected:not_preservable", "MOVE_INTENT")
        self.assertEqual(self.w.fx.irrev, [(cid, "move")])
        self.assertEqual(self.w.release(), DENIED)
        self.assertEqual((self.w.last_doc("preservation"), self.w.last_doc("ready")), (None, None))


# ------------------------------------------------------------------ positive traces

class Positive(Base):
    def test_complete_preservation_to_ready_and_d4(self):
        self.w.to_p13()
        self.w.handoff()
        self.assertEqual(rel(self.w.release()), "D3")
        self.ok("snapshot", "verify-preservation", "preservation", "ready")
        self.assertEqual(self.w.row(), ("P18", "VERIFIED"))
        self.assertEqual(rel(self.w.release()), "D4")
        doc = self.w.last_doc("preservation")
        self.assertEqual(PRES_KEYS & set(doc), PRES_KEYS)
        _, coid = pw.git_repo()
        self.assertEqual(doc["head"], coid)
        s = self.w.st()
        dirty = sorted(p.hex() for p in self.w.fx.tree(SLOT) if b".git" not in p.split(b"/"))
        vb = self.w.prov._r
        manifest = canonical.digest(pw.protocol.parse_doc(vb.artifact(s["cid"], s["fs_m"])))
        self.assertEqual(doc["artifact"], {"manifest": s["fs_m"], "blobs": s["fs_bs"]})
        self.assertEqual(manifest, s["fs_m"])
        self.assertEqual(doc["inventory"]["dirty"], [preserve.item_id("dirty", s["fs_m"], 0, dirty)])
        self.assertEqual(doc["inventory"]["unpublished_commits"],
                         [preserve.item_id("unpublished_commits", s["fs_m"], 0,
                                           ["i" + canonical.sha256(b"commit:" + coid.encode())[:63]])])
        self.assertTrue(all(v == {"verified": True, "to": "raw:" + s["fs_m"]} for v in doc["preserved"].values()))
        self.assertEqual(self.w.fx.irrev, [(s["cid"], "move")] + [(s["cid"], "revoke:" + a) for a in journal.ATTRS])

    def test_unsupported_git_with_complete_raw_bytes(self):
        """P-C4 as amended by R4.2 (supersedes P2's READY): packed-only objects
        keep raw COMPLETE, Q3 INCOMPLETE and Q4 COMPLETE, but Q5 is
        NOT_ELIGIBLE, so freeze-start is rejected after the move."""
        self.w.fx.trees[SLOT] = pw.git_repo(variant=("packed_only",))[0]
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")
        s = self.w.st()
        m = pw.protocol.parse_doc(self.w.res.artifact(s["cid"], s["ep_m"]))
        self.assertEqual(m["result"], "COMPLETE")
        files = {p[5:]: d for p, d in self.w.fx.tree(SLOT).items() if p.startswith(b".git/")}
        self.assertEqual(gitclass.questions({b"": gitclass.classify(b"", files)}, "COMPLETE")[1:4],
                         ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"))

    def test_missing_object_beside_a_junk_pack_is_not_preservable(self):
        """B-3: a pack file name never stands in for the missing object."""
        files = pw.git_repo(variant=("missing_object",))[0]
        files[b".git/objects/pack/junk.pack"] = b"not a pack"
        self.w.fx.trees[SLOT] = files
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")

    def test_missing_object_beside_an_unrelated_valid_pack_is_not_preservable(self):
        files = pw.git_repo(variant=("missing_object",))[0]
        oid, z = pw.gobj(b"blob", b"unrelated\n")
        pack, idx = pw.pack_pair({oid: pw.zlib.decompress(z)})
        files[b".git/objects/pack/pack-u.pack"], files[b".git/objects/pack/pack-u.idx"] = pack, idx
        self.w.fx.trees[SLOT] = files
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")

    def test_shallow_repository_is_not_recovery_eligible(self):
        self.w.fx.trees[SLOT] = pw.git_repo(variant=("shallow",))[0]
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")

    def test_alternates_make_preservation_incomplete(self):
        self.w.fx.trees[SLOT] = pw.git_repo(variant=("alternates",))[0]
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")

    def _nested(self, name, *variant):
        files, _ = pw.git_repo(worktree={b"work.txt": b"nested agent work\n"}, variant=variant, prefix=b"sub/")
        if "alternates" in variant:
            files = {p: d for p, d in files.items() if b"/objects/" not in p or p.endswith(b"alternates")}
        return {p.replace(b"sub/.git", b"sub/" + name, 1): d for p, d in files.items()}

    def _external_nested_refused(self, name):
        self.w.fx.trees[SLOT] = {**pw.git_repo()[0], **self._nested(name, "alternates")}
        self.ok("adopt", "request")
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")
        self.assertEqual(self.w.fx.irrev, [(self.w.cid(), "move")])

    def test_nested_external_objects_under_dot_git(self):
        self._external_nested_refused(b".git")

    def test_nested_external_objects_under_upper_case_gitdir(self):
        """B-2: `sub/.GIT` is the nested gitdir on case-insensitive APFS."""
        self._external_nested_refused(b".GIT")

    def test_nested_external_objects_under_mixed_case_gitdir(self):
        self._external_nested_refused(b".Git")

    def test_plain_nested_upper_case_repository_reaches_d4_outside_the_dirty_inventory(self):
        self.w.fx.trees[SLOT] = {**pw.git_repo()[0], **self._nested(b".GIT")}
        self.w.to_ready()
        self.assertEqual(rel(self.w.release()), "D4")
        s, doc = self.w.st(), self.w.last_doc("preservation")
        dirty = sorted(p.hex() for p in self.w.fx.tree(SLOT) if not gitclass.is_git_path(p))
        self.assertIn(b"sub/work.txt".hex(), dirty)
        self.assertFalse(any(b"/.GIT/" in bytes.fromhex(p) for p in dirty))
        self.assertEqual(doc["inventory"]["dirty"], [preserve.item_id("dirty", s["fs_m"], 0, dirty)])

    def test_owner_thaw_from_p7_then_d2(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.ok("thaw")
        self.assertEqual(self.w.cycles()[-1].closure, "thaw_complete")
        self.ok("cancel")
        self.assertEqual(rel(self.w.release()), "D2")
        cid = self.w.cid()
        thawed = [x for c, x in self.w.fx.irrev if c == cid and x.startswith("thaw:")]
        self.assertEqual(sorted(thawed), sorted("thaw:" + a for a in journal.ATTRS + ("move",)))

    def test_two_cycles_have_distinct_identities_and_artifacts(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        c1 = self.w.cid()
        self.ok("freeze-start", "freeze-continue", "quarantine", "quiescence")
        c2 = self.w.cid()
        self.assertNotEqual(c1, c2)
        self.assertEqual([c.cycle_id for c in self.w.cycles()], [c1, c2])
        self.w.handoff()
        self.ok("snapshot", "verify-preservation", "preservation", "ready")
        self.assertEqual(rel(self.w.release()), "D4")
        keys = self.w.w.o5.stores["journals"].keys()
        self.assertTrue(any(k.startswith(f"journal/{c1}/") for k in keys))
        self.assertTrue(any(k.startswith(f"journal/{c2}/") for k in keys))
        self.assertEqual(self.w.fx.irrev.count((c2, "move")), 1)

    def test_snapshot_crash_resumes_without_a_second_snapshot(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.fx.k["crash_snapshot"] = 1
        r = self.w.run("snapshot")
        self.assertEqual(outcome(r), ("UNFINISHED",))
        self.assertEqual(self.w.row(), ("P13", "SNAPSHOT_INTENT"))
        self.ok("resume-step")
        self.assertEqual(self.w.row(), ("P13", "SNAPSHOTTED"))
        self.assertEqual([v for v, _ in self.w.fx.calls].count("snapshot"), 2)
        self.ok("verify-preservation", "preservation", "ready")

    def test_completed_effect_without_completion_resumes_with_identical_bindings(self):
        """S5 / N-BX1-4: the effect completed, the completion is missing; the
        resume returns the same bindings and repeats no irreversible step."""
        self.ok("adopt", "request", "freeze-start")
        self.w.fx.k["crash"] = {"freeze-continue"}
        self.assertEqual(outcome(self.w.run("freeze-continue")), ("UNFINISHED",))
        irrev = list(self.w.fx.irrev)
        self.ok("resume-step")
        self.assertEqual(self.w.row()[1], "REVOKED")
        self.assertEqual(self.w.fx.irrev, irrev)

    def test_interrupted_thaw_applies_each_step_once(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.w.fx.k["crash_thaw_after"] = 2
        self.assertEqual(outcome(self.w.run("thaw")), ("UNFINISHED",))
        self.assertEqual(self.w.row()[1], "THAW_INTENT")
        self.ok("resume-step")
        cid = self.w.cid()
        steps = [x for c, x in self.w.fx.irrev if c == cid and x.startswith("thaw:")]
        self.assertEqual(sorted(steps), sorted("thaw:" + a for a in journal.ATTRS + ("move",)))
        self.assertEqual(self.w.cycles()[-1].closure, "thaw_complete")

    def test_ready_crash_at_p16_and_p17_reaches_p18(self):
        for point, row in (("library:after_intent", "P16"), ("library:before_marker_rename", "P17")):
            w = pw.World()
            try:
                w.to_verified()
                w.ok("preservation")
                w.env.faults.point = point
                with self.assertRaises(Crash):
                    w.run("ready")
                w.env.faults.point = None
                self.assertEqual(w.row()[0], row)
                self.assertEqual(rel(w.release()), "D3")
                w.ok("resume-step")
                self.assertEqual(w.row()[0], "P18")
                self.assertEqual(rel(w.release()), "D4")
            finally:
                w.close()

    def test_preservation_crash_resumes_through_d_pres_r(self):
        self.w.to_verified()
        self.w.env.faults.point = "library:after_intent"
        with self.assertRaises(Crash):
            self.w.run("preservation")
        self.w.env.faults.point = None
        self.assertEqual(self.w.row()[0], "P14")
        self.ok("resume-step", "ready")
        self.assertEqual(self.w.row()[0], "P18")

    def test_interrupted_quarantine_resumes_without_a_pr3b_cell(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.w.env.faults.point = "library:after_intent"
        with self.assertRaises(Crash):
            self.w.run("quarantine")
        self.w.env.faults.point = None
        self.ok("resume-step", "quiescence")


# ------------------------------------------------------------------ cross-cycle and binding counterexamples

class CrossCycle(Base):
    def first_cycle(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        return dict(self.w.st())

    def test_previous_cycle_early_manifest(self):
        s1 = self.first_cycle()
        self.w.fx.k["subst"]["freeze-start"] = lambda r: r._replace(bindings=dict(r.bindings, early=s1["early"]))
        self.rejected("freeze-start", "fresult_rejected:barrier_binding", "MOVE_INTENT")
        self.w.fx.k["subst"] = {}

    def test_previous_cycle_journal(self):
        s1 = self.first_cycle()
        self.ok("freeze-start")
        self.w.fx.k["subst"]["freeze-continue"] = lambda r: r._replace(bindings=dict(r.bindings, journal=s1["head"]))
        self.rejected("freeze-continue", "fresult_rejected:journal_mismatch", "REVOKE_INTENT")

    def test_previous_cycle_verify_binding(self):
        self.first_cycle()
        self.ok("freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.w.handoff()
        self.ok("snapshot")
        other = self.w.fx.obj(self.w.st(), {"header": dict(self.w.fx.header(self.w.st()), cycle_id="1" * 64),
                                            "cycle_id": "1" * 64, "manifest": self.w.st()["fs_m"],
                                            "blobs": self.w.st()["fs_bs"], "reverified": True, "sealed_at": 1})
        self.w.fx.k["subst"]["verify-preservation"] = lambda r: r._replace(bindings=dict(r.bindings, verify=other))
        self.rejected("verify-preservation", "fresult_rejected:cycle_binding", "VERIFY_INTENT")

    def test_wrong_freeze_start_context(self):
        self.ok("adopt", "request")
        self.w.fx.k["hdr"] = lambda h: dict(h, start_ctx="5" * 64)
        self.rejected("freeze-start", "fresult_rejected:cycle_binding", "MOVE_INTENT")

    def test_wrong_generation_header(self):
        self.ok("adopt", "request")
        self.w.fx.k["hdr"] = lambda h: dict(h, g_pre=h["g_pre"] + 1)
        self.rejected("freeze-start", "fresult_rejected:barrier_binding", "MOVE_INTENT")

    def test_wrong_cycle_identity_in_header(self):
        self.ok("adopt", "request")
        self.w.fx.k["hdr"] = lambda h: dict(h, cycle_id="7" * 64)
        self.rejected("freeze-start", "fresult_rejected:cycle_binding", "MOVE_INTENT")

    def test_wrong_root_and_cross_slot_source_identity(self):
        """Barrier basis: PR-3a's own root check and D-SNAP both bind the root;
        handoff basis: H-VAL fails first."""
        self.w.to_p13()
        for root in ("9" * 64, o5.h(f"root|{OTHER}")):
            r = self.w.run("snapshot", extra={"source_identity": {"root_id": root}})
            self.assertEqual(outcome(r), ("REFUSED", "STATE_UNKNOWN", "barrier_binding"))
        self.w.handoff()
        for root in ("9" * 64, o5.h(f"root|{OTHER}")):
            r = self.w.run("snapshot", extra={"source_identity": {"root_id": root}})
            self.assertEqual(outcome(r), ("REFUSED", "QUIESCENCE_NOT_ESTABLISHED", "handoff_invalid"))
        self.assertEqual(self.w.row(), ("P13", "REVOKED"))
        self.assertEqual(self.w.fx.calls[-1][0], "freeze-continue")

    def test_revoked_journal_point_required_at_snapshot(self):
        """N-R41-3: a SNAPSHOTTED result binding an older journal point of this
        cycle (before `revoked`) is rejected."""
        self.w.to_p13()
        self.w.handoff()
        old = canonical.digest(self.w.st()["records"][1])
        self.w.fx.k["subst"]["snapshot"] = lambda r: r._replace(bindings=dict(r.bindings, journal=old))
        self.rejected("snapshot", "fresult_rejected:journal_mismatch", "SNAPSHOT_INTENT")

    def test_revoked_record_whose_prev_skips_a_record(self):
        self.ok("adopt", "request", "freeze-start")
        self.w.fx.k["fork_revoked"] = True
        self.rejected("freeze-continue", "fresult_rejected:journal_mismatch", "REVOKE_INTENT")

    def test_revoked_result_must_bind_the_journal_escaped_set(self):
        self.ok("adopt", "request", "freeze-start")
        wrong = canonical.digest({"escaped": ["61"]})
        self.w.fx.k["subst"]["freeze-continue"] = lambda r: oc.FResult(r.value, r.reason, dict(r.bindings,
                                                                                              escaped=wrong))
        self.rejected("freeze-continue", "fresult_rejected:escaped", "REVOKE_INTENT")

    def test_verified_result_must_seal_the_snapshotted_manifest(self):
        """A verify binding over another final manifest of the same cycle."""
        self.w.to_p13()
        self.w.handoff()
        self.ok("snapshot")

        def other_manifest(r):
            s, fx = self.w.st(), self.w.fx
            files = {**fx.tree(SLOT), b"README": b"another snapshot\n"}
            _, fm, fbs = fx.manifest(s, "final", files)
            v = fx.obj(s, {"header": fx.header(s), "cycle_id": s["cid"], "manifest": fm, "blobs": fbs,
                           "reverified": True, "sealed_at": self.w.w.clock()})
            return oc.FResult(r.value, r.reason, dict(r.bindings, verify=v))
        self.w.fx.k["subst"]["verify-preservation"] = other_manifest
        self.rejected("verify-preservation", "fresult_rejected:artifact_mismatch", "VERIFY_INTENT")

    def test_corrupt_journal_record(self):
        self.ok("adopt", "request", "freeze-start")
        self.w.fx.k["corrupt_revoked"] = True
        self.rejected("freeze-continue", "fresult_rejected:journal_mismatch", "REVOKE_INTENT")


class Preservation(Base):
    def test_incomplete_raw_inventory(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.fx.k["result"] = "INCOMPLETE"
        self.rejected("snapshot", "fresult_rejected:raw_incomplete", "SNAPSHOT_INTENT")

    def test_mutated_tree_at_snapshot(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.fx.k["fs_mutate"] = True
        self.rejected("snapshot", "fresult_rejected:mutated", "SNAPSHOT_INTENT")

    def test_missing_blob_at_verify(self):
        """A blob lost before the effect fails H-VAL (ProtectedBlobs); one lost
        during the verify effect is rejected by `check_fresult`."""
        self.w.to_p13()
        self.w.handoff()
        self.ok("snapshot")
        readme = canonical.sha256(self.w.fx.tree(SLOT)[b"README"])
        arts = self.w.w.o5.stores["artifacts"].items

        def lose(r):
            del arts[f"blob/{readme}"]
            return r
        self.w.fx.k["subst"]["verify-preservation"] = lose
        self.rejected("verify-preservation", "fresult_rejected:artifact_mismatch", "VERIFY_INTENT")

    def test_missing_ep_blob_refuses_snapshot_before_the_effect(self):
        """N-3: on the barrier basis (no handoff yet) a missing EP blob refuses
        D-SNAP with no effect call, instead of failing after the snapshot."""
        self.w.to_p13()
        del self.w.w.o5.stores["artifacts"].items["blob/" + canonical.sha256(self.w.fx.tree(SLOT)[b"README"])]
        self.refused("snapshot", Code.STATE_UNKNOWN, "artifact_mismatch")

    def test_missing_blob_refuses_verify_before_the_effect(self):
        self.w.to_p13()
        self.ok("snapshot")
        del self.w.w.o5.stores["artifacts"].items["blob/" + canonical.sha256(self.w.fx.tree(SLOT)[b"README"])]
        self.refused("verify-preservation", Code.STATE_UNKNOWN, "artifact_mismatch")

    def test_failure_row_past_revoked_needs_a_revoked_journal_point(self):
        """N-1: SNAPSHOT_FAILED binding a journal head older than `revoked` is
        rejected; the current head is accepted."""
        self.w.to_p13()
        stale = canonical.digest(self.w.st()["records"][1])
        self.w.fx.k["subst"]["snapshot"] = lambda r: oc.FResult("SNAPSHOT_FAILED", "io", {"journal": stale})
        self.rejected("snapshot", "fresult_rejected:journal_mismatch", "SNAPSHOT_INTENT")

    def test_failure_row_with_the_current_journal_head_commits(self):
        self.w.to_p13()
        head = self.w.st()["head"]
        self.w.fx.k["subst"]["snapshot"] = lambda r: oc.FResult("SNAPSHOT_FAILED", "io", {"journal": head})
        self.ok("snapshot")
        self.assertEqual(self.w.row()[1], "SNAPSHOT_FAILED")

    def test_non_bytes_blob_refuses_release_without_an_exception(self):
        """Q3e: a store value that is not bytes refuses H-VAL and the discharge."""
        self.w.to_ready()
        readme = canonical.sha256(self.w.fx.tree(SLOT)[b"README"])
        self.w.w.o5.stores["artifacts"].items[f"blob/{readme}"] = "hello\n"
        self.assertNotIn(rel(self.w.release()), ("D3", "D4", "D3H", "D4H"))

    def test_missing_original_blob_makes_thaw_unsafe(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        acl = self.w.st()["originals"]["acl"]
        del self.w.w.o5.stores["artifacts"].items[f"blob/{acl}"]
        self.refused("thaw", Code.PROVENANCE_INSUFFICIENT, "thaw_not_safe(data_unavailable)")

    def test_irreversible_freeze_start(self):
        self.ok("adopt", "request")
        self.w.fx.k["drop_original"] = True
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")

    def test_premature_ready(self):
        self.w.to_verified()
        r = self.w.run("ready")
        self.assertEqual(outcome(r)[0], "REFUSED")
        self.assertEqual(self.w.row()[0], "P13")

    def test_ready_refuses_a_mutated_tree_and_a_wrong_head(self):
        self.w.to_verified()
        self.ok("preservation")
        for data, detail in ((self.w.final_data(projection="0" * 64), "mutated"),
                             (self.w.final_data(head_resolved="b" * 40), "head")):
            r = self.w.run("ready", extra={"final_observation": data})
            self.assertEqual(outcome(r), ("REFUSED", "NOT_CLEAN", detail))
        self.ok("ready")


# ------------------------------------------------------------------ escaped files (OD-R4-1)

def _old_policy(real=gitclass.questions):
    """The superseded R2 K7 rule (packed objects recovery eligible), used only
    to reach later gates and prove each refuses unverified packed evidence."""
    def questions(repos, q1):
        feats, q3, q4, q5, reasons, head = real(repos, q1)
        if q4 == "COMPLETE" and head is not None and not set(feats) & gitclass.UNSUPPORTED:
            q5 = "ELIGIBLE"
        return feats, q3, q4, q5, reasons, head
    return mock.patch.object(gitclass, "questions", questions)


class PackedObjects(Base):
    """Owner amendment R4.2: a reachable object known only through a pack
    index is not recovery evidence. The refusal happens in `check_fresult`
    after the freeze-start move effect ran (the EP manifest is that effect's
    output); nothing later can commit, discharge or replay past it."""

    def forged(self):
        loose, coid = pw.git_repo()
        toid, _ = pw.head_tree(loose, coid)
        files = {p: d for p, d in loose.items() if p != pw.loose_key(toid)}
        return pw.with_pack(files, {toid: b"blob 4\0junk"})

    def test_forged_pack_never_reaches_ready_or_d4(self):
        self.w.fx.trees[SLOT] = self.forged()
        self.stuck_at_move()

    def test_mixed_loose_and_genuine_packed_objects(self):
        loose, coid = pw.git_repo()
        toid, traw = pw.head_tree(loose, coid)
        self.w.fx.trees[SLOT] = pw.with_pack({p: d for p, d in loose.items() if p != pw.loose_key(toid)},
                                             {toid: traw})
        self.stuck_at_move()

    def test_unrelated_pack_beside_loose_objects_reaches_d4(self):
        oid, z = pw.gobj(b"blob", b"unrelated\n")
        self.w.fx.trees[SLOT] = pw.with_pack(pw.git_repo()[0], {oid: pw.zlib.decompress(z)}, b"u")
        self.w.to_ready()
        self.assertEqual(rel(self.w.release()), "D4")

    def test_second_cycle_does_not_inherit_eligibility(self):
        """Cycle 1 (loose, eligible) thaws; the agent repacks; cycle 2 is
        classified from its own EP bytes. Every manifest's stored `questions`
        claims ELIGIBLE and is never read."""
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        c1 = self.w.cid()
        self.w.fx.trees[SLOT] = pw.git_repo(variant=("packed_only",))[0]
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")
        self.assertNotEqual(self.w.cid(), c1)
        self.assertEqual(self.w.release(), DENIED)

    def test_quarantine_refuses_packed_evidence_on_its_own(self):
        self.w.fx.trees[SLOT] = self.forged()
        with _old_policy():
            self.ok("adopt", "request", "freeze-start")
        self.ok("freeze-continue")
        self.refused("quarantine", Code.PROVENANCE_INSUFFICIENT, "not_preservable(packed)")

    def test_preservation_and_ready_refuse_packed_evidence_on_their_own(self):
        """Past the earlier gates under the superseded rule, D-PRES still
        recomputes Q5 and refuses, so READY and D4 stay unreachable; D3 (the
        barrier discharge that never claims preservation) is unaffected and
        still re-hashes the protected copy of the forged pack."""
        self.w.fx.trees[SLOT] = self.forged()
        with _old_policy():
            self.w.to_verified()
        self.refused("preservation", Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(recovery)")
        self.refused("ready", Code.VERB_NOT_ALLOWED_IN_STATE, "")
        self.assertEqual(self.w.row(), ("P13", "VERIFIED"))
        self.w.observe()
        self.assertEqual(rel(self.w.release()), "D3")
        pack = next(d for p, d in self.w.fx.tree(SLOT).items() if p.endswith(b".pack"))
        self.w.w.o5.stores["artifacts"].items[f"blob/{canonical.sha256(pack)}"] = pack[:-1]
        self.w.observe()
        self.assertEqual(self.w.release(), DENIED)

    def test_packed_repository_written_by_git(self):
        """A real repository: loose as Git committed it reaches D4; after
        `git gc --aggressive` (deltified objects, no loose ones) it is not
        recovery eligible."""
        from gitfixture import GIT, Lab
        if GIT is None:
            self.skipTest("git not installed")
        lab = Lab()
        self.addCleanup(lab.cleanup)
        r = lab.path("repo")
        lab.git(lab.root, "init", "-q", r)
        text = [b"line %d of a file long enough to deltify\n" % i for i in range(200)]
        for n in range(4):
            text[n * 7] = b"edit %d\n" % n
            lab.write(r, "f.txt", b"".join(text))
            lab.git(r, "add", "f.txt")
            lab.git(r, "commit", "-q", "-m", str(n))

        def tree():
            out = {}
            for d, _, names in os.walk(r):
                for n in names:
                    p = os.path.join(d, n)
                    with open(p, "rb") as f:
                        out[os.path.relpath(p, r).encode()] = f.read()
            return out

        loose = tree()
        lab.git(r, "gc", "-q", "--aggressive")
        packed = tree()
        self.assertFalse(any(p.startswith(b".git/objects/") and len(p) == 54 for p in packed))
        idx = next(p for p in packed if p.endswith(b".idx"))
        listing = lab.git(r, "verify-pack", "-v", os.path.join(r, idx.decode())).stdout
        self.assertRegex(listing, rb"chain length = [1-9]")
        for files, want in ((loose, ("VERIFIED", "COMPLETE", "ELIGIBLE")),
                            (packed, ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"))):
            self.assertEqual(gitclass.questions(gitclass.repositories(files), "COMPLETE")[1:4], want)
        self.w.fx.trees[SLOT] = loose
        self.w.to_ready()
        self.assertEqual(rel(self.w.release()), "D4")
        self.w.fx.trees[OTHER] = packed
        self.ok("adopt", "request", slot=OTHER)
        self.assertEqual(outcome(self.w.run("freeze-start", slot=OTHER)), ("UNFINISHED",))
        self.assertEqual(self.w.row(OTHER), ("P7", "MOVE_INTENT"))


def _superseded_links():
    """The walker of PR #546 at 2ab37d65 (ids only, no structure or type
    check), used only to reach later gates and prove each refuses a
    malformed reachable object on its own."""
    def links(r, typ, body):
        out = []
        if typ in (b"commit", b"tag"):
            for line in body.split(b"\n"):
                if not line:
                    break
                key, _, val = line.partition(b" ")
                if key in ((b"tree", b"parent") if typ == b"commit" else (b"object",)):
                    out.append((val.decode(), None))
        elif typ == b"tree":
            i = 0
            while i < len(body):
                sp, nul = body.find(b" ", i), body.find(b"\0", i)
                if body[i:sp] != b"160000":
                    out.append((body[nul + 1:nul + 21].hex(), None))
                i = nul + 21
        return out
    return mock.patch.object(gitclass, "_links", links)


def _with_head(files, raw_objects, head):
    """`files` plus hand-built loose objects, `refs/heads/main` moved to `head`."""
    out = {**files, b".git/refs/heads/main": head.encode() + b"\n"}
    for raw in raw_objects:
        out[pw.loose_key(pw.hashlib.sha1(raw).hexdigest())] = pw.zlib.compress(raw)  # noqa: S324
    return out


def _commit(body):
    return b"commit %d\0" % len(body) + body


class MalformedObjects(Base):
    """A hash-consistent loose object whose structure or link types are not
    Git's is not semantic verification (Q3) and not recovery evidence (Q5).
    Like R4.2, the first refusal is `check_fresult` after the freeze-start
    move effect ran; D-QUAR and D-PRES refuse on their own."""
    IDENT = b"author a <a@x> 0 +0000\ncommitter a <a@x> 0 +0000\n"

    def cases(self):
        loose, coid = pw.git_repo()
        toid, _ = pw.head_tree(loose, coid)
        blob = pw.gobj(b"blob", b"hello\n")[0]
        no_tree = _commit(self.IDENT + b"\nno tree header\n")
        to_blob = _commit(b"tree " + blob.encode() + b"\n" + self.IDENT + b"\nm\n")
        entry = b"100644 README\0" + bytes.fromhex(blob)
        dup = b"tree %d\0" % (2 * len(entry)) + entry + entry
        dup_id = pw.hashlib.sha1(dup).hexdigest()  # noqa: S324
        on_dup = _commit(b"tree " + dup_id.encode() + b"\n" + self.IDENT + b"\nm\n")
        parent_tree = _commit(b"tree " + toid.encode() + b"\nparent " + toid.encode() + b"\n" + self.IDENT + b"\nm\n")
        sha = lambda raw: pw.hashlib.sha1(raw).hexdigest()  # noqa: E731,S324
        good = _commit(b"tree " + toid.encode() + b"\n" + self.IDENT + b"\nm\n").split(b"\0", 1)[1]
        zero_size = b"commit 0%d\0" % len(good) + good
        continued = _commit(b"tree " + toid.encode() + b"\n hidden\n" + self.IDENT + b"\nm\n")
        overflow = _commit(b"tree " + toid.encode() + b"\nauthor a <a@x> 9999999999999999999 +0000\n"
                           b"committer a <a@x> 0 +0000\n\nm\n")
        return {"leading-zero object size": _with_head(loose, [zero_size], sha(zero_size)),
                "continuation inside the mandatory headers": _with_head(loose, [continued], sha(continued)),
                "author time past Git's limit": _with_head(loose, [overflow], sha(overflow)),
                "missing tree": _with_head(loose, [no_tree], sha(no_tree)),
                "tree names a blob": _with_head(loose, [to_blob], sha(to_blob)),
                "duplicate tree entries": _with_head(loose, [dup, on_dup], sha(on_dup)),
                "parent names a tree": _with_head(loose, [parent_tree], sha(parent_tree))}

    def test_never_reach_ready_or_d4(self):
        for case, files in self.cases().items():
            with self.subTest(case):
                self.w = pw.World()
                self.addCleanup(self.w.close)
                self.w.fx.trees[SLOT] = files
                self.stuck_at_move()

    def test_quarantine_refuses_on_its_own(self):
        self.w.fx.trees[SLOT] = self.cases()["missing tree"]
        with _superseded_links():
            self.ok("adopt", "request", "freeze-start")
        self.ok("freeze-continue")
        self.refused("quarantine", Code.PROVENANCE_INSUFFICIENT, "not_preservable(corrupt_object)")

    def test_preservation_and_ready_refuse_on_their_own(self):
        """Past every earlier gate under the superseded walker, D-PRES still
        recomputes Q3-Q5 and refuses; READY and D4 stay unreachable. D3 never
        claims preservation and still re-hashes the protected copy."""
        files = self.cases()["tree names a blob"]
        self.w.fx.trees[SLOT] = files
        with _superseded_links():
            self.w.to_verified()
        self.refused("preservation", Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(raw)")
        self.refused("ready", Code.VERB_NOT_ALLOWED_IN_STATE, "")
        self.assertEqual(self.w.row(), ("P13", "VERIFIED"))
        self.w.observe()
        self.assertEqual(rel(self.w.release()), "D3")
        head = files[b".git/refs/heads/main"][:40].decode()
        obj = self.w.fx.tree(SLOT)[pw.loose_key(head)]
        self.w.w.o5.stores["artifacts"].items[f"blob/{canonical.sha256(obj)}"] = obj[:-1]
        self.w.observe()
        self.assertEqual(self.w.release(), DENIED)

    def test_second_cycle_is_classified_from_its_own_bytes(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        c1 = self.w.cid()
        self.w.fx.trees[SLOT] = self.cases()["missing tree"]
        self.rejected("freeze-start", "fresult_rejected:not_preservable", "MOVE_INTENT")
        self.assertNotEqual(self.w.cid(), c1)
        self.assertEqual(self.w.release(), DENIED)

    def test_gitlink_without_submodule_state_reaches_d4(self):
        """R4 §5.2 (B25): a gitlink entry alone is benign. The submodule commit
        lives in another object store, so it is recorded (Q2 `gitlink`) and
        not followed; a checked-out submodule is a nested repository or a
        gitlink file and is classified as such."""
        loose, coid = pw.git_repo()
        toid, traw = pw.head_tree(loose, coid)
        body = traw.split(b"\0", 1)[1] + b"160000 sub\0" + bytes.fromhex("5" * 40)
        tree = b"tree %d\0" % len(body) + body
        c = _commit(b"tree " + pw.hashlib.sha1(tree).hexdigest().encode() + b"\n" + self.IDENT  # noqa: S324
                    + b"\nm\n")
        files = _with_head(loose, [tree, c], pw.hashlib.sha1(c).hexdigest())  # noqa: S324
        feats, q3, q4, q5, _, _ = gitclass.questions(gitclass.repositories(files), "COMPLETE")
        self.assertEqual((q3, q4, q5, "gitlink" in feats), ("VERIFIED", "COMPLETE", "ELIGIBLE", True))
        self.w.fx.trees[SLOT] = files
        self.w.to_ready()
        self.assertEqual(rel(self.w.release()), "D4")


class Escaped(Base):
    def test_non_empty_escaped_set_refuses_quarantine_and_keeps_the_barrier(self):
        self.w.fx.k["escaped"] = [b"README".hex()]
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.refused("quarantine", Code.QUIESCENCE_NOT_ESTABLISHED, "escaped_present")
        self.assertEqual(self.w.row()[0], "P7")
        self.assertNotIn(rel(self.w.release()), ("D1", "D2"))

    def test_unclassifiable_escaped_set(self):
        self.w.fx.k["escaped"] = [b"not-in-the-manifest".hex()]
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.refused("quarantine", Code.PROVENANCE_INSUFFICIENT, "escaped_unclassifiable")

    def test_owner_thaw_after_the_escaped_refusal(self):
        self.w.fx.k["escaped"] = [b"README".hex()]
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.refused("quarantine", Code.QUIESCENCE_NOT_ESTABLISHED, "escaped_present")
        self.ok("thaw", "cancel")
        self.assertEqual(rel(self.w.release()), "D2")

    def test_stop_refuses_thaw(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        self.w.env.run("stop", reason=harness.reason())
        n = len(self.w.fx.calls)
        self.assertEqual(outcome(self.w.run("thaw"))[:2], ("REFUSED", "STOPPED"))
        self.assertEqual(len(self.w.fx.calls), n)


# ------------------------------------------------------------------ handoff and discharge

class Handoff(Base):
    def test_unselected_enforcement_record_is_unreadable(self):
        self.w.to_p13()
        self.w.observe()
        cyc = self.w.cycles()[-1]
        doc = o5._owner_doc(self.w.env, "containment-handoff", slot=SLOT, op_id="op1", slot_generation=cyc.g_pre,
                            cycle_id=cyc.cycle_id, barrier_id=cyc.barrier_id)
        pre, sel = freeze.basis_for(self.w.w, SLOT, "handoff", doc=doc)
        good = self.w.prov.handoff_establish(pre, sel.public(), None)
        self.assertEqual((good.passed, good.reasons), (True, []))
        chain = o5.chain(self.w.env, cyc.barrier_id)
        for other, why in ((canonical.digest(chain.authorized), "barrier_binding"),
                           ("a" * 64, "enforcement_record_unavailable")):
            pub = sel.public()
            pub["provider_inputs"] = sorted([k, other if k == "enforcement_record" else v]
                                            for k, v in pub["provider_inputs"])
            bad = self.w.prov.handoff_establish(pre, pub, None)
            self.assertEqual((bad.passed, bad.reasons), (False, [why]))
            with self.assertRaises(Refused):
                freeze.guard(bad, "H-EST", pre.digest, sel, oc.H_EST_KINDS)

    def test_supporting_holder_scan_is_not_decisive(self):
        self.w.to_p13()
        self.w.ov["holder_role"] = "supporting"
        self.assertEqual(self.w.try_handoff(), ("QUIESCENCE_NOT_ESTABLISHED", "handoff_failed"))

    def test_holder_scans_closer_than_two_seconds(self):
        self.w.to_p13()
        real = pw.ev

        def close(env, kind, slot, data, at, **kw):
            return real(env, kind, slot, data, at + (3 if kind == "holder_scan_1" else 0), **kw)
        with mock.patch.object(pw, "ev", close):
            self.assertEqual(self.w.try_handoff(), ("QUIESCENCE_NOT_ESTABLISHED", "handoff_failed"))

    def test_mounts_baseline_absent(self):
        self.w.close()
        self.w = pw.World(est_detail={k: v for k, v in pw.EST_DETAIL.items() if k != "mounts_baseline"})
        self.w.to_p13()
        self.assertEqual(self.w.try_handoff(), ("QUIESCENCE_NOT_ESTABLISHED", "handoff_failed"))

    def test_writable_protected_object_fails_establishment_and_validation(self):
        self.w.to_p13()
        bad = {"artifact_isolation": {"objects": [[d, 0, 0o644, 0, 1, True] for d in self.w._bound(SLOT)]}}
        self.assertEqual(self.w.try_handoff(extra=bad), ("QUIESCENCE_NOT_ESTABLISHED", "handoff_failed"))
        self.w.handoff()
        r = self.w.run("snapshot", extra=bad)
        self.assertEqual(outcome(r), ("REFUSED", "QUIESCENCE_NOT_ESTABLISHED", "handoff_invalid"))

    def test_agent_activity_after_release_cannot_veto_h_val(self):
        """R4.1 §5.3: H-VAL reads nothing an agent controls after an authorized
        release: processes, channels, setuid files and external mounts."""
        self.w.to_p13()
        self.w.handoff()
        agent = {"uid_process_scan": {"uids": [o5.AGENT_UID], "processes": [[601, 601, 601, 4242]]},
                 "agent_channels": {"channels": {"agent_crontab": "present"}},
                 "setuid_writer_scan": {"files": [[pw.SETUID, 0o4755, False]]},
                 "mounts_external": {"mounts": "0" * 64, "baseline": pw.MOUNTS}}
        self.w.ov.update(agent)
        self.ok("snapshot", "verify-preservation", "preservation", "ready")

    def test_evidence_changed_between_v_and_r(self):
        self.w.to_p13()
        self.w.handoff()
        r = self.w.run("snapshot", confirm_hook=lambda: self.w.observe())
        self.assertEqual(outcome(r)[:2], ("REFUSED", "CONTEXT_CHANGED"))
        self.assertEqual(self.w.row(), ("P13", "REVOKED"))

    def test_discharge_without_superseded_or_on_a_foreign_cycle(self):
        self.w.to_p13()
        self.w.handoff()
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D3")
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        cur = self.w.cycles()[-1]
        view = types.SimpleNamespace(slot=SLOT, row="P13", f="REVOKED", current=cur)
        self.assertIsNotNone(self.w.prov.discharge(view, chain, pre, sel.public()))
        foreign = types.SimpleNamespace(slot=SLOT, row="P13", f="REVOKED", current=cur._replace(cycle_id="1" * 64))
        self.assertIsNone(self.w.prov.discharge(foreign, chain, pre, sel.public()))
        self.assertIsNone(self.w.prov.discharge(types.SimpleNamespace(slot=SLOT, row="P18", f="REVOKED",
                                                                      current=cur), chain, pre, sel.public()))
        self.assertIsNone(self.w.prov.discharge(view, chain._replace(superseded={}), pre, sel.public()))

    def test_overlapping_invalid_chain_is_never_read(self):
        """NB-8 residual (PR-4 veto): PR-3b reads barrier records of the holder
        chain only, by exact key; another chain is never consulted."""
        self.w.to_p13()
        self.w.handoff()
        bogus = "d" * 64
        self.w.w.o5.stores["barriers"].items[f"barrier/{bogus}/{0:010d}"] = b"{}"
        reads = []
        real = self.w.res.barrier

        def spy(bid, seq):
            reads.append(bid)
            return real(bid, seq)
        self.w.res.barrier = spy
        self.ok("snapshot")
        self.assertTrue(reads)
        self.assertEqual(set(reads), {self.w.env.barrier_id})


class Invariants(Base):
    """Direct calls on PR-3a-built contexts captured from real invocations."""

    def capture(self, name):
        got = []
        real = getattr(self.w.prov, name)

        def spy(*a):
            got.append(a)
            return real(*a)
        setattr(self.w.prov, name, spy)
        return got, real

    def release_basis(self):
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D3")
        return pre, sel.public()

    def test_check_fresult_binds_the_given_identity(self):
        """A valid result of this cycle passed with a foreign identity raises."""
        self.w.to_p13()
        self.w.handoff()
        got, real = self.capture("check_fresult")
        self.ok("snapshot")
        ctx, verb, res, cid = got[0]
        real(ctx, verb, res, cid)
        with self.assertRaisesRegex(ValueError, "fresult_rejected:cycle_binding"):
            real(ctx, verb, res, "1" * 64)

    def test_d_thaw_refuses_under_stop_itself(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        got, real = self.capture("decide")
        self.ok("thaw")
        ctx, sel, view = got[0]
        stopped = ctx.pre.__class__(oc._CSEAL, **dict(ctx.pre._f, stop=True))
        d = real(oc.ContainmentContext(oc._CSEAL, pre=stopped, basis=ctx.basis, superseded=None, verdict=None),
                 sel, view)
        self.assertEqual((d.result, d.code), ("REFUSE", Code.STOPPED))

    def test_cycle_identity_is_re_derived(self):
        self.w.to_p13()
        self.w.handoff()
        pre, _ = self.release_basis()
        self.assertEqual(self.w.prov._ids(pre)[0], pre.cycle_id)
        for k, v in (("cycle_id", "1" * 64), ("freeze_start_auth", "2" * 64), ("g_pre", 7)):
            bad = pre.__class__(oc._CSEAL, **dict(pre._f, **{k: v}))
            with self.assertRaises(journal.Mismatch) as e:
                self.w.prov._ids(bad)
            self.assertEqual(e.exception.detail, "barrier_binding", k)

    def test_resume_intent_requires_the_original_cell(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.fx.k["crash_snapshot"] = 1
        self.w.run("snapshot")
        got, real = self.capture("decide")
        self.ok("resume-step")
        ctx, sel, view = got[0]
        self.assertEqual(real(ctx, sel, view).result, "PROCEED")
        real_doc = self.w.res.burned_doc

        def forged(d):
            doc = real_doc(d)
            if doc and doc.get("verb") == "snapshot":
                doc = dict(doc, o5=dict(doc["o5"], decision={"cell": "D-VERIFY", "digest": "0" * 64}))
            return doc
        with mock.patch.object(self.w.res, "burned_doc", forged):
            d = real(ctx, sel, view)
        self.assertEqual((d.result, d.detail), ("REFUSE", "original_mismatch"))

    def test_d_pres_r_compares_the_original(self):
        self.w.to_verified()
        self.w.env.faults.point = "library:after_intent"
        with self.assertRaises(Crash):
            self.w.run("preservation")
        self.w.env.faults.point = None
        fields = preserve.Preservation._pres_fields
        with mock.patch.object(preserve.Preservation, "_pres_fields",
                               lambda self, *a: dict(fields(self, *a), head="b" * 40)):
            self.refused("resume-step", Code.STATE_UNKNOWN, "original_mismatch")
        self.ok("resume-step")
        self.assertEqual(self.w.row()[0], "P15")

    def test_validation_binds_the_superseded_record(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.observe()
        pre, pub = self.release_basis()
        kinds = oc.discharge_kinds("D3", pre.f)
        self.w.prov._validate(pre, pub, kinds)
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        rec, digest = chain.superseded[SLOT]
        seq = rec["seq"]
        forged = canonical.canon(dict(rec, detail=dict(rec["detail"], cycle_id="1" * 64)))
        real = self.w.res.barrier
        with mock.patch.object(self.w.res, "barrier", lambda bid, n: forged if n == seq else real(bid, n)):
            sub = dict(pub, provider_inputs=sorted([k, canonical.sha256(forged) if k == "superseded" else v]
                                                   for k, v in pub["provider_inputs"]))
            with self.assertRaises(journal.Mismatch) as e:
                self.w.prov._validate(pre, sub, kinds)
        self.assertEqual(e.exception.detail, "barrier_binding")

    def test_discharge_kind_rules(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.observe()
        pre, pub = self.release_basis()
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        cur = self.w.cycles()[-1]
        p18 = types.SimpleNamespace(slot=SLOT, row="P18", f="VERIFIED", current=cur)
        self.assertIsNone(self.w.prov.discharge(p18, chain, pre, pub))
        view = types.SimpleNamespace(slot=SLOT, row="P13", f="REVOKED", current=cur)
        self.assertEqual(self.w.prov.discharge(view, chain, pre, pub).kind, "D3")
        other = pre.__class__(oc._CSEAL, **dict(pre._f, release_target="9" * 64))
        self.assertIsNone(self.w.prov.discharge(view, chain, other, pub))

    def test_d4_requires_the_ready_closure(self):
        self.w.to_ready()
        self.w.observe()
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D4")
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        cur = self.w.cycles()[-1]
        view = types.SimpleNamespace(slot=SLOT, row="P18", f="VERIFIED", current=cur)
        self.assertEqual(self.w.prov.discharge(view, chain, pre, sel.public()).kind, "D4")
        pending = types.SimpleNamespace(slot=SLOT, row="P18", f="VERIFIED", current=cur._replace(closure="ready_pending"))
        self.assertIsNone(self.w.prov.discharge(pending, chain, pre, sel.public()))

    def test_resolver_returns_only_burned_documents(self):
        doc = canonical.canon({"v": 1, "verb": "freeze-start", "slot": SLOT})
        d = canonical.sha256(doc)
        self.w.w.authz.docs[d] = doc
        self.assertIsNone(self.w.res.burned_doc(d))
        o5.burn(self.w.env, {"v": 1, "verb": "freeze-start", "slot": SLOT})
        self.assertIsNotNone(self.w.res.burned_doc(d))


# ------------------------------------------------------------------ protected blob integrity (PR #546 B-1)

DENIED = (Code.VERB_NOT_ALLOWED_IN_STATE.value, "barrier_in_use(preservation_dependent)")


class ProtectedBlobs(Base):
    """R4 §6.3 V-2, R2 §3.2, R4 §8.4: H-VAL and every D3/D3H/D4/D4H discharge
    re-read and re-hash every protected EP blob (entries and originals) and
    every bound FS blob on each call; no earlier result stands in for it."""

    def fresh(self):
        self.w = pw.World()
        self.addCleanup(self.w.close)

    @property
    def arts(self):
        return self.w.w.o5.stores["artifacts"].items

    def content(self, name=b"README"):
        return canonical.sha256(self.w.fx.tree(SLOT)[name])

    def original(self):
        return self.w.st()["originals"]["acl"]

    def tamper(self, d, how):
        key = f"blob/{d}"
        if how == "delete":
            del self.arts[key]
        elif how == "corrupt":
            self.arts[key] = b"tampered\n"
        else:
            self.arts[key] = next(v for k, v in sorted(self.arts.items()) if k.startswith("blob/") and k != key)

    def denied(self, bid=None):
        """The whole release refuses: no discharge, no effect, no write."""
        self.w.observe()
        before = (pw.store_bytes(self.w), len(self.w.fx.calls), list(self.w.fx.irrev))
        self.assertEqual(self.w.release(bid), DENIED)
        self.assertEqual((pw.store_bytes(self.w), len(self.w.fx.calls), self.w.fx.irrev), before)

    def at_p13(self):
        self.w.to_p13()
        self.w.handoff()

    def direct(self, kind="D3", row="P13"):
        """(handoff_validate, discharge) on the release basis PR-3a builds now."""
        self.w.observe()
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind=kind)
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        view = types.SimpleNamespace(slot=SLOT, row=row, f=pre.f, current=self.w.cycles()[-1])
        return (lambda: self.w.prov.handoff_validate(pre, sel.public(), None),
                lambda: self.w.prov.discharge(view, chain, pre, sel.public()))

    def early_objects(self):
        s = self.w.st()
        b = pw.protocol.parse_doc(self.w.res.artifact(s["cid"], s["early"]))
        m = pw.protocol.parse_doc(self.w.res.artifact(s["cid"], b["manifest"]))
        idx = pw.protocol.parse_doc(self.w.res.artifact(s["cid"], m["entry_index"][0]))
        return s["cid"], b, m, idx

    # ---- A. H-VAL through the merged invoke

    def test_damage_matrix_over_every_stage(self):
        """Every stage from REVOKED to READY, a content blob (EP and FS) and an
        original-attribute blob (EP only), each damage including a value that
        is not bytes: H-VAL and the discharge refuse, the release grants
        nothing and nothing raises."""
        stages = {"revoked": (), "snapshotted": ("snapshot",), "verified": ("snapshot", "verify-preservation"),
                  "ready": ("snapshot", "verify-preservation", "preservation", "ready")}
        damages = {"delete": None, "corrupt": lambda b: b"tampered\n", "truncate": lambda b: b[:-1],
                   "extend": lambda b: b + b"\0", "not bytes": lambda b: b.decode("latin-1")}
        for stage, verbs in stages.items():
            for target in ("content", "original"):
                for how, change in damages.items():
                    with self.subTest(stage=stage, target=target, how=how):
                        self.fresh()
                        self.at_p13()
                        self.ok(*verbs)
                        key = f"blob/{self.content() if target == 'content' else self.original()}"
                        if change is None:
                            del self.arts[key]
                        else:
                            self.arts[key] = change(self.arts[key])
                        self.denied()
                        validate, discharge = self.direct(*(("D4", "P18") if stage == "ready" else ()))
                        self.assertEqual((validate().passed, validate().reasons), (False, ["artifact_mismatch"]))
                        self.assertIsNone(discharge())

    def test_h_val_rehashes_every_early_blob(self):
        """A1–A4, D26: a deleted, corrupted or digest-swapped EP content blob or
        original-attribute blob fails H-VAL; the snapshot effect never runs."""
        self.at_p13()
        self.ok("snapshot")
        for target in ("content", "original"):
            for how in ("delete", "corrupt", "swap"):
                with self.subTest(target=target, how=how):
                    self.fresh()
                    self.at_p13()
                    self.tamper(self.content() if target == "content" else self.original(), how)
                    self.refused("snapshot", Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_invalid")
                    validate, _ = self.direct()
                    self.assertEqual((validate().passed, validate().reasons), (False, ["artifact_mismatch"]))
                    self.assertEqual(self.w.row(), ("P13", "REVOKED"))

    def test_early_data_without_a_snapshot_is_evaluable(self):
        """A5, A6: before any FS manifest the EP set alone is validated; missing
        or corrupted manifest metadata refuses."""
        self.at_p13()
        validate, discharge = self.direct()
        self.assertEqual((validate().passed, discharge().kind), (True, "D3"))
        cid, b, m, idx = self.early_objects()
        other = self.arts[f"artifact/{cid}/{b['blobs']}"]
        for key, how, why in ((b["blobs"], "delete", "evidence_unavailable"),
                              (b["blobs"], "truncate", "evidence_unavailable"),
                              (b["manifest"], "swap", "artifact_mismatch")):
            with self.subTest(key=key, how=how):
                k = f"artifact/{cid}/{key}"
                good = self.arts[k]
                self.addCleanup(self.arts.__setitem__, k, good)
                if how == "delete":
                    del self.arts[k]
                else:
                    self.arts[k] = good[:-1] if how == "truncate" else other
                self.assertEqual((validate().passed, validate().reasons, discharge()), (False, [why], None))
                self.denied()
                self.arts[k] = good
        self.assertEqual(discharge().kind, "D3")

    # ---- B. D3 / D3H

    def test_d3_refused_over_a_damaged_protected_blob(self):
        """B7–B9, B12, B14: intact EP copy gives D3; a deleted or corrupted
        protected blob refuses the whole release with no write or effect."""
        self.at_p13()
        self.assertEqual(rel(self.w.release()), "D3")
        for target in ("content", "original"):
            for how in ("delete", "corrupt"):
                with self.subTest(target=target, how=how):
                    self.fresh()
                    self.at_p13()
                    self.tamper(self.content() if target == "content" else self.original(), how)
                    self.denied()
                    self.assertEqual(o5.chain(self.w.env, self.w.env.barrier_id).state, "ESTABLISHED")

    def test_final_snapshot_blob_before_verification(self):
        """B10: at SNAPSHOTTED, before any verified binding, a damaged FS blob
        refuses D3 and the verify effect."""
        for how in ("delete", "corrupt"):
            with self.subTest(how=how):
                self.fresh()
                self.at_p13()
                self.ok("snapshot")
                self.w.observe()
                self.assertEqual(rel(self.w.release()), "D3")
                self.tamper(self.content(), how)
                self.denied()
                self.refused("verify-preservation", Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_invalid")

    def test_d3h_after_a_historical_handoff(self):
        """B11: the historical kind runs the same validation."""
        for how in ("delete", "corrupt"):
            with self.subTest(how=how):
                self.fresh()
                self.at_p13()
                o5.release_chain(self.w.env, self.w.env.barrier_id)
                b2 = o5.synthetic_barrier(self.w.env, [SLOT, OTHER])
                self.assertEqual(rel(self.w.release(b2)), "D3H")
                self.tamper(self.original(), how)
                self.denied(b2)

    def test_another_slot_cannot_supply_a_replacement(self):
        """B13: a valid blob of another slot, present under its own digest or
        placed under this slot's digest, never satisfies this slot's set."""
        self.at_p13()
        mine = self.original()
        theirs = b"original|acl|" + OTHER.encode()
        self.arts[f"blob/{canonical.sha256(theirs)}"] = theirs
        del self.arts[f"blob/{mine}"]
        self.denied()
        self.arts[f"blob/{mine}"] = theirs
        self.denied()

    # ---- C. D4 / D4H

    def test_d4_refused_over_a_damaged_protected_blob(self):
        """C15–C17, C19: READY and its gate documents do not stand in for the
        bytes; the EP originals are checked at P18 as well."""
        self.w.to_ready()
        self.assertEqual(rel(self.w.release()), "D4")
        for target in ("content", "original"):
            for how in ("delete", "corrupt"):
                with self.subTest(target=target, how=how):
                    self.fresh()
                    self.w.to_ready()
                    self.assertEqual(self.w.row(), ("P18", "VERIFIED"))
                    self.tamper(self.content() if target == "content" else self.original(), how)
                    self.denied()
                    _, discharge = self.direct("D4", "P18")
                    self.assertIsNone(discharge())

    def test_d4h_after_a_historical_handoff(self):
        """C18."""
        for target in ("content", "original"):
            with self.subTest(target=target):
                self.fresh()
                self.w.to_ready()
                o5.release_chain(self.w.env, self.w.env.barrier_id)
                b3 = o5.synthetic_barrier(self.w.env, [SLOT])
                self.assertEqual(rel(self.w.release(b3)), "D4H")
                self.tamper(self.content() if target == "content" else self.original(), "corrupt")
                self.denied(b3)

    # ---- D. temporal and adversarial

    def test_no_earlier_result_is_reused(self):
        """C20, D21: validate, damage, validate again, restore."""
        self.at_p13()
        validate, discharge = self.direct()
        self.assertEqual((validate().passed, discharge().kind), (True, "D3"))
        d = self.content()
        good = self.arts[f"blob/{d}"]
        self.tamper(d, "corrupt")
        self.assertEqual((validate().passed, discharge()), (False, None))
        self.arts[f"blob/{d}"] = good
        self.assertEqual((validate().passed, discharge().kind), (True, "D3"))

    def test_protected_bytes_change_between_v_and_r(self):
        """D22: R re-runs H-VAL; nothing is written and the effect never runs."""
        self.at_p13()
        d = self.content()
        good = self.arts[f"blob/{d}"]
        self.w.observe()
        before = (self.w.env.snapshot(), pw.store_bytes(self.w), len(self.w.fx.calls))
        seen = []
        real = self.w.prov.handoff_validate

        def spy(*a):
            v = real(*a)
            seen.append(v.passed)
            return v
        self.w.prov.handoff_validate = spy
        r = self.w.run("snapshot", confirm_hook=lambda: self.tamper(d, "corrupt"))
        self.assertEqual(outcome(r), ("REFUSED", "QUIESCENCE_NOT_ESTABLISHED", "handoff_invalid"))
        self.assertEqual(seen, [True, False])
        self.arts[f"blob/{d}"] = good
        self.w.observe()
        self.assertEqual(len(self.w.fx.calls), before[2])
        self.assertEqual(self.w.row(), ("P13", "REVOKED"))
        self.ok("snapshot")

    def test_agent_original_mutation_is_not_a_veto(self):
        """D23: the agent-writable working tree changes after the handoff; the
        protected copies are intact, so H-VAL and D3 still pass."""
        self.at_p13()
        self.w.fx.trees[SLOT] = dict(self.w.fx.tree(SLOT))
        self.w.fx.trees[SLOT][b"README"] = b"rewritten by the agent\n"
        self.w.fx.trees[SLOT][b"new-file"] = b"agent\n"
        validate, discharge = self.direct()
        self.assertEqual((validate().passed, discharge().kind), (True, "D3"))
        self.assertEqual(rel(self.w.release()), "D3")

    def test_two_cycles_check_only_their_own_inventory(self):
        """D24: a blob only the thawed cycle bound is irrelevant to the next
        cycle; a blob only the current cycle bound is checked."""
        tree = dict(self.w.fx.tree(SLOT))
        self.w.fx.trees[SLOT] = dict(tree)
        self.w.fx.trees[SLOT][b"only-first"] = b"first cycle\n"
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        first = self.content(b"only-first")
        self.w.fx.trees[SLOT] = dict(tree)
        self.w.fx.trees[SLOT][b"only-second"] = b"second cycle\n"
        self.ok("freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.w.handoff()
        s = self.w.st()
        self.assertNotIn(first, pw.protocol.parse_doc(self.w.res.artifact(s["cid"], s["ep_bs"]))["set"])
        self.tamper(first, "corrupt")
        self.assertEqual(rel(self.w.release()), "D3")
        self.tamper(self.content(b"only-second"), "corrupt")
        self.denied()

    def test_previous_cycle_blob_set_binding(self):
        """D25: the previous cycle's valid early binding in the selection fails
        H-VAL and the discharge (the journal pins this cycle's EP)."""
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "thaw")
        s1 = dict(self.w.st())
        self.ok("freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.w.handoff()
        self.w.observe()
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D3")
        pub = sel.public()
        sub = dict(pub, provider_inputs=sorted([k, s1["early"] if k == "ep_manifest" else v]
                                               for k, v in pub["provider_inputs"]))
        v = self.w.prov.handoff_validate(pre, sub, None)
        self.assertEqual((v.passed, v.reasons), (False, ["barrier_binding"]))
        view = types.SimpleNamespace(slot=SLOT, row="P13", f=pre.f, current=self.w.cycles()[-1])
        chain = o5.chain(self.w.env, self.w.env.barrier_id)
        self.assertIsNone(self.w.prov.discharge(view, chain, pre, sub))
        self.assertEqual(self.w.prov.discharge(view, chain, pre, pub).kind, "D3")

    def test_one_blob_in_a_chunked_inventory(self):
        """D27, D29: many chunks over several index objects; one damaged blob,
        a missing chunk or a malformed index object refuses."""
        self.w.fx.k["per_chunk"], self.w.fx.k["per_index"] = 2, 2
        self.w.fx.trees[SLOT] = dict(self.w.fx.tree(SLOT))
        self.w.fx.trees[SLOT].update({b"f%02d" % i: b"file %d\n" % i for i in range(9)})
        self.at_p13()
        cid, _, m, idx = self.early_objects()
        self.assertGreater(len(m["entry_index"]), 1)
        validate, discharge = self.direct()
        self.assertEqual(discharge().kind, "D3")
        for key, mutate, why in ((f"blob/{self.content(b'f08')}", lambda v: b"tampered\n", "artifact_mismatch"),
                                 (f"artifact/{cid}/{idx['chunks'][-1]}", None, "evidence_unavailable"),
                                 (f"artifact/{cid}/{m['entry_index'][-1]}",
                                  lambda v: self.arts[f"artifact/{cid}/{m['entry_index'][0]}"], "artifact_mismatch")):
            with self.subTest(key=key):
                good = self.arts[key]
                self.addCleanup(self.arts.__setitem__, key, good)
                if mutate is None:
                    del self.arts[key]
                else:
                    self.arts[key] = mutate(good)
                self.assertEqual((validate().reasons, discharge()), ([why], None))
                self.denied()
                self.arts[key] = good
        self.assertEqual(discharge().kind, "D3")

    def test_omitted_blob_set_member(self):
        """D28: a blob set missing one member of entries ∪ originals is
        refused when the effect first binds it."""
        self.ok("adopt", "request")
        self.w.fx.k["bs_drop"] = True
        self.rejected("freeze-start", "fresult_rejected:artifact_mismatch", "MOVE_INTENT")

    def test_each_bound_set_is_rehashed_on_its_own(self):
        """The EP, FS and verified sets are each re-hashed. While FS ⊆ EP a
        member bound by only one set cannot occur, so one is forced here."""
        self.w.to_verified()
        validate, discharge = self.direct()
        self.assertEqual((validate().passed, discharge().kind), (True, "D3"))
        phantom = canonical.sha256(b"never stored")
        real = preserve._Cycle.binding
        for target in ("early", "final", "verify"):
            def extra(cy, d, phase, verify=False, target=target):
                b, m, es, bs = real(cy, d, phase, verify)
                hit = {"early": phase == "early", "final": phase == "final" and not verify, "verify": verify}[target]
                return b, m, es, sorted(bs + [phantom]) if hit else bs
            with self.subTest(target=target), mock.patch.object(preserve._Cycle, "binding", extra):
                self.assertEqual((validate().passed, validate().reasons, discharge()),
                                 (False, ["artifact_mismatch"], None))

    def test_the_complete_authenticated_set_is_rehashed(self):
        """D30 and the full set: on the positive path every discharge re-hashes
        exactly entries ∪ originals of each bound set, recomputed here."""
        self.at_p13()
        s = self.w.st()
        want = set(pw.protocol.parse_doc(self.w.res.artifact(s["cid"], s["ep_bs"]))["set"])
        self.assertEqual(want, {canonical.sha256(v) for v in self.w.fx.tree(SLOT).values()}
                         | set(s["originals"].values()))
        asked = []
        real = preserve._Cycle.blobs

        def spy(cy, digests):
            asked.append(set(digests))
            return real(cy, digests)
        with mock.patch.object(preserve._Cycle, "blobs", spy):
            self.assertEqual(rel(self.w.release()), "D3")
            self.assertIn(want, asked)
            self.ok("snapshot", "verify-preservation", "preservation", "ready")
            asked.clear()
            self.assertEqual(rel(self.w.release()), "D4")
            self.assertIn(want, asked)


# ------------------------------------------------------------------ BX-1 (PR #545 N1, mandatory)

class BX1(Base):
    def test_journal_trace_with_lost_intent_is_inconsistent(self):
        """A valid freeze-start burned, the cycle id established, the effect's
        seq-0 journal record written under journal/<cid>/, then the F WAL intent
        lost: classify_burn finds the trace, the burn is
        STATE_UNKNOWN(cycle_inconsistent), D1 is refused and no second effect runs."""
        self.ok("adopt", "request")
        self.w.fx.k["crash_seq0"] = 1
        self.assertEqual(outcome(self.w.run("freeze-start")), ("UNFINISHED",))
        (cid,) = [c for _, c in self.w.fx.calls]
        auth = [e["auth_digest"] for e in self.w.w.ledger.entries()
                if (self.w.res.burned_doc(e["auth_digest"]) or {}).get("verb") == "freeze-start"][-1]
        self.assertEqual(cid, oc.cycle_id(SLOT, 1, "op1", auth))
        self.assertIn(f"journal/{cid}/00000000", self.w.w.o5.stores["journals"].keys())
        self.w.w.fwal.entries[SLOT] = [e for e in self.w.w.fwal.entries[SLOT] if e.get("auth_digest") != auth]
        self.assertEqual(self.w.classify(), ["STATE_UNKNOWN(cycle_inconsistent)"])
        with self.assertRaises(Refused) as e:
            freeze.release_check(self.w.w, self.w.env.barrier_id)
        self.assertEqual((e.exception.code, e.exception.detail),
                         (Code.STATE_UNKNOWN, "STATE_UNKNOWN(cycle_inconsistent)"))
        r = self.w.run("freeze-start")
        self.assertEqual(outcome(r)[:2], ("REFUSED", "STATE_UNKNOWN"))
        self.assertEqual(len(self.w.fx.calls), 1)

    def test_pre_effect_crash_is_abandoned_and_releases_d1(self):
        """Control: burn committed, no F intent, no effect-produced object, the
        stores classifiable: ABANDONED_PRE_INTENT and D1."""
        self.ok("adopt", "request")
        real = invocation._f_cycle_id

        def skewed(gate, facts, verb, digest, doc):
            return real(gate, facts, verb, digest, dict(doc, slot_generation=doc["slot_generation"] + 1))
        with mock.patch.object(invocation, "_f_cycle_id", skewed):
            self.assertEqual(outcome(self.w.run("freeze-start")), ("UNFINISHED",))
        self.assertEqual(self.w.fx.calls, [])
        self.assertEqual(self.w.w.o5.stores["journals"].keys(), [])
        self.assertEqual(self.w.classify(), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(rel(self.w.release()), "D1")

    def test_unreadable_enumeration_is_never_empty(self):
        """A store listing that fails raises; it is never read as 'no trace'."""
        self.ok("adopt", "request")
        real = invocation._f_cycle_id

        def skewed(gate, facts, verb, digest, doc):
            return real(gate, facts, verb, digest, dict(doc, slot_generation=doc["slot_generation"] + 1))
        with mock.patch.object(invocation, "_f_cycle_id", skewed):
            self.w.run("freeze-start")

        def broken():
            raise Refused(Code.STATE_UNKNOWN, "artifact_inventory")
        with mock.patch.object(self.w.w.o5.stores["journals"], "keys", broken):
            with self.assertRaises(Refused):
                self.w.classify()


# ------------------------------------------------------------------ refusals and boundaries

class Boundaries(Base):
    def test_missing_provider(self):
        self.w.w.o5.preservation = None
        self.ok("adopt", "request")
        self.refused("freeze-start", Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")

    def test_protected_mode_refuses_everywhere(self):
        with mock.patch.object(oc, "mode", lambda world: oc.PROTECTED):
            for read in (lambda: self.w.res.journal("0" * 64, 0), lambda: self.w.res.burned_doc("0" * 64),
                         lambda: self.w.res.library(SLOT, "op1"), lambda: self.w.res.fwal(SLOT)):
                with self.assertRaises(Refused) as e:
                    read()
                self.assertEqual(e.exception.detail, "barrier_unavailable(protected_mode_unsupported)")
        self.w.to_p13()
        self.w.handoff()
        self.w.observe()
        cyc = self.w.cycles()[-1]
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D3")
        prot = pre.__class__(oc._CSEAL, **dict(pre._f, mode=oc.PROTECTED))
        ctx = oc.ContainmentContext(oc._CSEAL, pre=prot, basis="handoff", superseded=None, verdict=None)
        d = self.w.prov.decide(ctx, sel.public(), None)
        self.assertEqual((d.result, d.code, d.detail),
                         ("REFUSE", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(protected_mode_unsupported)"))
        self.assertFalse(self.w.prov.handoff_establish(prot, sel.public(), None).passed)
        self.assertFalse(self.w.prov.handoff_validate(prot, sel.public(), None).passed)
        view = types.SimpleNamespace(slot=SLOT, row="P13", f="REVOKED", current=cyc)
        self.assertIsNone(self.w.prov.discharge(view, o5.chain(self.w.env, cyc.barrier_id), prot, sel.public()))
        with self.assertRaisesRegex(ValueError, "protected_mode_unsupported"):
            self.w.prov.check_fresult(ctx, "snapshot", oc.FResult("SNAPSHOT_FAILED", "io", {}), cyc.cycle_id)

    def test_unlisted_cell_and_unlisted_result(self):
        self.w.to_p13()
        self.w.handoff()
        self.w.observe()
        pre, sel = freeze.basis_for(self.w.w, SLOT, "release", release_target=self.w.env.barrier_id, kind="D3")
        odd = pre.__class__(oc._CSEAL, **dict(pre._f, verb="adopt", row="P2"))
        ctx = oc.ContainmentContext(oc._CSEAL, pre=odd, basis="barrier", superseded=None, verdict=None)
        d = self.w.prov.decide(ctx, sel.public(), None)
        self.assertEqual((d.result, d.code, d.detail), ("REFUSE", Code.PROVENANCE_INSUFFICIENT, "no_decision_cell"))
        ctx2 = oc.ContainmentContext(oc._CSEAL, pre=pre, basis="barrier", superseded=None, verdict=None)
        with self.assertRaisesRegex(ValueError, "fresult_unlisted"):
            self.w.prov.check_fresult(ctx2, "snapshot", oc.FResult("REVOKED", "", {}), pre.cycle_id)

    def test_missing_required_input_refuses(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        real = freeze.derived_input

        def drop(world, facts, cycle, chain, kind, sup=None):
            return None if kind == "journal_head" else real(world, facts, cycle, chain, kind, sup)
        with mock.patch.object(freeze, "derived_input", drop):
            self.refused("quarantine", Code.QUIESCENCE_NOT_ESTABLISHED, "evidence_missing")
        pre_ctx = []
        real_decide = self.w.prov.decide

        def capture(ctx, selection, view):
            pre_ctx.append((ctx, selection))
            return real_decide(ctx, selection, view)
        self.w.prov.decide = capture
        self.ok("quarantine")
        ctx, selection = pre_ctx[0]
        sel = dict(selection, provider_inputs=[p for p in selection["provider_inputs"] if p[0] != "journal_head"])
        d = real_decide(ctx, sel, None)
        self.assertEqual((d.result, d.detail), ("REFUSE", "evidence_unavailable"))

    def test_doc_field_collisions_refuse_before_any_write(self):
        """A provider that adds a reserved or nested `o5` key is refused by PR-3a;
        the real provider's fields are exactly the §4.2 set."""
        self.w.to_verified()
        real = preserve.Preservation._D_PRES
        for extra in ({"o5": {"cell": "D-PRES"}}, {"slot": SLOT}, {"evidence": "x"}):
            with mock.patch.object(preserve.Preservation, "_D_PRES",
                                   lambda self, pre, i, s, extra=extra: dict(real(self, pre, i, s), **extra)):
                r = self.w.run("preservation")
            self.assertEqual(outcome(r)[:2], ("REFUSED", "FIELD_INVALID"), extra)
        self.ok("preservation")

    def test_invalid_decision_cell_is_refused_by_the_guard(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        real = self.w.prov.decide

        def wrong(ctx, selection, view):
            d = real(ctx, selection, view)
            return oc.PreservationDecision(oc._PSEAL, cell="D-QUIES", ctx=d.ctx, inputs=d.inputs, result=d.result,
                                           code=d.code, detail=d.detail, doc_fields={})
        self.w.prov.decide = wrong
        self.refused("quarantine", Code.STATE_UNKNOWN, "decision_incomplete")

    def test_no_production_write_path(self):
        """The provider and resolver hold no CommitTxn and expose no writer."""
        for obj in (self.w.prov, self.w.res):
            names = {n for n in dir(obj) if not n.startswith("__")}
            self.assertFalse(names & {"put", "append", "write", "txn", "ingest"}, names)


if __name__ == "__main__":
    unittest.main()
