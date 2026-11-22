"""Contract v10 tests (A1-A9, RS-1..RS-8, OD-S1..OD-S7) for the FsCap authority, ported from the approved R2
model suite. Deterministic: Gate
barriers and lock-waiter counts only, no sleeps, no real filesystem effects."""
import os, sys, threading, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fsharness as M  # noqa: E402
from test_fscap_fsmodel import Base, attempt, spawn, done, named, XOBJ, RAW, SEALED, POISONED  # noqa: E402


class V10(Base):
    def thaw(self, auth=M.A_THAW, **fs):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, **fs)
        return disk, M.start_invocation(disk, "thaw", auth)

    def crashed_after_move(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def crash_after_rename(store, fscap):
            M.journal_through_move_intent(store)
            self.set(disk, dst=RAW, slot=None)
            raise RuntimeError("renamed, then stopped")
        self.assertEqual(M.invoke(inv, crash_after_rename)[0], "UNFINISHED")
        return disk


# ---------------------------------------------------------------- OD-S6 / RS-2: protected observation I/O

class ObservationIO(V10):
    def test_precheck_observation_io_seals_no_rename_intent_pending(self):
        for key in ("slot", "quarantine", "walk_q"):
            with self.subTest(io=key):
                self.fresh()
                disk, inv = self.thaw(io=[key])
                seen = {}

                def eff(store, fscap):
                    seen["pre"] = attempt(M.thaw_precheck, store, fscap)
                    seen["state"] = inv.authority.state
                    self.set(disk, io=[])
                    seen["back"] = attempt(fscap.move_back)
                    return "NONE", "", {"journal": "d" * 64}
                result = M.invoke(inv, eff)
                self.assertEqual(seen["pre"], ("Unfinished", f"observation I/O ({key})"))
                self.assertEqual((seen["state"], seen["back"]), (M.FSCAP_SEALED, SEALED))
                rec = inv.authority.record
                self.assertEqual((rec.io, rec.poison, rec.claimed, inv.authority.renames), (key, None, False, 0))
                self.assertEqual(result[0], "UNFINISHED")
                self.assertEqual((M.fstate(disk)[0], self.kinds(disk)[-1]), ("THAW_INTENT", "thaw.move.intent"))
                inv2 = M.start_invocation(disk, "resume-step")          # separate authorized resume revalidates
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 1)

    def test_object_io_below_Q_is_not_case_C(self):
        """RS-2 (reviewer probe 4): an object syscall failure below Q is I/O, not identity."""
        disk, inv = self.thaw(objects="io")
        self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
        self.assertEqual((inv.authority.record.poison, inv.authority.record.io), (None, "walk_q"))
        self.assertEqual(M.fstate(disk)[0], "THAW_INTENT")

    def test_call_pre_rename_io_no_rename(self):
        disk = self.world(io=["call_pre"])
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        result = M.invoke(inv, M.effect_freeze_start)
        rec = inv.authority.record
        self.assertEqual((result[0], rec.outcome[0], rec.rename_issued, inv.authority.renames),
                         ("UNFINISHED", "unavailable", False, 0))
        self.unmoved(disk)

    def test_post_rename_io_is_an_uncertain_effect_not_pre_effect(self):
        disk = self.world(io=["call_post"])
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        result = M.invoke(inv, M.effect_freeze_start)
        rec = inv.authority.record
        self.assertEqual((result[0], rec.outcome, rec.rename_issued, inv.authority.renames),
                         ("UNFINISHED", ("uncertain", "observation I/O"), True, 1))
        self.assertEqual((M.fstate(disk)[0], self.kinds(disk)[-1]), ("MOVE_INTENT", "move.intent"))
        self.set(disk, io=[])
        inv2 = M.start_invocation(disk, "resume-step")                  # re-observes (b): no second rename
        self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
        self.assertEqual(inv2.authority.renames, 0)

    def test_proven_C_is_never_downgraded_to_io(self):
        disk, inv = self.thaw(slot=XOBJ)
        seen = {}

        def eff(store, fscap):
            seen["c"] = attempt(fscap.observe, "slot")
            self.set(disk, io=["walk_q"])
            seen["io"] = attempt(fscap.observe, "walk_q")
            return "NONE", "", {"journal": "d" * 64}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
        self.assertEqual((seen["c"][0], seen["io"]), ("Refused", POISONED))
        self.assertIsNone(inv.authority.record.io)


