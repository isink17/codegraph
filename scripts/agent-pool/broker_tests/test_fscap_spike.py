"""Post-v9 spike tests (RR9-1..RR9-5) for the FsCap authority, ported from the approved R2 model suite. Deterministic: Gate barriers and
lock-waiter counts only. """
import os, sys, threading, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fsharness as M  # noqa: E402
from test_fscap_fsmodel import (Base, attempt, spawn, done, named, XOBJ, RAW, SEALED, POISONED)  # noqa: E402


def blocked_thaw(test, disk):
    """A first owner thaw ending in a durable thaw FREEZE_BLOCKED at tail thaw.move.intent (RR9-1 setting)."""
    test.set(disk, slot=XOBJ)
    inv = M.start_invocation(disk, "thaw", M.A_THAW)
    test.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))
    return inv


# ---------------------------------------------------------------- RR9-1: the thaw pre-check is protected

class PrecheckRouting(Base):
    def test_RR9_1_precheck_C_poisons_then_original_returns_move_back_refused(self):
        """Reviewer counterexample through the model's real thaw pre-check entrypoint: tail
        thaw.move.intent, the pre-check sees C, the authority is poisoned, the original state returns,
        move_back refuses without observing, no rename, no fabricated thaw completion."""
        for new_owner_thaw in (False, True):
            with self.subTest(after_thaw_freeze_blocked=new_owner_thaw):
                self.fresh()
                disk = self.world()
                M.seed_thaw(disk)
                if new_owner_thaw:
                    blocked_thaw(self, disk)
                self.set(disk, slot=XOBJ)                       # 2. pre-check will observe C
                inv = M.start_invocation(disk, "thaw", M.A_THAW2 if new_owner_thaw else M.A_THAW)
                self.assertEqual(self.kinds(disk)[-1], "thaw.move.intent")   # 1. journal ends at the intent
                seen = {}

                def eff(store, fscap):
                    seen["pre"] = attempt(M.thaw_precheck, store, fscap)
                    seen["state"] = inv.authority.state
                    self.set(disk, slot=None)                   # 4. original expected state returns (A)
                    seen["obs"] = inv.authority.observations
                    seen["back"] = attempt(fscap.move_back)
                    seen["obs2"] = inv.authority.observations
                    return "NONE", "", {"journal": "d" * 64}    # the effect lies
                result = M.invoke(inv, eff)
                self.assertEqual(seen["pre"], ("Refused", "FREEZE_BLOCKED(thaw_target_occupied)"))
                self.assertEqual(seen["state"], M.FSCAP_POISONED)                       # 3.
                self.assertEqual(seen["back"], POISONED)                                # 5.
                self.assertEqual(seen["obs"], seen["obs2"])                             # 5. no further observation
                self.assertEqual(inv.authority.renames, 0)                              # 6.
                self.assertEqual((disk.fs()["slot"], disk.fs()["dst"]), (None, RAW))
                self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))   # 7. R1
                self.assertNotIn("thaw.move.done", self.kinds(disk))
                self.assertEqual(self.kinds(disk)[-1], "thaw.move.intent")
                self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")

    def test_RR9_1_trusted_thaw_effect_C(self):
        disk = self.world()
        M.seed_thaw(disk)
        inv = blocked_thaw(self, disk)
        self.assertEqual(inv.authority.record.poison, "thaw_target_occupied")
        self.assertEqual((inv.authority.renames, inv.authority.record.claimed), (0, False))

    def test_precheck_walk_object_identity_is_C(self):
        """D2.2.1 thaw matrix rows 1-7 below Q are identity decisions: C(identity), poison."""
        disk = self.world(objects="substituted")
        M.seed_thaw(disk)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.assertEqual((inv.authority.record.poison, inv.authority.renames), ("identity", 0))

    def test_precheck_location_observations_all_go_through_the_authority(self):
        disk = self.world()
        M.seed_thaw(disk)
        inv = M.start_invocation(disk, "thaw", M.A_THAW)
        self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "NONE", ""))
        log = inv.authority.record.log
        self.assertEqual([e for e in log if e[0] in ("obs", "content")][:5],
                         [("obs", "slot", "A"), ("obs", "quarantine", "A"), ("obs", "walk_q", "A"),
                          ("content", "originals", "ok"), ("content", "attrs", "ok")])
        self.assertEqual(inv.authority.renames, 1)


# ---------------------------------------------------------------- RR9-5: intent authentication