# ---------------------------------------------------------------- OD-S1, RS-4, RS-5, RS-1: content veto

class ContentVeto(V10):
    def test_negative_verdict_then_move_back_refused(self):
        """RS-5 (reviewer probe 5): a definite content veto terminates the authority before any move_back."""
        disk, inv = self.thaw(content={"attrs": "journal_mismatch"})
        seen = {}

        def eff(store, fscap):
            seen["pre"] = M.thaw_precheck(store, fscap)
            seen["back"] = attempt(fscap.move_back)
            seen["obs"] = attempt(fscap.observe, "slot")
            return "NONE", "", {"journal": "d" * 64}                   # buggy effect ignores the verdict
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
        self.assertEqual((seen["pre"], seen["back"], seen["obs"]), ("journal_mismatch", SEALED, SEALED))
        rec = inv.authority.record
        self.assertEqual((rec.veto, rec.poison, rec.claimed, inv.authority.renames), ("journal_mismatch", None, False, 0))
        self.assertEqual((disk.fs()["dst"], self.kinds(disk)[-1]), (RAW, "thaw.move.intent"))

    def test_forged_effect_verdict_clean_content_unfinished(self):
        """RS-1 R6-NOVERDICT: every location A, content clean, the effect claims a content FREEZE_BLOCKED."""
        disk, inv = self.thaw()
        seen = {}

        def eff(store, fscap):
            for w in M.LOCATION:
                fscap.observe(w)
            seen["reads"] = [fscap.read_content(w) for w in ("originals", "attrs")]
            return "FREEZE_BLOCKED", "journal_mismatch", {}
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.assertEqual(seen["reads"], ["ok", "ok"])
        self.assertEqual(M.fstate(disk)[0], "THAW_INTENT")

    def test_effect_cannot_choose_the_veto_reason(self):
        disk, inv = self.thaw(content={"attrs": "journal_mismatch"})

        def eff(store, fscap):
            M.thaw_precheck(store, fscap)
            return "FREEZE_BLOCKED", "identity", {}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))

    def test_verdict_bound_to_its_source(self):
        """RS-4 (reviewer probe 7): a fact that is not the source's own mismatch is no verdict."""
        for content in ({"originals": "journal_mismatch"}, {"attrs": "thaw_target_occupied"}, {"attrs": "identity"}):
            with self.subTest(content=content):
                self.fresh()
                disk, inv = self.thaw(content=content)
                self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
                rec = inv.authority.record
                self.assertEqual((rec.veto, rec.poison, inv.authority.renames), (None, None, 0))
                self.assertTrue(rec.io.startswith("content"))

    def test_content_read_refused_for_freeze_start(self):
        """RS-1 R6-ANYVERB: no content veto exists for freeze-start; R5 or R1 only."""
        disk = self.world(content={"originals": "identity"})
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            for w in M.LOCATION:
                fscap.observe(w)
            seen["read"] = attempt(fscap.read_content, "originals")
            return "FREEZE_BLOCKED", "identity", {}
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.assertEqual(seen["read"], ("Refused", "content read authority"))
        self.assertEqual((inv.authority.record.claimed, inv.authority.record.veto), (False, None))

    def test_content_read_needs_authenticated_intent(self):
        """The durable move.intent / thaw.move.intent pair names another quarantine parent: grammar-valid (the
        merged walker pins only the inverse), but it does not authenticate under the pinned layout."""
        disk = self.world(content={"attrs": "journal_mismatch"})
        pinned = M.QD
        M.QD = "4" * 64
        try:
            M.seed_thaw(disk)
        finally:
            M.QD = pinned
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        seen = {}

        def eff(store, fscap):
            seen["read"] = attempt(fscap.read_content, "attrs")
            seen["obs"] = attempt(fscap.observe, "slot")
            return "FREEZE_BLOCKED", "journal_mismatch", {}
        result = M.invoke(inv, eff)
        self.assertEqual((seen["read"], seen["obs"]), (("Refused", "content read authority"), ("Refused", "intent authority")))
        rec = inv.authority.record
        self.assertEqual((result[0], rec.veto, rec.poison, inv.authority.observations), ("UNFINISHED", None, None, 0))

    def test_content_read_after_poison_and_after_seal_refused(self):
        """RS-1 CONTENT-NOSTATE."""
        disk, inv = self.thaw(slot=XOBJ)
        seen, leak = {}, {}

        def eff(store, fscap):
            leak["f"] = fscap
            seen["c"] = attempt(fscap.observe, "slot")
            self.set(disk, slot=None)
            seen["read"] = attempt(fscap.read_content, "attrs")
            return "NONE", "", {"journal": "d" * 64}
        self.assertEqual(M.invoke(inv, eff)[1], "FREEZE_BLOCKED")
        self.assertEqual(seen["read"], POISONED)
        self.assertEqual(attempt(leak["f"].read_content, "attrs"), SEALED)
        self.assertEqual(inv.authority.observations, 1)

    def test_caller_order_cannot_mask_C_with_a_content_veto(self):
        """RS-1 R6-NOA: the capability establishes every location before computing a verdict."""
        disk, inv = self.thaw(slot=XOBJ, content={"attrs": "journal_mismatch"})

        def eff(store, fscap):
            fscap.read_content("attrs")
            return "FREEZE_BLOCKED", "journal_mismatch", {}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
        self.assertIsNone(inv.authority.record.veto)


# ---------------------------------------------------------------- RS-1 SEAL-OUTSIDE, A7: store failure seal

class StoreSeal(V10):
    def test_waiting_caller_cannot_run_between_failure_and_seal(self):
        """Deterministic version of the reviewer's probe 3: P fails inside LP-PUT while B waits on the lock."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        out, g_put, g_seal = {}, M.Gate(), M.Gate()
        M.HOOKS.update(store_checked=named("P", g_put.arrive), seal_enter=named("P", g_seal.arrive))

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            a = inv.authority
            M.FAULTS.add("journal:put:before")
            spawn(out, "P", store.put, "move.abort", intent=M.h(disk.journal()[-1]), reason="rename_failed")
            M.await_(lambda: g_put.arrived, "P holds the lock")
            spawn(out, "B", fscap.view().move)
            M.await_(lambda: a.lock.waiting >= 1, "B waits")
            g_put.release()
            M.await_(lambda: g_seal.arrived or done(out, "P"), "P sealed in its hold, or paused before a re-lock")
            M.FAULTS.clear()
            M.await_(lambda: done(out, "B"), "B done")
            g_seal.release()
            M.await_(lambda: done(out, "P"), "P done")
            return "NONE", "rename_failed", {}
        result = M.invoke(inv, eff)
        self.assertEqual(out["B"], SEALED)
        self.assertFalse(g_seal.arrived)                                # the failing put never re-took the lock
        self.assertEqual((inv.authority.renames, result[0]), (0, "UNFINISHED"))
        self.unmoved(disk)

    def test_store_read_failure_inside_put_seals(self):
        """A7: a store-machinery read failure inside LP-PUT seals like a write failure."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}
        M.HOOKS["store_checked"] = lambda: M.FAULTS.add("journal:read") if seen.get("arm") else None

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            seen["arm"] = True
            try:
                store.put("move.abort", intent="0" * 64, reason="rename_failed")
            except OSError as e:
                seen["err"] = e.strerror
            M.FAULTS.clear()
            seen["state"] = inv.authority.state
            seen["move"] = attempt(fscap.move)
            return "NONE", "rename_failed", {}
        result = M.invoke(inv, eff)
        self.assertEqual((seen["err"], seen["state"], seen["move"]), ("journal read failed", M.FSCAP_SEALED, SEALED))
        self.assertEqual((result[0], inv.authority.renames), ("UNFINISHED", 0))
        self.assertTrue(inv.authority.record.store_failure.startswith("move.abort"))

    def test_store_failure_after_seq0_binding_free_none_unfinished(self):
        """Reviewer's OWN-R2-SEQ0 (claimed equivalent): rejected by R2 and again by check_fresult."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def eff(store, fscap):
            store.put("start")
            M.FAULTS.add("journal:put:before")
            try:
                store.put("ep.done", binding=M.EARLY)
            except OSError:
                pass
            M.FAULTS.clear()
            return "NONE", "store_error", {}
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        with self.assertRaisesRegex(ValueError, "fresult_rejected"):
            M.check_fresult(inv, "NONE", "store_error", {})              # the second, independent layer


# ---------------------------------------------------------------- OD-S5 / RS-6: no stranded tree

class StrandedTree(V10):
    def test_move_done_then_false_FREEZE_BLOCKED_not_committed(self):
        """Reviewer's RS-6 sequence: (b) under the claimed call, move.done put, then FREEZE_BLOCKED(identity)."""
        disk = self.crashed_after_move()
        inv = M.start_invocation(disk, "resume-step")
        seen = {}

        def eff(store, fscap):
            seen["move"] = fscap.move()
            store.put("move.done", intent=M.h(disk.journal()[-1]), linked=[])
            return "FREEZE_BLOCKED", "identity", {}
        result = M.invoke(inv, eff)
        self.assertEqual(seen["move"], "completed")
        self.assertEqual(inv.authority.record.log[-1], ("put", "move.done"))
        self.assertEqual(result[0], "UNFINISHED")                       # not committed, not rewritten to MOVED
        self.assertEqual((M.fstate(disk)[0], self.kinds(disk)[-1]), ("MOVE_INTENT", "move.done"))
        self.assertEqual(disk.fs()["dst"], RAW)                         # no cleanup
        inv2 = M.start_invocation(disk, "resume-step")                  # durable evidence kept: legal path
        self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
        self.assertEqual(inv2.authority.renames, 0)

    def test_resume_at_move_done_false_FREEZE_BLOCKED_not_committed(self):
        """The same stranding through R6 at the unprotected tail move.done (no call)."""
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def moved_then_append_fails(store, fscap):
            out = M.effect_freeze_start(store, fscap)
            M.FAULTS.add("fwal:append:before")
            return out
        self.assertEqual(M.invoke(inv, moved_then_append_fails)[0], "UNFINISHED")
        M.FAULTS.clear()
        self.assertEqual((self.kinds(disk)[-1], M.fstate(disk)[0], disk.fs()["dst"]), ("move.done", "MOVE_INTENT", RAW))
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, lambda s, f: ("FREEZE_BLOCKED", "identity", {}))[0], "UNFINISHED")
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")
        inv3 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv3, M.effect_resume_move), ("COMMITTED", "MOVED", ""))

    def test_thaw_move_done_then_false_FREEZE_BLOCKED_not_committed(self):
        """Thaw analogue through R6 at the unprotected tail thaw.move.done (no call)."""
        disk, inv = self.thaw()
        seen = {}

        def eff(store, fscap):
            M.thaw_precheck(store, fscap)
            seen["back"] = fscap.move_back()
            store.put("thaw.move.done", intent=M.h(disk.journal()[-1]))
            raise RuntimeError("stopped before end{thawed}")
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.assertEqual((seen["back"], self.kinds(disk)[-1], disk.fs()["slot"]), ("completed", "thaw.move.done", RAW))
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, lambda s, f: ("FREEZE_BLOCKED", "identity", {}))[0], "UNFINISHED")
        self.assertEqual(M.fstate(disk)[0], "THAW_INTENT")
        inv3 = M.start_invocation(disk, "resume-step")                  # A9: tail thaw.move.done -> end{thawed}
        self.assertEqual(M.invoke(inv3, M.effect_thaw), ("COMMITTED", "NONE", ""))
        self.assertEqual(inv3.authority.renames, 0)