class IntentAuthentication(Base):
    def substituted(self):
        disk = self.world(slot=XOBJ)                            # a C is present at the slot name
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        return disk, inv

    def test_substituted_current_intent_refuses_without_observing_or_poisoning(self):
        disk, inv = self.substituted()
        seen = {}

        def eff(store, fscap):
            store.put("start")
            store.put("ep.done", binding=M.EARLY)
            store.put("move.intent", src_parent=M.SP, src_name=M.SN, dst_parent="4" * 64, dst_name=M.DN,
                      root=M.ROOT)                              # grammar-valid; names another directory
            seen["obs"] = attempt(fscap.observe, "slot")
            seen["state"] = inv.authority.state
            seen["move"] = attempt(fscap.move)
            return "FREEZE_BLOCKED", "identity_swap", {}
        result = M.invoke(inv, eff)
        self.assertEqual(seen["obs"], ("Refused", "intent authority"))
        self.assertEqual(seen["state"], M.FSCAP_OPEN)
        self.assertEqual(seen["move"], ("Unfinished", "intent authority"))
        self.assertIsNone(inv.authority.record.poison)
        self.assertEqual((inv.authority.observations, inv.authority.renames), (0, 0))
        self.assertEqual(result[0], "UNFINISHED")               # R3
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")

    def test_missing_intent_authentication_journal_rejected(self):
        disk = self.world(slot=XOBJ)
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            items = disk.world.o5.stores["journals"].items     # the durable intent no longer walks
            key = f"journal/{M.CID}/{2:08d}"                    # port delta: a store item, not a file
            rec = M.json.loads(items[key])
            rec["prev"] = "0" * 64
            items[key] = M.canonical.canon(rec)
            seen["obs"] = attempt(fscap.observe, "slot")
            seen["state"] = inv.authority.state
            return "FREEZE_BLOCKED", "identity_swap", {}
        result = M.invoke(inv, eff)
        self.assertEqual(seen["obs"], ("Refused", "journal_mismatch"))
        self.assertEqual(seen["state"], M.FSCAP_OPEN)
        self.assertEqual((inv.authority.observations, inv.authority.record.poison), (0, None))
        self.assertEqual(result[0], "UNFINISHED")

    def test_stale_journal_head_each_observation_rereads_the_tail(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        seen = {}

        def eff(store, fscap):
            store.put("start")
            store.put("ep.done", binding=M.EARLY)
            seen["before"] = attempt(fscap.observe, "slot")[1][0]   # tail ep.done: unprotected
            store.put("move.intent", src_parent=M.SP, src_name=M.SN, dst_parent=M.QD, dst_name=M.DN, root=M.ROOT)
            self.set(disk, slot=XOBJ)
            seen["after"] = attempt(fscap.observe, "slot")         # tail move.intent: protected, C
            return "MOVED", "", {"early": M.EARLY}
        result = M.invoke(inv, eff)
        self.assertEqual(seen["before"], "unprotected")
        self.assertEqual(seen["after"], ("Refused", "FREEZE_BLOCKED(identity_swap)"))
        self.assertEqual(result, ("COMMITTED", "FREEZE_BLOCKED", "identity_swap"))


# ---------------------------------------------------------------- RR9-2: the R6 decision table (rows A-H)

class R6Table(Base):
    def thaw(self, auth=M.A_THAW, **fs):
        disk = self.world()
        M.seed_thaw(disk)
        self.set(disk, **fs)
        return disk, M.start_invocation(disk, "thaw", auth)

    def test_A_content_read_failure_before_any_protected_observation(self):
        for lie in (False, True):
            with self.subTest(effect_returns_freeze_blocked=lie):
                self.fresh()
                disk, inv = self.thaw(content={"originals": "io"})
                seen = {}

                def eff(store, fscap):
                    try:
                        fscap.read_content("originals")         # EIO: the effect raises (R2) ...
                    except OSError:
                        if not lie:
                            raise
                    seen["state"] = inv.authority.state         # ... or catches it and claims a block
                    return "FREEZE_BLOCKED", "journal_mismatch", {}
                result = M.invoke(inv, eff)
                self.assertEqual(result[0], "UNFINISHED")
                self.assertIsNone(inv.authority.record.poison)
                if lie:
                    self.assertEqual(seen["state"], M.FSCAP_SEALED)     # v10 OD-S1/S6: content I/O seals, no verdict
                self.assertEqual((inv.authority.record.claimed, inv.authority.renames), (False, 0))
                self.assertEqual((M.fstate(disk)[0], self.kinds(disk)[-1]), ("THAW_INTENT", "thaw.move.intent"))
                self.assertEqual(M.inspect_durable(inv)["fwal"], "intent_pending")
                self.set(disk, content={})                         # the read succeeds later
                inv2 = M.start_invocation(disk, "resume-step")
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 1)

    def test_B_content_evidence_failure_after_protected_A(self):
        for what, reason in (("attrs", "journal_mismatch"), ("originals", "identity")):
            with self.subTest(what=what):
                self.fresh()
                disk, inv = self.thaw(content={what: reason})
                self.assertEqual(M.invoke(inv, M.effect_thaw), ("COMMITTED", "FREEZE_BLOCKED", reason))
                rec = inv.authority.record
                self.assertEqual((rec.poison, rec.claimed, rec.rename_issued), (None, False, False))
                self.assertEqual(M.fstate(disk)[0], "FREEZE_BLOCKED")
                self.assertEqual(self.kinds(disk)[-1], "thaw.move.intent")
                self.set(disk, content={})                         # owner repairs; new owner thaw
                inv2 = M.start_invocation(disk, "thaw", M.A_THAW2)
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 1)

    def test_B_content_read_without_prior_location_capability_observes_first(self):
        """v10 (OD-S1, OD-S7): the capability itself makes every location observation before a content verdict,
        so the caller's order cannot skip them; the veto then decides."""
        disk, inv = self.thaw(content={"attrs": "journal_mismatch"})

        def eff(store, fscap):
            return "FREEZE_BLOCKED", fscap.read_content("attrs"), {}
        self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "journal_mismatch"))
        self.assertEqual([e for e in inv.authority.record.log if e[0] in ("obs", "veto")],
                         [("obs", "slot", "A"), ("obs", "quarantine", "A"), ("obs", "walk_q", "A"),
                          ("veto", "journal_mismatch")])

    def test_B_C_is_never_downgraded_to_a_content_failure(self):
        for content_first in (False, True):
            with self.subTest(content_first=content_first):
                self.fresh()
                disk, inv = self.thaw(slot=XOBJ, content={"attrs": "journal_mismatch"})

                def eff(store, fscap):
                    if content_first:
                        fscap.read_content("attrs")
                        for w in M.LOCATION:
                            fscap.observe(w)
                    return M.effect_thaw(store, fscap)
                self.assertEqual(M.invoke(inv, eff), ("COMMITTED", "FREEZE_BLOCKED", "thaw_target_occupied"))

    def test_D_resume_case_A(self):
        disk, inv = self.thaw()

        def crash(store, fscap):
            raise RuntimeError("stopped before the move back")
        self.assertEqual(M.invoke(inv, crash)[0], "UNFINISHED")
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
        self.assertEqual(inv2.authority.renames, 1)

    def test_E_resume_case_B_physical_effect_complete(self):
        disk, inv = self.thaw()

        def crash_after_rename(store, fscap):
            self.set(disk, slot=RAW, dst=None)
            raise RuntimeError("renamed back, then stopped")
        self.assertEqual(M.invoke(inv, crash_after_rename)[0], "UNFINISHED")
        inv2 = M.start_invocation(disk, "resume-step")
        self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
        rec = inv2.authority.record
        self.assertEqual((rec.claimed, rec.rename_issued, rec.outcome), (True, False, ("completed",)))

    def test_E_freeze_start_row_b_FREEZE_BLOCKED_needs_the_claimed_call_first(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)

        def crash_after_rename(store, fscap):
            M.journal_through_move_intent(store)
            self.set(disk, dst=RAW, slot=None)
            raise RuntimeError("crash")
        self.assertEqual(M.invoke(inv, crash_after_rename)[0], "UNFINISHED")
        wrong = M.start_invocation(disk, "resume-step")          # EP checked first, no claimed call
        self.assertEqual(M.invoke(wrong, lambda s, f: ("FREEZE_BLOCKED", "identity", {}))[0], "UNFINISHED")
        self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")         # not permanent: still pending
        right = M.start_invocation(disk, "resume-step")

        def ordered(store, fscap):
            self.assertEqual(fscap.move(), "completed")             # (b) observed under the claimed call
            return "FREEZE_BLOCKED", "identity", {}                 # then the EP does not re-verify
        self.assertEqual(M.invoke(right, ordered), ("COMMITTED", "FREEZE_BLOCKED", "identity"))
        self.assertEqual(right.authority.renames, 0)

    def test_G_device_incompatibility_with_identities_verified(self):
        disk, inv = self.thaw(slot_parent={"key": M.SP, "dev": 2})
        result = M.invoke(inv, M.effect_thaw)
        self.assertEqual(result[0], "UNFINISHED")
        rec = inv.authority.record
        self.assertEqual((rec.poison, rec.renames, M.fstate(disk)[0]), (None, 0, "THAW_INTENT"))
        self.assertEqual(disk.fs()["dst"], RAW)                     # no copy, no delete
        inv2 = M.start_invocation(disk, "resume-step")              # a fresh resume stays possible
        self.assertEqual(M.invoke(inv2, M.effect_thaw)[0], "UNFINISHED")

    def test_H_failed_or_rejected_result(self):
        for fault in ("check_fresult", "relation"):
            with self.subTest(fault=fault):
                self.fresh()
                disk, inv = self.thaw()
                leak = {}

                def eff(store, fscap):
                    leak["fscap"] = fscap
                    out = M.effect_thaw(store, fscap)
                    if fault == "check_fresult":
                        M.FAULTS.add("check_fresult")
                        return out
                    return "NONE", "not_a_thaw_reason", out[2]
                result = M.invoke(inv, eff)
                M.FAULTS.clear()
                self.assertEqual(result[0], "UNFINISHED")
                self.assertEqual((M.inspect_durable(inv)["fwal"], M.fstate(disk)[0]), ("intent_pending", "THAW_INTENT"))
                self.assertEqual(attempt(leak["fscap"].move_back), SEALED)
                self.assertEqual(inv.authority.renames, 1)
                inv2 = M.start_invocation(disk, "resume-step")      # tail end{thawed}: writes nothing
                self.assertEqual(M.invoke(inv2, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(inv2.authority.renames, 0)


# ---------------------------------------------------------------- RR9-3: store failure seals in the same hold

class StoreFailure(Base):
    def test_store_failure_seals_atomically_no_later_rename(self):
        for point in ("journal:put:before", "journal:put:after"):
            with self.subTest(point=point):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)
                seen, out, gate = {}, {}, M.Gate()
                M.HOOKS["pre_acquire"] = named("D", gate.arrive)

                def eff(store, fscap):
                    M.journal_through_move_intent(store)
                    seen["move"] = fscap.move()                         # one rename, (b) observed
                    spawn(out, "D", fscap.view().move)                  # background call, not yet at the lock
                    M.await_(lambda: gate.arrived, "D started")
                    M.FAULTS.add(point)
                    try:
                        store.put("move.done", intent=M.h(disk.journal()[-1]), linked=[])
                    except OSError as e:
                        seen["err"] = e.strerror
                    M.FAULTS.clear()
                    seen["state"] = inv.authority.state
                    seen["record"] = inv.authority.record
                    seen["later"] = [attempt(fscap.move), attempt(fscap.observe, "slot"),
                                     attempt(store.put, "end", outcome="none", reason="identity")]
                    gate.release()
                    M.await_(lambda: done(out, "D"), "D done")
                    return "MOVED", "", {"early": M.EARLY, "journal": "d" * 64}
                result = M.invoke(inv, eff)
                self.assertEqual(seen["move"], "completed")
                self.assertIn("put", seen["err"])
                self.assertEqual(seen["state"], M.FSCAP_SEALED)
                self.assertIs(inv.authority.record, seen["record"])     # call-site seal: same record
                self.assertEqual(seen["record"].store_failure.split(":")[0], "move.done")
                self.assertEqual(seen["later"], [SEALED, SEALED, SEALED])
                self.assertEqual(out["D"], SEALED)
                self.assertEqual(inv.authority.renames, 1)
                self.assertEqual(result[0], "UNFINISHED")               # seq 0 exists
                self.assertEqual(M.fstate(disk)[0], "MOVE_INTENT")
                inv2 = M.start_invocation(disk, "resume-step")          # the re-read decides; no rename
                self.assertEqual(M.invoke(inv2, M.effect_resume_move), ("COMMITTED", "MOVED", ""))
                self.assertEqual(inv2.authority.renames, 0)

    def test_store_failure_waiting_caller_refused_after_seal(self):
        disk = self.world()
        inv = M.start_invocation(disk, "freeze-start", M.A_START)
        out, gate = {}, M.Gate()
        M.HOOKS["store_checked"] = named("P", gate.arrive)

        def eff(store, fscap):
            M.journal_through_move_intent(store)
            a = inv.authority
            M.FAULTS.add("journal:put:before")
            spawn(out, "P", store.put, "move.abort", intent=M.h(disk.journal()[-1]), reason="rename_failed")
            M.await_(lambda: gate.arrived, "P holds the lock")
            spawn(out, "B", fscap.view().move)
            M.await_(lambda: a.lock.waiting >= 1, "B waits for the lock")
            gate.release()
            M.await_(lambda: done(out, "P", "B"), "P, B done")
            M.FAULTS.clear()
            return "NONE", "rename_failed", {}
        result = M.invoke(inv, eff)
        self.assertEqual(out["P"][0], "error")                          # OSError propagates to its caller
        self.assertEqual(out["B"], SEALED)
        self.assertEqual((inv.authority.renames, inv.authority.observations), (0, 0))
        self.assertEqual(result[0], "UNFINISHED")
        self.unmoved(disk)

    def test_store_failure_before_seq0_permits_only_store_none(self):
        for res, want in ((("NONE", "store_error", {}), ("COMMITTED", "NONE", "store_error")),
                          (("MOVED", "", {"early": M.EARLY}), ("UNFINISHED",))):
            with self.subTest(res=res[:2]):
                self.fresh()
                disk = self.world()
                inv = M.start_invocation(disk, "freeze-start", M.A_START)

                def eff(store, fscap):
                    M.FAULTS.add("journal:put:before")
                    attempt_err = None
                    try:
                        store.put("start")
                    except OSError as e:
                        attempt_err = e
                    M.FAULTS.clear()
                    assert attempt_err is not None
                    return res
                self.assertEqual(M.invoke(inv, eff)[:len(want)], want)
                self.assertEqual(inv.authority.record.store_failure.split(":")[0], "start")


# ---------------------------------------------------------------- RR9-4: F WAL completion and `refreshed`

class Durability(Base):
    ROWS = (("completion_prepared", "UNFINISHED", "intent_pending", False, "THAW_INTENT"),
            ("fwal:append:before", "UNFINISHED", "intent_pending", False, "THAW_INTENT"),
            ("fwal:append:after", "UNFINISHED", "completion_durable", False, "FREEZE_BLOCKED"),
            (None, "COMMITTED", None, True, "FREEZE_BLOCKED"),
            ("barrier:put:before", "UNFINISHED", "completion_durable", False, "FREEZE_BLOCKED"),
            ("barrier:put:after", "UNFINISHED", "completion_durable", True, "FREEZE_BLOCKED"))

    def test_completion_and_refreshed_failure_matrix(self):
        for point, outcome, report, refreshed, f in self.ROWS:
            with self.subTest(point=point):
                self.fresh()
                disk = self.world()
                M.seed_thaw(disk)
                self.set(disk, slot=XOBJ)                             # C: thaw FREEZE_BLOCKED
                inv = M.start_invocation(disk, "thaw", M.A_THAW)

                def eff(store, fscap):
                    attempt(M.effect_thaw, store, fscap)
                    if point:
                        M.FAULTS.add(point)
                    return "NONE", "", {"journal": "d" * 64}
                result = M.invoke(inv, eff)
                M.FAULTS.clear()
                self.assertEqual(result[0], outcome)
                if report:
                    self.assertEqual(M.inspect_durable(inv)["fwal"], report)       # durable completion reported
                self.assertEqual(M.fstate(disk)[0], f)
                named_ = [r["completion"] for r in disk.barrier()]
                self.assertEqual(bool(named_), refreshed)
                self.assertTrue(M.refreshed_consistent(disk))         # a missing `refreshed` is consistent
                barrier_before = disk.barrier()
                self.set(disk, slot=None)
                if f == "THAW_INTENT":                                # intent pending: a resume re-evaluates
                    later = M.start_invocation(disk, "resume-step")
                else:                                                 # completion durable: nothing pending
                    with self.assertRaisesRegex(M.Refused, "nothing pending"):
                        M.start_invocation(disk, "resume-step")
                    later = M.start_invocation(disk, "thaw", M.A_THAW2)   # the permitted new owner thaw
                self.assertEqual(M.invoke(later, M.effect_thaw), ("COMMITTED", "NONE", ""))
                self.assertEqual(later.authority.renames, 1)
                self.assertEqual(disk.barrier()[:len(barrier_before)], barrier_before)
                self.assertEqual(len(disk.barrier()), len(barrier_before) + 1)   # only the new completion's


if __name__ == "__main__":
    unittest.main()