# ---------------------------------------------------------------- OD-S2: thaw case B

class ThawCaseB(V10):
    def test_B_idempotent_completion(self):
        disk, inv = self.thaw(slot=RAW, dst=None)
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "NONE", ""))
        rec = inv.authority.record
        self.assertEqual((rec.claimed, rec.rename_issued, inv.authority.renames), (True, False, 0))
        self.assertEqual(self.kinds(disk)[-2:], ["thaw.move.done", "end"])
        self.assertEqual(disk.journal()[-1]["outcome"], "thawed")

    def test_B_with_inconsistent_quarantine_or_identity_is_C(self):
        for fs, reason in (({"slot": RAW, "dst": None, "extra": ["x"]}, "identity"),
                           ({"slot": XOBJ, "dst": None}, "identity"),
                           ({"slot": {"key": M.ROOT, "dev": 3}, "dst": None}, "identity")):
            with self.subTest(fs=fs):
                self.fresh()
                disk, inv = self.thaw(**fs)
                self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", reason))
                self.assertEqual(inv.authority.renames, 0)

    def test_B_after_an_A_observation_without_a_rename_is_contradictory(self):
        disk, inv = self.thaw()
        seen = {}

        def eff(store, fscap):
            seen["first"] = fscap.observe("slot")[0]
            self.set(disk, slot=RAW, dst=None)                          # foreign rename between observations
            return M.effect_move_back(store, fscap)
        self.assertEqual(M.invoke(inv, eff)[0], "UNFINISHED")
        self.assertEqual(seen["first"], "A")
        self.assertEqual(inv.authority.record.outcome, ("completed",))
        self.assertEqual(inv.authority.renames, 0)
        self.assertEqual(M.fstate(disk)[0], "THAW_INTENT")


# ---------------------------------------------------------------- OD-S7: precedence when failures overlap

class Precedence(V10):
    def test_C_beats_device_mismatch_for_thaw(self):
        disk, inv = self.thaw(slot=XOBJ, slot_parent={"key": M.SP, "dev": 2})
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))

    def test_content_veto_precedes_device_mismatch(self):
        disk, inv = self.thaw(slot_parent={"key": M.SP, "dev": 2}, content={"attrs": "journal_mismatch"})
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
        self.assertEqual(inv.authority.renames, 0)

    def test_content_io_precedes_device_mismatch_and_later_content(self):
        disk, inv = self.thaw(slot_parent={"key": M.SP, "dev": 2},
                              content={"originals": "io", "attrs": "journal_mismatch"})
        self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
        rec = inv.authority.record
        self.assertEqual((rec.io, rec.veto, rec.claimed), ("content originals", None, False))

    def test_observation_io_before_an_unobserved_C_is_unfinished(self):
        disk, inv = self.thaw(io=["slot"], extra=["intruder"])
        self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
        self.assertEqual((inv.authority.record.poison, inv.authority.record.io), (None, "slot"))

    def test_device_mismatch_alone_unfinished_after_content(self):
        disk, inv = self.thaw(slot_parent={"key": M.SP, "dev": 2})
        self.assertEqual(M.invoke(inv, M.effect_thaw)[0], "UNFINISHED")
        rec = inv.authority.record
        self.assertEqual([e for e in rec.log if e[0] == "content"],
                         [("content", "originals", "ok"), ("content", "attrs", "ok")])
        self.assertEqual((rec.outcome, rec.poison, inv.authority.renames), (("unavailable", "clean device mismatch"), None, 0))

    def test_invoke_result_shape_is_merged(self):
        """OD-S3: invoke's result is unchanged; durability comes only from the separate inspection."""
        disk, inv = self.thaw(slot=XOBJ)

        def eff(store, fscap):
            M.FAULTS.add("barrier:put:before")
            return M.effect_thaw(store, fscap)
        result = M.invoke(inv, eff)
        M.FAULTS.clear()
        self.assertEqual(len(result), 2)
        self.assertEqual(result[0], "UNFINISHED")
        self.assertIsNone(getattr(inv, "after", None))
        self.assertEqual(M.inspect_durable(inv), {"fwal": "completion_durable", "refreshed": False})
        self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")


if __name__ == "__main__":
    unittest.main()
